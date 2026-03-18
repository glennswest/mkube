package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// NetworkType classifies the purpose of a network.
type NetworkType string

const (
	NetworkTypeData       NetworkType = "data"
	NetworkTypeIPMI       NetworkType = "ipmi"
	NetworkTypeManagement NetworkType = "management"
	NetworkTypeBoot       NetworkType = "boot"
	NetworkTypeStorage    NetworkType = "storage"
	NetworkTypeUser       NetworkType = "user"
	NetworkTypeExternal   NetworkType = "external"
)

// Network is a cluster-scoped CRD representing a layer-2/3 network.
type Network struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              NetworkSpec   `json:"spec"`
	Status            NetworkStatus `json:"status,omitempty"`
}

// NetworkSpec defines the desired state of a Network.
type NetworkSpec struct {
	Type          NetworkType       `json:"type"`                    // data, ipmi, management, boot, storage, external
	PairNetwork   string            `json:"pairNetwork,omitempty"`   // companion network (e.g. g12↔g13 data/ipmi pairing)
	Bridge        string            `json:"bridge,omitempty"`        // RouterOS bridge name
	CIDR          string            `json:"cidr"`                    // e.g. "192.168.10.0/24"
	Gateway       string            `json:"gateway"`                 // router IP on this network
	VLAN          int               `json:"vlan,omitempty"`
	Router        RouterRef         `json:"router,omitempty"`
	DNS           NetworkDNSSpec    `json:"dns,omitempty"`
	DHCP          NetworkDHCPSpec   `json:"dhcp,omitempty"`
	IPAM          NetworkIPAMSpec   `json:"ipam,omitempty"`
	ExternalDNS   bool              `json:"externalDNS,omitempty"`   // DNS not managed by mkube
	Managed       bool              `json:"managed,omitempty"`       // part 2: auto-deploy microdns
	Provisioned   bool              `json:"provisioned,omitempty"`   // infrastructure created by provider
	StaticRecords []StaticDNSRecord `json:"staticRecords,omitempty"`
}

// RouterRef identifies the router serving this network.
type RouterRef struct {
	Name string `json:"name,omitempty"` // e.g. "rose1"
	IP   string `json:"ip,omitempty"`   // e.g. "192.168.10.1"
}

// NetworkDNSSpec defines DNS settings for a network.
type NetworkDNSSpec struct {
	Endpoint string `json:"endpoint,omitempty"` // microdns REST URL
	Zone     string `json:"zone"`               // e.g. "g10.lo"
	Server   string `json:"server"`             // DNS server IP
}

// NetworkDHCPSpec defines DHCP settings for a network.
type NetworkDHCPSpec struct {
	Enabled       bool                    `json:"enabled"`
	RangeStart    string                  `json:"rangeStart,omitempty"`
	RangeEnd      string                  `json:"rangeEnd,omitempty"`
	LeaseTime     int                     `json:"leaseTime,omitempty"`
	NextServer    string                  `json:"nextServer,omitempty"`
	BootFile      string                  `json:"bootFile,omitempty"`
	BootFileEFI   string                  `json:"bootFileEfi,omitempty"`
	ServerNetwork string                  `json:"serverNetwork,omitempty"` // DHCP relay target
	Reservations  []NetworkDHCPReservation `json:"reservations,omitempty"`
}

// NetworkDHCPReservation is a static DHCP lease for a known MAC address.
type NetworkDHCPReservation struct {
	MAC         string   `json:"mac"`
	IP          string   `json:"ip"`
	Hostname    string   `json:"hostname,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`     // per-host gateway (defaults from network)
	DNSServers  []string `json:"dnsServers,omitempty"`  // per-host DNS servers (defaults from network)
	Domain      string   `json:"domain,omitempty"`      // per-host domain (defaults from network)
	NextServer  string   `json:"nextServer,omitempty"`  // per-host PXE next-server
	BootFile    string   `json:"bootFile,omitempty"`     // per-host PXE boot file (BIOS)
	BootFileEFI string   `json:"bootFileEfi,omitempty"` // per-host PXE boot file (UEFI)
	RootPath    string   `json:"rootPath,omitempty"`     // iSCSI root path (option 17)
}

// NetworkIPAMSpec defines IPAM allocation range for a network.
type NetworkIPAMSpec struct {
	Start string `json:"start,omitempty"` // first container IPAM IP
	End   string `json:"end,omitempty"`   // last container IPAM IP
}

// StaticDNSRecord is an infrastructure DNS record.
type StaticDNSRecord struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// NetworkStatus reports the observed state of a Network.
type NetworkStatus struct {
	Phase    string `json:"phase"`              // Active, Degraded, Error
	DNSAlive bool   `json:"dnsAlive,omitempty"`
	PodCount int    `json:"podCount,omitempty"`
}

// NetworkList is a list of Network objects.
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Network `json:"items"`
}

// DeepCopy returns a deep copy of the Network.
func (n *Network) DeepCopy() *Network {
	out := *n
	out.ObjectMeta = *n.ObjectMeta.DeepCopy()
	out.Spec.StaticRecords = append([]StaticDNSRecord(nil), n.Spec.StaticRecords...)
	out.Spec.DHCP.Reservations = append([]NetworkDHCPReservation(nil), n.Spec.DHCP.Reservations...)
	return &out
}

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadNetworksFromStore loads Network objects from the NATS NETWORKS bucket.
func (p *MicroKubeProvider) LoadNetworksFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Networks == nil {
		return
	}

	keys, err := p.deps.Store.Networks.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list networks from store", "error", err)
		return
	}

	for _, key := range keys {
		var net Network
		if _, err := p.deps.Store.Networks.GetJSON(ctx, key, &net); err != nil {
			p.deps.Logger.Warnw("failed to read network from store", "key", key, "error", err)
			continue
		}
		p.networks[net.Name] = &net

		// Register with IPAM if not already known (CRD-only networks like
		// g8/g9 are not in config.yaml and need dynamic registration)
		if p.deps.NetworkMgr != nil {
			if err := p.deps.NetworkMgr.RegisterNetwork(networkToNetworkDef(&net)); err != nil {
				p.deps.Logger.Warnw("failed to register network with IPAM on load", "network", net.Name, "error", err)
			}
		}
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded networks from store", "count", len(keys))
	}

	// Ensure managed networks have their DNS pods deployed.
	// On restart, the pod store entry may have been lost (e.g. if the pod
	// was not tracked in p.pods when teardown ran, or if NATS lost the key).
	for _, net := range p.networks {
		if !net.Spec.Managed || net.Spec.DNS.Zone == "" || net.Spec.DNS.Server == "" {
			continue
		}
		podKey := net.Name + "/dns"
		if _, ok := p.pods[podKey]; ok {
			continue // already tracked
		}
		// Check NATS pod store
		if p.deps.Store != nil && p.deps.Store.Pods != nil {
			storeKey := net.Name + ".dns"
			var existing corev1.Pod
			if _, err := p.deps.Store.Pods.GetJSON(ctx, storeKey, &existing); err == nil {
				continue // exists in store, reconciler will pick it up
			}
		}
		p.deps.Logger.Infow("managed network missing DNS pod, deploying", "network", net.Name)
		if err := p.deployManagedDNS(ctx, net); err != nil {
			p.deps.Logger.Warnw("failed to deploy managed DNS on load", "network", net.Name, "error", err)
		}
	}
}

// reconcileManagedDNSPods checks all managed networks and recreates DNS pods
// that are missing (e.g. deleted manually or lost during restart).
func (p *MicroKubeProvider) reconcileManagedDNSPods(ctx context.Context) {
	for _, net := range p.networks {
		if !net.Spec.Managed || net.Spec.ExternalDNS || net.Spec.DNS.Zone == "" {
			continue
		}
		podMapKey := net.Name + "/dns"
		if _, ok := p.pods[podMapKey]; ok {
			continue // pod tracked in memory
		}
		p.deps.Logger.Infow("managed network missing DNS pod, recreating", "network", net.Name)
		if err := p.deployManagedDNS(ctx, net); err != nil {
			p.deps.Logger.Warnw("failed to recreate managed DNS pod", "network", net.Name, "error", err)
		}
	}
}

// ReconcileNetworkConfigMaps regenerates DNS ConfigMaps from the current
// Network CRD state. Call after loading both networks and configmaps from
// store to fix stale ConfigMaps that diverged (e.g. reservations changed
// while mkube was down or before ConfigMap sync code existed).
func (p *MicroKubeProvider) ReconcileNetworkConfigMaps(ctx context.Context) {
	updated := 0
	for _, net := range p.networks {
		hasDNS := net.Spec.DNS.Zone != "" && net.Spec.DNS.Server != ""
		if !hasDNS {
			continue
		}
		toml := p.generateMinimalTOML(net)
		cmKey := net.Name + "/dns-config"
		cm, ok := p.configMaps[cmKey]
		if !ok {
			continue
		}
		if cm.Data["microdns.toml"] != toml {
			cm.Data["microdns.toml"] = toml
			if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
				storeKey := net.Name + ".dns-config"
				_, _ = p.deps.Store.ConfigMaps.PutJSON(ctx, storeKey, cm)
			}
			updated++
			p.deps.Logger.Infow("reconciled stale DNS ConfigMap on boot",
				"network", net.Name)
		}
	}
	if updated > 0 {
		p.syncConfigMapsToDisk(ctx)
		p.deps.Logger.Infow("reconciled DNS ConfigMaps on boot", "updated", updated)
	}
}

// MigrateNetworkConfig migrates config.yaml NetworkDef entries to Network CRDs
// on first boot (when the NETWORKS bucket is empty).
func (p *MicroKubeProvider) MigrateNetworkConfig(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Networks == nil {
		return
	}

	empty, err := p.deps.Store.Networks.IsEmpty(ctx)
	if err != nil {
		p.deps.Logger.Warnw("failed to check networks store", "error", err)
		return
	}
	if !empty {
		p.deps.Logger.Debugw("networks store already populated, skipping migration")
		return
	}

	if len(p.deps.Config.Networks) == 0 {
		return
	}

	p.deps.Logger.Infow("migrating config.yaml networks to Network CRDs",
		"count", len(p.deps.Config.Networks))

	for _, nd := range p.deps.Config.Networks {
		net := networkDefToNetwork(nd)
		if _, err := p.deps.Store.Networks.PutJSON(ctx, net.Name, &net); err != nil {
			p.deps.Logger.Warnw("failed to migrate network", "name", nd.Name, "error", err)
			continue
		}
		p.networks[net.Name] = &net
		p.deps.Logger.Infow("migrated network", "name", net.Name, "type", net.Spec.Type)
	}
}

// networkDefToNetwork converts a config.NetworkDef to a Network CRD.
func networkDefToNetwork(nd config.NetworkDef) Network {
	netType := NetworkTypeData
	if nd.Type != "" {
		netType = NetworkType(nd.Type)
	} else if nd.ExternalDNS {
		netType = NetworkTypeExternal
	}

	var reservations []NetworkDHCPReservation
	for _, r := range nd.DNS.DHCP.Reservations {
		reservations = append(reservations, NetworkDHCPReservation{
			MAC:      r.MAC,
			IP:       r.IP,
			Hostname: r.Hostname,
		})
	}

	var staticRecords []StaticDNSRecord
	for _, r := range nd.DNS.StaticRecords {
		staticRecords = append(staticRecords, StaticDNSRecord{
			Name: r.Name,
			IP:   r.IP,
		})
	}

	return Network{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Network"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              nd.Name,
			CreationTimestamp: metav1.Now(),
		},
		Spec: NetworkSpec{
			Type:    netType,
			Bridge:  nd.Bridge,
			CIDR:    nd.CIDR,
			Gateway: nd.Gateway,
			VLAN:    nd.VLAN,
			DNS: NetworkDNSSpec{
				Endpoint: nd.DNS.Endpoint,
				Zone:     nd.DNS.Zone,
				Server:   nd.DNS.Server,
			},
			DHCP: NetworkDHCPSpec{
				Enabled:       nd.DNS.DHCP.Enabled,
				RangeStart:    nd.DNS.DHCP.RangeStart,
				RangeEnd:      nd.DNS.DHCP.RangeEnd,
				LeaseTime:     nd.DNS.DHCP.LeaseTime,
				NextServer:    nd.DNS.DHCP.NextServer,
				BootFile:      nd.DNS.DHCP.BootFile,
				BootFileEFI:   nd.DNS.DHCP.BootFileEFI,
				ServerNetwork: nd.DNS.DHCP.ServerNetwork,
				Reservations:  reservations,
			},
			IPAM: NetworkIPAMSpec{
				Start: nd.IPAMStart,
				End:   nd.IPAMEnd,
			},
			ExternalDNS:   nd.ExternalDNS,
			Managed:       !nd.ExternalDNS && nd.DNS.Zone != "" && nd.DNS.Server != "",
			StaticRecords: staticRecords,
		},
		Status: NetworkStatus{
			Phase: "Active",
		},
	}
}

// networkToNetworkDef converts a Network CRD back to a config.NetworkDef,
// used for dynamic IPAM registration when networks are created via API.
func networkToNetworkDef(n *Network) config.NetworkDef {
	return config.NetworkDef{
		Name:        n.Name,
		Type:        string(n.Spec.Type),
		Bridge:      n.Spec.Bridge,
		CIDR:        n.Spec.CIDR,
		Gateway:     n.Spec.Gateway,
		VLAN:        n.Spec.VLAN,
		IPAMStart:   n.Spec.IPAM.Start,
		IPAMEnd:     n.Spec.IPAM.End,
		ExternalDNS: n.Spec.ExternalDNS,
		DNS: config.DNSConfig{
			Endpoint: n.Spec.DNS.Endpoint,
			Zone:     n.Spec.DNS.Zone,
			Server:   n.Spec.DNS.Server,
		},
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListNetworks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchNetworks(w, r)
		return
	}

	items := make([]Network, 0, len(p.networks))
	for _, net := range p.networks {
		enriched := net.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
		p.enrichNetworkStatus(r.Context(), enriched)
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, networkListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, NetworkList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NetworkList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	net, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

	enriched := net.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
	p.enrichNetworkStatus(r.Context(), enriched)

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, networkListToTable([]Network{*enriched}))
		return
	}

	podWriteJSON(w, http.StatusOK, enriched)
}

func (p *MicroKubeProvider) handleCreateNetwork(w http.ResponseWriter, r *http.Request) {
	var net Network
	if err := json.NewDecoder(r.Body).Decode(&net); err != nil {
		http.Error(w, fmt.Sprintf("invalid Network JSON: %v", err), http.StatusBadRequest)
		return
	}

	if net.Name == "" {
		http.Error(w, "network name is required", http.StatusBadRequest)
		return
	}
	if net.Spec.CIDR == "" {
		http.Error(w, "network CIDR is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.networks[net.Name]; exists {
		http.Error(w, fmt.Sprintf("network %q already exists", net.Name), http.StatusConflict)
		return
	}

	net.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
	if net.CreationTimestamp.IsZero() {
		net.CreationTimestamp = metav1.Now()
	}
	if net.Status.Phase == "" {
		net.Status.Phase = "Active"
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(r.Context(), net.Name, &net); err != nil {
			http.Error(w, fmt.Sprintf("persisting network: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Set bridge name before registration so the network manager has it
	if net.Spec.Bridge == "" {
		net.Spec.Bridge = "bridge-" + net.Name
	}

	p.networks[net.Name] = &net

	// Register with IPAM so pods on this network can allocate IPs
	if err := p.deps.NetworkMgr.RegisterNetwork(networkToNetworkDef(&net)); err != nil {
		p.deps.Logger.Warnw("failed to register network with IPAM", "network", net.Name, "error", err)
	}

	// Provision infrastructure (bridge, gateway IP, DHCP relay) synchronously
	// before deploying DNS, so the bridge exists when the DNS pod is created.
	if !net.Spec.Provisioned {
		p.provisionNetwork(r.Context(), &net)
	}

	// Auto-deploy managed DNS pod if network is managed with DNS config
	if net.Spec.Managed && net.Spec.DNS.Zone != "" && net.Spec.DNS.Server != "" {
		if err := p.deployManagedDNS(r.Context(), &net); err != nil {
			p.deps.Logger.Warnw("auto-deploy DNS failed", "network", net.Name, "error", err)
		}
	}

	// Rebuild DHCP index to include the new network
	p.rebuildDHCPIndex()

	podWriteJSON(w, http.StatusCreated, &net)
}

func (p *MicroKubeProvider) handleUpdateNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	old, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}
	wasManaged := old.Spec.Managed

	var net Network
	if err := json.NewDecoder(r.Body).Decode(&net); err != nil {
		http.Error(w, fmt.Sprintf("invalid Network JSON: %v", err), http.StatusBadRequest)
		return
	}
	net.Name = name
	net.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}

	// Preserve creation timestamp from existing
	if net.CreationTimestamp.IsZero() {
		net.CreationTimestamp = old.CreationTimestamp
	}

	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(r.Context(), name, &net); err != nil {
			http.Error(w, fmt.Sprintf("persisting network update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.networks[name] = &net

	// Handle managed DNS transitions
	p.handleManagedDNSTransition(r.Context(), wasManaged, &net)

	// Rebuild DHCP index on reservation changes
	p.rebuildDHCPIndex()
	p.triggerNetworkReseed(name)
	p.triggerReconcile()

	podWriteJSON(w, http.StatusOK, &net)
}

func (p *MicroKubeProvider) handlePatchNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}
	wasManaged := existing.Spec.Managed

	// Start from existing, overlay the patch
	merged := existing.DeepCopy()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading body: %v", err), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}
	merged.Name = name
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
	merged.CreationTimestamp = existing.CreationTimestamp

	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(r.Context(), name, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting network patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.networks[name] = merged

	// Handle managed DNS transitions
	p.handleManagedDNSTransition(r.Context(), wasManaged, merged)

	// Rebuild DHCP index on reservation changes
	p.rebuildDHCPIndex()
	p.triggerNetworkReseed(name)
	p.triggerReconcile()

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	net, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

	// Teardown auto-deployed DNS pod before the pod-reference check
	if net.Spec.Managed {
		if err := p.teardownManagedDNS(r.Context(), name); err != nil {
			p.deps.Logger.Warnw("teardown managed DNS failed", "network", name, "error", err)
		}
	}

	// Deprovision infrastructure (DHCP relay, bridge IP, bridge)
	if net.Spec.Provisioned {
		p.deprovisionNetwork(r.Context(), net)
	}

	// Check if any (non-managed) pod still references this network
	for _, pod := range p.pods {
		if pod.Annotations[annotationNetwork] == name {
			http.Error(w, fmt.Sprintf("cannot delete network %q: pod %s/%s references it",
				name, pod.Namespace, pod.Name), http.StatusConflict)
			return
		}
	}

	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if err := p.deps.Store.Networks.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting network from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.networks, name)

	// Unregister from IPAM and network manager
	if p.deps.NetworkMgr != nil {
		p.deps.NetworkMgr.UnregisterNetwork(name)
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("network %q deleted", name),
	})
}

// ─── Config Generation ──────────────────────────────────────────────────────

// handleGetNetworkConfig generates a microdns TOML config for a network.
func (p *MicroKubeProvider) handleGetNetworkConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	net, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

	toml := p.generateMinimalTOML(net)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(toml))
}

// generateMinimalTOML produces a minimal microdns TOML config from a Network CRD.
// It contains only structural config (instance, DNS auth/recursor, API, database,
// logging). DHCP pools, reservations, and forward zones are seeded via REST API
// by seedDNSConfig() — not baked into TOML.
func (p *MicroKubeProvider) generateMinimalTOML(net *Network) string {
	// RouterOS containers use "gateway" mode (DHCP via relay, no raw sockets).
	dnsMode := "standalone"
	if p.deps.Config.Backend == "" || p.deps.Config.Backend == "routeros" {
		dnsMode = "gateway"
	}

	// DHCP enabled section: just the interface/server_ip/listen_ports
	// so microdns knows to start the DHCP listener. Pools and reservations
	// come via REST API.
	var dhcpSection string
	hasDHCP := net.Spec.DHCP.Enabled && net.Spec.DHCP.ServerNetwork == ""
	if !hasDHCP {
		// Check if any peer relays to this network
		for _, peer := range p.networks {
			if peer.Name != net.Name && peer.Spec.DHCP.Enabled && peer.Spec.DHCP.ServerNetwork == net.Name {
				hasDHCP = true
				break
			}
		}
	}
	if hasDHCP {
		// Build reverse zone from CIDR for DNS registration
		reverseZone := ""
		if cidrParts := strings.Split(net.Spec.CIDR, "/"); len(cidrParts) == 2 {
			octets := strings.Split(cidrParts[0], ".")
			if len(octets) == 4 {
				reverseZone = fmt.Sprintf("%s.%s.%s.in-addr.arpa", octets[2], octets[1], octets[0])
			}
		}
		dhcpSection = fmt.Sprintf(`
[dhcp.v4]
enabled = true
interface = "eth0"
server_ip = %q
listen_ports = [67]

[dhcp.dns_registration]
enabled = true
forward_zone = %q
reverse_zone_v4 = %q
reverse_zone_v6 = ""
default_ttl = 300
`, net.Spec.DNS.Server, net.Spec.DNS.Zone, reverseZone)
	}

	// Build NATS messaging section
	natsURL := p.natsURL()
	var messagingSection string
	if natsURL != "" {
		messagingSection = fmt.Sprintf(`
[messaging]
backend = "nats"
topic_prefix = "microdns"
url = %q
`, natsURL)
	}

	return fmt.Sprintf(`[instance]
id = "microdns-%s"
mode = "%s"

[dns.auth]
enabled = true
listen = "0.0.0.0:15353"
zones = ["%s"]

[dns.recursor]
enabled = true
listen = "0.0.0.0:53"

[dns.recursor.forward_zones]

[api.rest]
enabled = true
listen = "0.0.0.0:8080"

[database]
path = "/data/microdns.redb"

[logging]
level = "info"
format = "text"
%s%s`, net.Name, dnsMode, net.Spec.DNS.Zone, dhcpSection, messagingSection)
}

// ─── Status Enrichment ──────────────────────────────────────────────────────

// enrichNetworkStatus computes live status: pod count and DNS liveness.
func (p *MicroKubeProvider) enrichNetworkStatus(ctx context.Context, net *Network) {
	// Count pods on this network
	count := 0
	for _, pod := range p.pods {
		if pod.Annotations[annotationNetwork] == net.Name {
			count++
		}
	}
	net.Status.PodCount = count

	// DNS liveness check
	if net.Spec.DNS.Server != "" && net.Spec.DNS.Zone != "" && !net.Spec.ExternalDNS {
		net.Status.DNSAlive = probeDNSPort(net.Spec.DNS.Server, net.Spec.DNS.Zone, 3*time.Second)
	}
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchNetworks(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.Networks == nil {
		http.Error(w, "watch requires NATS store", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Send existing Network objects as ADDED events (snapshot under read lock)
	enc := json.NewEncoder(w)
	p.mu.RLock()
	netSnapshot := make([]*Network, 0, len(p.networks))
	for _, net := range p.networks {
		netSnapshot = append(netSnapshot, net.DeepCopy())
	}
	p.mu.RUnlock()
	for _, enriched := range netSnapshot {
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
		p.enrichNetworkStatus(ctx, enriched)
		evt := K8sWatchEvent{Type: "ADDED", Object: enriched}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.Networks.WatchAll(ctx)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			var net Network
			if evt.Type == store.EventDelete {
				net = Network{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Network"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &net); err != nil {
					continue
				}
				net.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}
				p.enrichNetworkStatus(ctx, &net)
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &net,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

// networkListToTable converts a NetworkList to a Table response for oc/kubectl.
func networkListToTable(networks []Network) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Type", Type: "string"},
			{Name: "Pair", Type: "string"},
			{Name: "CIDR", Type: "string"},
			{Name: "Gateway", Type: "string"},
			{Name: "DNS Zone", Type: "string"},
			{Name: "DNS Server", Type: "string"},
			{Name: "DHCP", Type: "string"},
			{Name: "Managed", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range networks {
		net := &networks[i]

		dhcp := "false"
		if net.Spec.DHCP.Enabled {
			dhcp = "true"
		}
		managed := "false"
		if net.Spec.Managed {
			managed = "true"
		}

		age := "<unknown>"
		if !net.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(net.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              net.Name,
				"creationTimestamp": net.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				net.Name,
				string(net.Spec.Type),
				net.Spec.PairNetwork,
				net.Spec.CIDR,
				net.Spec.Gateway,
				net.Spec.DNS.Zone,
				net.Spec.DNS.Server,
				dhcp,
				managed,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Managed DNS Auto-Deploy ─────────────────────────────────────────────────

// deployManagedDNS creates a ConfigMap and DNS pod for a managed network.
// This makes Network CRD creation a one-stop operation: create the network
// and DNS "just works" without manual boot-order or oc apply.
func (p *MicroKubeProvider) deployManagedDNS(ctx context.Context, net *Network) error {
	log := p.deps.Logger.With("network", net.Name)

	if net.Spec.DNS.Zone == "" || net.Spec.DNS.Server == "" {
		return fmt.Errorf("DNS zone and server are required for managed DNS")
	}

	// 1. Generate and persist ConfigMap
	toml := p.generateMinimalTOML(net)
	cm := corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "dns-config", Namespace: net.Name},
		Data:       map[string]string{"microdns.toml": toml},
	}

	cmKey := net.Name + "/dns-config"
	p.configMaps[cmKey] = &cm

	if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
		storeKey := net.Name + ".dns-config"
		if _, err := p.deps.Store.ConfigMaps.PutJSON(ctx, storeKey, &cm); err != nil {
			return fmt.Errorf("persisting dns-config configmap: %w", err)
		}
	}

	p.syncConfigMapsToDisk(ctx)
	log.Infow("created dns-config ConfigMap", "zone", net.Spec.DNS.Zone)

	// 2. Build DNS pod matching boot-order template
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dns",
			Namespace: net.Name,
			Annotations: map[string]string{
				annotationNetwork:     net.Name,
				"vkube.io/boot-priority": "10",
				annotationStaticIP:    net.Spec.DNS.Server,
				annotationImagePolicy: "auto",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "dns-config"},
						},
					},
				},
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: net.Name + "-dns-data",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "microdns",
					Image: "192.168.200.3:5000/microdns:edge",
					StartupProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/api/v1/zones",
								Port: intstr.FromInt32(8080),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       3,
						FailureThreshold:    10,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/api/v1/zones",
								Port: intstr.FromInt32(8080),
							},
						},
						PeriodSeconds:    30,
						FailureThreshold: 3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.FromInt32(53),
							},
						},
						PeriodSeconds: 10,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/etc/microdns"},
						{Name: "data", MountPath: "/data"},
					},
				},
			},
		},
	}

	// 3. Ensure PVC exists for DNS data persistence
	pvcName := net.Name + "-dns-data"
	pvcMapKey := net.Name + "/" + pvcName
	if _, exists := p.pvcs[pvcMapKey]; !exists {
		storageClass := "local"
		pvc := &corev1.PersistentVolumeClaim{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
			ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: net.Name},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &storageClass,
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		}
		p.pvcs[pvcMapKey] = pvc
		storeKey := net.Name + "." + pvcName
		if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
			if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
				log.Warnw("failed to persist DNS data PVC", "error", err)
			}
		}
		log.Infow("created DNS data PVC", "pvc", pvcName, "namespace", net.Name)
	}

	// 4. Persist pod to NATS
	if p.deps.Store != nil && p.deps.Store.Pods != nil {
		storeKey := net.Name + ".dns"
		if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, &pod); err != nil {
			return fmt.Errorf("persisting dns pod: %w", err)
		}
	}

	// 6. Create the pod (goes through ContainerRuntime interface)
	if err := p.CreatePod(ctx, &pod); err != nil {
		return fmt.Errorf("creating dns pod: %w", err)
	}

	// 7. Set DNS endpoint on network so REST API calls work
	net.Spec.DNS.Endpoint = "http://" + net.Spec.DNS.Server + ":8080"
	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(ctx, net.Name, net); err != nil {
			log.Warnw("failed to persist DNS endpoint update", "error", err)
		}
	}

	// 6. Async seed DHCP pools, reservations, and forwarders via REST API.
	// Uses the reseedRunning atomic guard to prevent unbounded goroutine growth.
	p.triggerNetworkReseed(net.Name)

	log.Infow("deployed managed DNS pod",
		"zone", net.Spec.DNS.Zone,
		"server", net.Spec.DNS.Server,
		"pod", net.Name+"/dns")
	return nil
}

// teardownManagedDNS removes the auto-deployed DNS pod and ConfigMap for a network.
func (p *MicroKubeProvider) teardownManagedDNS(ctx context.Context, netName string) error {
	log := p.deps.Logger.With("network", netName)

	// Remove DNS pod
	podMapKey := netName + "/dns"
	if pod, ok := p.pods[podMapKey]; ok {
		if err := p.DeletePod(ctx, pod); err != nil {
			log.Warnw("failed to delete managed DNS pod", "error", err)
		} else {
			log.Infow("deleted managed DNS pod")
		}
	} else {
		// Pod not tracked (e.g. CreatePod failed or was loaded from NATS
		// during boot but never reconciled into p.pods). Directly clean up
		// the container and veth that may still exist on the device.
		containerName := netName + "_dns_microdns"
		vethName := "veth_" + netName + "_dns_0"
		log.Infow("DNS pod not tracked, cleaning up container/veth directly",
			"container", containerName, "veth", vethName)
		p.stopAndRemoveContainer(ctx, containerName, "")
		_ = p.deps.Runtime.RemoveMountsByList(ctx, containerName)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); err != nil {
			log.Warnw("failed to release veth during direct cleanup", "veth", vethName, "error", err)
		}
	}

	// Remove from NATS pod store regardless of tracking state
	if p.deps.Store != nil && p.deps.Store.Pods != nil {
		storeKey := netName + ".dns"
		if err := p.deps.Store.Pods.Delete(ctx, storeKey); err != nil {
			log.Warnw("failed to delete dns pod from store", "error", err)
		}
	}

	// Remove DNS ConfigMap
	cmKey := netName + "/dns-config"
	if _, ok := p.configMaps[cmKey]; ok {
		delete(p.configMaps, cmKey)
		if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
			storeKey := netName + ".dns-config"
			if err := p.deps.Store.ConfigMaps.Delete(ctx, storeKey); err != nil {
				log.Warnw("failed to delete dns-config from store", "error", err)
			}
		}
		log.Infow("deleted managed DNS ConfigMap")
	}

	return nil
}

// handleManagedDNSTransition handles managed flag changes on update/patch.
func (p *MicroKubeProvider) handleManagedDNSTransition(ctx context.Context, wasManaged bool, net *Network) {
	nowManaged := net.Spec.Managed
	hasDNS := net.Spec.DNS.Zone != "" && net.Spec.DNS.Server != ""

	switch {
	case !wasManaged && nowManaged && hasDNS:
		// false→true: deploy DNS
		if err := p.deployManagedDNS(ctx, net); err != nil {
			p.deps.Logger.Warnw("auto-deploy DNS on managed transition failed",
				"network", net.Name, "error", err)
		}
	case wasManaged && !nowManaged:
		// true→false: teardown DNS
		if err := p.teardownManagedDNS(ctx, net.Name); err != nil {
			p.deps.Logger.Warnw("teardown DNS on unmanaged transition failed",
				"network", net.Name, "error", err)
		}
	}

	// Push reservation and forwarder changes via REST API (no TOML rewrite needed).
	// ConfigMap only updated if structural config changed.
	if hasDNS {
		endpoint := p.networkDNSEndpoint(net)
		if endpoint != "" {
			// Sync reservations: upsert all from Network CRD
			dnsClient := p.deps.NetworkMgr.DNSClient()
			if err := dnsClient.HealthCheck(ctx, endpoint); err == nil {
				for _, r := range net.Spec.DHCP.Reservations {
					res := networkReservationToDNS(r)
					if err := dnsClient.UpsertDHCPReservation(ctx, endpoint, res); err != nil {
						p.deps.Logger.Warnw("failed to sync reservation on network update",
							"network", net.Name, "mac", r.MAC, "error", err)
					}
				}
			}
		}

		// Update ConfigMap only if structural TOML changed
		toml := p.generateMinimalTOML(net)
		cmKey := net.Name + "/dns-config"
		if cm, ok := p.configMaps[cmKey]; ok {
			if cm.Data["microdns.toml"] != toml {
				cm.Data["microdns.toml"] = toml
				if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
					storeKey := net.Name + ".dns-config"
					_, _ = p.deps.Store.ConfigMaps.PutJSON(ctx, storeKey, cm)
				}
				p.syncConfigMapsToDisk(ctx)
				p.deps.Logger.Infow("updated DNS ConfigMap (structural change)", "network", net.Name)
			}
		}
	}
}

// ─── Consistency Checks ─────────────────────────────────────────────────────

// checkNetworkCRDs verifies Network CRD consistency between memory and NATS,
// and checks DNS liveness for managed networks.
func (p *MicroKubeProvider) checkNetworkCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		storeKeys, err := p.deps.Store.Networks.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.networks {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("network-crd/%s", name),
						Status:  "pass",
						Message: "network CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("network-crd/%s", name),
						Status:  "fail",
						Message: "network CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			// NATS entries not in memory
			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("network-crd/%s", name),
					Status:  "warn",
					Message: "network CRD in NATS but not in memory",
				})
			}
		}
	}

	// DNS liveness per managed network
	for _, net := range p.networks {
		if net.Spec.ExternalDNS || net.Spec.DNS.Server == "" || net.Spec.DNS.Zone == "" {
			continue
		}
		alive := probeDNSPort(net.Spec.DNS.Server, net.Spec.DNS.Zone, 3*time.Second)
		if alive {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("network-dns/%s", net.Name),
				Status:  "pass",
				Message: fmt.Sprintf("DNS alive on %s", net.Spec.DNS.Server),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("network-dns/%s", net.Name),
				Status:  "fail",
				Message: fmt.Sprintf("DNS not responding on %s", net.Spec.DNS.Server),
			})
		}
	}

	return items
}
