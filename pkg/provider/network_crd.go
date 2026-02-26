package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

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
	MAC         string `json:"mac"`
	IP          string `json:"ip"`
	Hostname    string `json:"hostname,omitempty"`
	NextServer  string `json:"nextServer,omitempty"`  // per-host PXE next-server
	BootFile    string `json:"bootFile,omitempty"`     // per-host PXE boot file (BIOS)
	BootFileEFI string `json:"bootFileEfi,omitempty"` // per-host PXE boot file (UEFI)
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
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded networks from store", "count", len(keys))
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
	if nd.ExternalDNS {
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
			StaticRecords: staticRecords,
		},
		Status: NetworkStatus{
			Phase: "Active",
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

	p.networks[net.Name] = &net
	podWriteJSON(w, http.StatusCreated, &net)
}

func (p *MicroKubeProvider) handleUpdateNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if _, ok := p.networks[name]; !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

	var net Network
	if err := json.NewDecoder(r.Body).Decode(&net); err != nil {
		http.Error(w, fmt.Sprintf("invalid Network JSON: %v", err), http.StatusBadRequest)
		return
	}
	net.Name = name
	net.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Network"}

	// Preserve creation timestamp from existing
	if net.CreationTimestamp.IsZero() {
		net.CreationTimestamp = p.networks[name].CreationTimestamp
	}

	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(r.Context(), name, &net); err != nil {
			http.Error(w, fmt.Sprintf("persisting network update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.networks[name] = &net
	podWriteJSON(w, http.StatusOK, &net)
}

func (p *MicroKubeProvider) handlePatchNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.networks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

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
	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if _, ok := p.networks[name]; !ok {
		http.Error(w, fmt.Sprintf("network %q not found", name), http.StatusNotFound)
		return
	}

	// Check if any pod references this network
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

	toml := p.generateNetworkTOML(net)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(toml))
}

// generateNetworkTOML produces a complete microdns TOML config from a Network CRD.
func (p *MicroKubeProvider) generateNetworkTOML(net *Network) string {
	// Collect remote DHCP sections: other networks that relay to this one
	var remoteDHCP []string
	for _, peer := range p.networks {
		if peer.Name == net.Name {
			continue
		}
		if peer.Spec.DHCP.Enabled && peer.Spec.DHCP.ServerNetwork == net.Name {
			remoteDHCP = append(remoteDHCP, buildNetworkDHCPSection(peer))
		}
	}

	// Forward zones: all peer networks
	fwdZones := p.computeForwardZones(net.Name)

	// Local DHCP section (no serverNetwork means served locally)
	var dhcpSection string
	if net.Spec.DHCP.Enabled && net.Spec.DHCP.ServerNetwork == "" {
		dhcpSection = buildNetworkDHCPSection(net)
	}
	for _, section := range remoteDHCP {
		dhcpSection += section
	}

	return fmt.Sprintf(`[instance]
id = "microdns-%s"
mode = "standalone"

[dns.auth]
enabled = true
listen = "0.0.0.0:15353"
zones = ["%s"]

[dns.recursor]
enabled = true
listen = "0.0.0.0:53"

[dns.recursor.forward_zones]
%s
[api.rest]
enabled = true
listen = "0.0.0.0:8080"

[database]
path = "./data/microdns.redb"

[logging]
level = "info"
format = "text"
%s`, net.Name, net.Spec.DNS.Zone, fwdZones, dhcpSection)
}

// computeForwardZones builds the TOML forward_zones map for a network,
// listing all peer networks' DNS servers.
func (p *MicroKubeProvider) computeForwardZones(excludeName string) string {
	var b strings.Builder
	for _, peer := range p.networks {
		if peer.Name == excludeName || peer.Spec.DNS.Zone == "" || peer.Spec.DNS.Server == "" {
			continue
		}
		fmt.Fprintf(&b, "    %q = [\"%s:53\"]\n", peer.Spec.DNS.Zone, peer.Spec.DNS.Server)
	}
	return b.String()
}

// buildNetworkDHCPSection generates the TOML DHCP config block from a Network CRD.
func buildNetworkDHCPSection(net *Network) string {
	var dhcp strings.Builder
	leaseTime := net.Spec.DHCP.LeaseTime
	if leaseTime == 0 {
		leaseTime = 3600
	}

	fmt.Fprintf(&dhcp, "\n[dhcp.v4]\nenabled = true\ninterface = \"eth0\"\nserver_ip = %q\nlisten_ports = [67]\n\n", net.Spec.DNS.Server)
	fmt.Fprintf(&dhcp, "[[dhcp.v4.pools]]\n")
	fmt.Fprintf(&dhcp, "range_start = %q\n", net.Spec.DHCP.RangeStart)
	fmt.Fprintf(&dhcp, "range_end = %q\n", net.Spec.DHCP.RangeEnd)
	fmt.Fprintf(&dhcp, "subnet = %q\n", net.Spec.CIDR)
	fmt.Fprintf(&dhcp, "gateway = %q\n", net.Spec.Gateway)
	fmt.Fprintf(&dhcp, "dns = [%q]\n", net.Spec.DNS.Server)
	fmt.Fprintf(&dhcp, "domain = %q\n", net.Spec.DNS.Zone)
	fmt.Fprintf(&dhcp, "lease_time_secs = %d\n", leaseTime)
	if net.Spec.DHCP.NextServer != "" {
		fmt.Fprintf(&dhcp, "next_server = %q\n", net.Spec.DHCP.NextServer)
	}
	if net.Spec.DHCP.BootFile != "" {
		fmt.Fprintf(&dhcp, "boot_file = %q\n", net.Spec.DHCP.BootFile)
		if net.Spec.DHCP.NextServer != "" {
			fmt.Fprintf(&dhcp, "ipxe_boot_url = \"http://%s:8080/boot.ipxe\"\n", net.Spec.DHCP.NextServer)
		}
	}
	if net.Spec.DHCP.BootFileEFI != "" {
		fmt.Fprintf(&dhcp, "boot_file_efi = %q\n", net.Spec.DHCP.BootFileEFI)
	}
	for _, r := range net.Spec.DHCP.Reservations {
		fmt.Fprintf(&dhcp, "\n[[dhcp.v4.reservations]]\n")
		fmt.Fprintf(&dhcp, "mac = %q\n", r.MAC)
		fmt.Fprintf(&dhcp, "ip = %q\n", r.IP)
		if r.Hostname != "" {
			fmt.Fprintf(&dhcp, "hostname = %q\n", r.Hostname)
		}
		if r.NextServer != "" {
			fmt.Fprintf(&dhcp, "next_server = %q\n", r.NextServer)
		}
		if r.BootFile != "" {
			fmt.Fprintf(&dhcp, "boot_file = %q\n", r.BootFile)
		}
		if r.BootFileEFI != "" {
			fmt.Fprintf(&dhcp, "boot_file_efi = %q\n", r.BootFileEFI)
		}
	}

	// Build reverse zone from CIDR: 192.168.11.0/24 -> 11.168.192.in-addr.arpa
	reverseZone := ""
	if cidrParts := strings.Split(net.Spec.CIDR, "/"); len(cidrParts) == 2 {
		octets := strings.Split(cidrParts[0], ".")
		if len(octets) == 4 {
			reverseZone = fmt.Sprintf("%s.%s.%s.in-addr.arpa", octets[2], octets[1], octets[0])
		}
	}
	fmt.Fprintf(&dhcp, "\n[dhcp.dns_registration]\n")
	fmt.Fprintf(&dhcp, "enabled = true\n")
	fmt.Fprintf(&dhcp, "forward_zone = %q\n", net.Spec.DNS.Zone)
	fmt.Fprintf(&dhcp, "reverse_zone_v4 = %q\n", reverseZone)
	fmt.Fprintf(&dhcp, "reverse_zone_v6 = \"\"\n")
	fmt.Fprintf(&dhcp, "default_ttl = 300\n")
	return dhcp.String()
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

	// Send existing Network objects as ADDED events
	enc := json.NewEncoder(w)
	for _, net := range p.networks {
		enriched := net.DeepCopy()
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
