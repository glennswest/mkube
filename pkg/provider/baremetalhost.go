package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"net"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// BareMetalHost represents a physical server managed through bmh-operator/IPMI.
type BareMetalHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              BMHSpec   `json:"spec"`
	Status            BMHStatus `json:"status,omitempty"`
}

type BMHSpec struct {
	BMC            BMCDetails      `json:"bmc,omitempty"`
	BootMACAddress string          `json:"bootMACAddress"`
	Online         *bool           `json:"online,omitempty"`
	Image          string          `json:"image,omitempty"`
	Network        string          `json:"network,omitempty"`        // network CRD name (e.g. "g10")
	IP             string          `json:"ip,omitempty"`             // static IP for DHCP reservation
	Hostname       string          `json:"hostname,omitempty"`       // hostname for DHCP reservation
	NextServer     string          `json:"nextServer,omitempty"`     // PXE next-server (TFTP)
	BootFile       string          `json:"bootFile,omitempty"`       // PXE boot file (BIOS)
	BootFileEFI    string          `json:"bootFileEfi,omitempty"`    // PXE boot file (UEFI)
	BootConfigRef  string          `json:"bootConfigRef,omitempty"`  // reference to a BootConfig CRD name (legacy — use Template for cloudid)
	Disk           string          `json:"disk,omitempty"`           // ISCSIDisk name for iSCSI root disk boot
	Template       string          `json:"template,omitempty"`       // cloudid template ref (e.g. "agent-runner.ign.json")
	Ignition       json.RawMessage `json:"ignition,omitempty"`       // base Ignition v3 JSON (platform config — disks, filesystems)
	Kickstart      string          `json:"kickstart,omitempty"`      // base kickstart text (platform config)
	NICs           []BMHNICSpec    `json:"nics,omitempty"`           // secondary NICs (B ports) — IP only, no gateway, no PXE
}

// BMHNICSpec describes a secondary NIC on a bare metal host.
// Secondary NICs get DHCP reservations with no default gateway and no PXE
// options, preventing competing routes and accidental PXE boot.
type BMHNICSpec struct {
	MAC     string `json:"mac"`
	IP      string `json:"ip,omitempty"`
	Role    string `json:"role"`              // "data", "mgmt", "storage"
	Network string `json:"network,omitempty"` // defaults to spec.network
}

type BMCDetails struct {
	Address  string `json:"address,omitempty"`  // IPMI IP address
	MAC      string `json:"mac,omitempty"`      // IPMI MAC address (for DHCP reservation)
	Hostname string `json:"hostname,omitempty"` // IPMI DNS name (e.g. "server1-ipmi")
	Network  string `json:"network,omitempty"`  // IPMI network CRD name (e.g. "g11")
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type BMHStatus struct {
	Phase             string             `json:"phase"`
	PoweredOn         bool               `json:"poweredOn"`
	IP                string             `json:"ip,omitempty"`
	LastBoot          string             `json:"lastBoot,omitempty"`
	BootCount         int                `json:"bootCount,omitempty"`
	ErrorMessage      string             `json:"errorMessage,omitempty"`
	AvailableImages   []string           `json:"availableImages,omitempty"`
	Hardware          *HardwareDetails   `json:"hardware,omitempty"`
	NetworkInterfaces map[string]NICInfo `json:"networkInterfaces,omitempty"`
	// Operator-managed fields (written by bmh-operator via PATCH)
	State           string `json:"state,omitempty"`           // Operator state: Idle, PoweringOn, Provisioning, Ready, PoweringOff, Error
	LastStateChange string `json:"lastStateChange,omitempty"` // RFC3339 timestamp of last state transition
	OperatorVersion string `json:"operatorVersion,omitempty"` // bmh-operator version managing this host
	SerialActive    bool   `json:"serialActive,omitempty"`    // IPMI SOL session active
	IPMIReachable   bool   `json:"ipmiReachable,omitempty"`   // Last IPMI ping result
}

// HardwareDetails holds full hardware inventory for a bare metal host.
type HardwareDetails struct {
	Manufacturer  string  `json:"manufacturer,omitempty"`
	ProductName   string  `json:"productName,omitempty"`
	SerialNumber  string  `json:"serialNumber,omitempty"`
	BIOSVersion   string  `json:"biosVersion,omitempty"`
	CPUModel      string  `json:"cpuModel,omitempty"`
	CPUCount      int     `json:"cpuCount,omitempty"`
	TotalMemoryGB float64 `json:"totalMemoryGB,omitempty"`
	DiskInfo      string  `json:"diskInfo,omitempty"`
	LastInventory string  `json:"lastInventory,omitempty"`
}

// NICInfo describes a network interface on a bare metal host.
type NICInfo struct {
	MAC     string `json:"mac"`
	Network string `json:"network,omitempty"`
	IP      string `json:"ip,omitempty"`
}

type BareMetalHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []BareMetalHost `json:"items"`
}


func (b *BareMetalHost) DeepCopy() *BareMetalHost {
	out := *b
	out.ObjectMeta = *b.ObjectMeta.DeepCopy()
	if b.Spec.Online != nil {
		v := *b.Spec.Online
		out.Spec.Online = &v
	}
	if b.Spec.Ignition != nil {
		ign := make(json.RawMessage, len(b.Spec.Ignition))
		copy(ign, b.Spec.Ignition)
		out.Spec.Ignition = ign
	}
	if b.Spec.NICs != nil {
		nics := make([]BMHNICSpec, len(b.Spec.NICs))
		copy(nics, b.Spec.NICs)
		out.Spec.NICs = nics
	}
	if b.Status.Hardware != nil {
		hw := *b.Status.Hardware
		out.Status.Hardware = &hw
	}
	if b.Status.NetworkInterfaces != nil {
		nics := make(map[string]NICInfo, len(b.Status.NetworkInterfaces))
		for k, v := range b.Status.NetworkInterfaces {
			nics[k] = v
		}
		out.Status.NetworkInterfaces = nics
	}
	return &out
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleCreateBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var bmh BareMetalHost
	if err := json.NewDecoder(r.Body).Decode(&bmh); err != nil {
		http.Error(w, fmt.Sprintf("invalid BareMetalHost JSON: %v", err), http.StatusBadRequest)
		return
	}
	bmh.Namespace = ns
	if bmh.Name == "" {
		http.Error(w, "BareMetalHost name is required", http.StatusBadRequest)
		return
	}
	bmh.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
	if bmh.CreationTimestamp.IsZero() {
		bmh.CreationTimestamp = metav1.Now()
	}
	if bmh.Status.Phase == "" {
		bmh.Status.Phase = "Registering"
	}
	// Default IPMI credentials if not provided
	if bmh.Spec.BMC.Address != "" {
		if bmh.Spec.BMC.Username == "" {
			bmh.Spec.BMC.Username = "ADMIN"
		}
		if bmh.Spec.BMC.Password == "" {
			bmh.Spec.BMC.Password = "ADMIN"
		}
	}

	key := ns + "/" + bmh.Name

	if p.bareMetalHosts.Has(key) {
		http.Error(w, fmt.Sprintf("BareMetalHost %s already exists", key), http.StatusConflict)
		return
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + bmh.Name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, &bmh); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts.Set(key, &bmh)

	// Sync DHCP reservations to Network CRDs (data + IPMI)
	p.syncBMHToNetwork(r.Context(), &bmh, "", "", "", "")

	// Sync BootConfig assignedTo
	p.syncBootConfigRef(r.Context(), bmh.Name, "", bmh.Spec.BootConfigRef)
	p.triggerScheduler()

	podWriteJSON(w, http.StatusCreated, &bmh)
}

func (p *MicroKubeProvider) handleGetBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	bmh, ok := p.bareMetalHosts.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	enriched := bmh.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, bmhListToTable([]BareMetalHost{*enriched}))
		return
	}

	podWriteJSON(w, http.StatusOK, enriched)
}

func (p *MicroKubeProvider) handleListAllBMH(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchBMH(w, r, "")
		return
	}

	snap := p.bareMetalHosts.Snapshot()
	items := make([]BareMetalHost, 0, len(snap))
	for _, bmh := range snap {
		enriched := bmh.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, bmhListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, BareMetalHostList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHostList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchBMH(w, r, ns)
		return
	}

	items := make([]BareMetalHost, 0)
	for _, bmh := range p.bareMetalHosts.Snapshot() {
		if !bmhReferencesNetwork(bmh, ns) {
			continue
		}
		enriched := bmh.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, bmhListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, BareMetalHostList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHostList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleUpdateBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.bareMetalHosts.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	oldDataNetwork := existing.Spec.Network
	oldIPMINetwork := existing.Spec.BMC.Network
	oldHostname := firstNonEmpty(existing.Spec.Hostname, existing.Name)
	oldIP := existing.Spec.IP
	oldBootConfigRef := existing.Spec.BootConfigRef

	var bmh BareMetalHost
	if err := json.NewDecoder(r.Body).Decode(&bmh); err != nil {
		http.Error(w, fmt.Sprintf("invalid BareMetalHost JSON: %v", err), http.StatusBadRequest)
		return
	}
	bmh.Namespace = ns
	bmh.Name = name
	bmh.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
	if bmh.CreationTimestamp.IsZero() {
		bmh.CreationTimestamp = existing.CreationTimestamp
	}
	// Preserve existing credentials if not provided in the update
	if bmh.Spec.BMC.Username == "" && existing.Spec.BMC.Username != "" {
		bmh.Spec.BMC.Username = existing.Spec.BMC.Username
	}
	if bmh.Spec.BMC.Password == "" && existing.Spec.BMC.Password != "" {
		bmh.Spec.BMC.Password = existing.Spec.BMC.Password
	}

	// Detect manual power-on via PUT
	wasOnline := existing.Spec.Online != nil && *existing.Spec.Online
	isOnline := bmh.Spec.Online != nil && *bmh.Spec.Online
	if isOnline && !wasOnline {
		if bmh.Annotations == nil {
			bmh.Annotations = make(map[string]string)
		}
		bmh.Annotations["bmh.mkube.io/manual-power"] = time.Now().UTC().Format(time.RFC3339)
		p.deps.Logger.Infow("manual power-on detected, scheduler will not override",
			"bmh", name, "annotation", "bmh.mkube.io/manual-power")
	}
	if !isOnline && wasOnline {
		delete(bmh.Annotations, "bmh.mkube.io/manual-power")
	}

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, &bmh); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts.Set(key, &bmh)

	// Clean up DHCP reservations for NICs that were removed
	p.cleanRemovedNICs(r.Context(), existing, &bmh)

	// Sync DHCP reservations + DNS to Network CRDs (data + IPMI + NICs)
	p.syncBMHToNetwork(r.Context(), &bmh, oldDataNetwork, oldIPMINetwork, oldHostname, oldIP)

	// Sync BootConfig assignedTo
	p.syncBootConfigRef(r.Context(), bmh.Name, oldBootConfigRef, bmh.Spec.BootConfigRef)
	p.triggerScheduler()

	podWriteJSON(w, http.StatusOK, &bmh)
}

func (p *MicroKubeProvider) handlePatchBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.bareMetalHosts.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	// Start from existing, overlay the patch
	oldDataNetwork := existing.Spec.Network
	oldIPMINetwork := existing.Spec.BMC.Network
	oldHostname := firstNonEmpty(existing.Spec.Hostname, existing.Name)
	oldIP := existing.Spec.IP
	oldBootConfigRef := existing.Spec.BootConfigRef
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
	merged.Namespace = ns
	merged.Name = name
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}

	// Preserve existing credentials if not provided in patch
	if merged.Spec.BMC.Username == "" && existing.Spec.BMC.Username != "" {
		merged.Spec.BMC.Username = existing.Spec.BMC.Username
	}
	if merged.Spec.BMC.Password == "" && existing.Spec.BMC.Password != "" {
		merged.Spec.BMC.Password = existing.Spec.BMC.Password
	}

	// Detect manual power-on: user set spec.online=true via PATCH.
	// Add annotation so the scheduler doesn't immediately power it back off.
	wasOnline := existing.Spec.Online != nil && *existing.Spec.Online
	isOnline := merged.Spec.Online != nil && *merged.Spec.Online
	if isOnline && !wasOnline {
		if merged.Annotations == nil {
			merged.Annotations = make(map[string]string)
		}
		merged.Annotations["bmh.mkube.io/manual-power"] = time.Now().UTC().Format(time.RFC3339)
		p.deps.Logger.Infow("manual power-on detected, scheduler will not override",
			"bmh", name, "annotation", "bmh.mkube.io/manual-power")
	}
	// Clear manual-power annotation when user powers off
	if !isOnline && wasOnline {
		delete(merged.Annotations, "bmh.mkube.io/manual-power")
	}

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts.Set(key, merged)

	// Clean up DHCP reservations for NICs that were removed
	p.cleanRemovedNICs(r.Context(), existing, merged)

	// Sync DHCP reservations + DNS to Network CRDs (data + IPMI + NICs)
	p.syncBMHToNetwork(r.Context(), merged, oldDataNetwork, oldIPMINetwork, oldHostname, oldIP)

	// Sync BootConfig assignedTo
	p.syncBootConfigRef(r.Context(), merged.Name, oldBootConfigRef, merged.Spec.BootConfigRef)
	p.triggerScheduler()

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	bmh, ok := p.bareMetalHosts.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	// Remove DHCP reservations from referenced Network CRDs (data + IPMI + NICs)
	p.removeBMHFromNetwork(r.Context(), bmh.Spec.BootMACAddress, bmh.Spec.Network)
	p.removeBMHFromNetwork(r.Context(), bmh.Spec.BMC.MAC, bmh.Spec.BMC.Network)

	// Remove secondary NIC reservations and DNS
	for _, nic := range bmh.Spec.NICs {
		nicNetwork := nic.Network
		if nicNetwork == "" {
			nicNetwork = bmh.Spec.Network
		}
		p.removeBMHFromNetwork(r.Context(), nic.MAC, nicNetwork)
		if nicNetwork != "" && nic.IP != "" {
			hostname := firstNonEmpty(bmh.Spec.Hostname, bmh.Name)
			nicHostname := hostname + "-" + nic.Role
			if err := p.deps.NetworkMgr.DeregisterDNS(r.Context(), nicNetwork, nicHostname, nic.IP); err != nil {
				p.deps.Logger.Warnw("BMH NIC DNS deregistration failed", "bmh", bmh.Name, "nic", nic.MAC, "error", err)
			}
		}
	}

	// Remove DNS A record for data network
	if bmh.Spec.Network != "" && bmh.Spec.IP != "" {
		hostname := firstNonEmpty(bmh.Spec.Hostname, bmh.Name)
		if err := p.deps.NetworkMgr.DeregisterDNS(r.Context(), bmh.Spec.Network, hostname, bmh.Spec.IP); err != nil {
			p.deps.Logger.Warnw("BMH DNS deregistration failed", "bmh", bmh.Name, "error", err)
		}
	}

	// Remove from BootConfig assignedTo
	p.removeBootConfigRef(r.Context(), bmh.Name, bmh.Spec.BootConfigRef)

	p.bareMetalHosts.Delete(key)

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.BareMetalHosts.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete BMH from store", "key", storeKey, "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("BareMetalHost %q deleted", name),
	})
}

// handleRefreshBMH triggers a baremetalservices re-probe by setting an annotation.
func (p *MicroKubeProvider) handleRefreshBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	ts := time.Now().UTC().Format(time.RFC3339)
	if err := p.updateBMHFields(r.Context(), key, func(bmh *BareMetalHost) {
		if bmh.Annotations == nil {
			bmh.Annotations = make(map[string]string)
		}
		bmh.Annotations["bmh.mkube.io/refresh"] = ts
	}); err != nil {
		http.Error(w, fmt.Sprintf("persisting refresh: %v", err), http.StatusInternalServerError)
		return
	}

	podWriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": fmt.Sprintf("refresh requested for %s", name),
	})
}

// handleRefreshAllBMH triggers a refresh on every BMH.
func (p *MicroKubeProvider) handleRefreshAllBMH(w http.ResponseWriter, r *http.Request) {
	ts := time.Now().UTC().Format(time.RFC3339)
	var names []string

	for key, bmh := range p.bareMetalHosts.Snapshot() {
		if err := p.updateBMHFields(r.Context(), key, func(b *BareMetalHost) {
			if b.Annotations == nil {
				b.Annotations = make(map[string]string)
			}
			b.Annotations["bmh.mkube.io/refresh"] = ts
		}); err != nil {
			p.deps.Logger.Warnw("failed to persist refresh for BMH", "key", key, "error", err)
			continue
		}
		names = append(names, bmh.Name)
	}

	podWriteJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("refresh requested for %d hosts", len(names)),
		"hosts":   names,
	})
}

// updateBMHFields atomically applies field-level updates to a BMH without
// overwriting the entire object. This prevents the read-merge-write race where
// internal reconciliation (job scheduler, consistency checks) can clobber a
// concurrent PATCH from the user (e.g. spec.online=true gets overwritten).
//
// The mutate function receives the live in-memory BMH and should only modify
// the specific fields it owns. The updated object is then persisted to NATS.
func (p *MicroKubeProvider) updateBMHFields(ctx context.Context, key string, mutate func(bmh *BareMetalHost)) error {
	bmh, ok := p.bareMetalHosts.Get(key)
	if !ok {
		return fmt.Errorf("BareMetalHost %s not found", key)
	}

	// Apply the mutation to the live object
	mutate(bmh)

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 {
			storeKey := parts[0] + "." + parts[1]
			if _, err := p.deps.Store.BareMetalHosts.PutJSON(ctx, storeKey, bmh); err != nil {
				return fmt.Errorf("persisting BMH update: %w", err)
			}
		}
	}
	return nil
}

// ─── BMH → Network CRD Sync ─────────────────────────────────────────────────

// syncBMHToNetwork upserts DHCP reservations on the BMH's referenced Network CRDs
// (both data network and IPMI network). Old network/hostname/IP are used to clean up
// reservations and DNS records when references change.
func (p *MicroKubeProvider) syncBMHToNetwork(ctx context.Context, bmh *BareMetalHost, oldDataNetwork, oldIPMINetwork, oldHostname, oldIP string) {
	log := p.deps.Logger

	// Sync data network reservation (boot MAC → data network)
	if oldDataNetwork != "" && oldDataNetwork != bmh.Spec.Network {
		p.removeBMHFromNetwork(ctx, bmh.Spec.BootMACAddress, oldDataNetwork)
		// Deregister old DNS A record from old network
		if oldIP != "" && oldHostname != "" {
			if err := p.deps.NetworkMgr.DeregisterDNS(ctx, oldDataNetwork, oldHostname, oldIP); err != nil {
				log.Warnw("BMH old DNS deregistration failed", "bmh", bmh.Name, "oldNetwork", oldDataNetwork, "error", err)
			}
		}
	}
	if bmh.Spec.Network != "" && bmh.Spec.BootMACAddress != "" {
		hostname := firstNonEmpty(bmh.Spec.Hostname, bmh.Name)
		res := NetworkDHCPReservation{
			MAC:         bmh.Spec.BootMACAddress,
			IP:          bmh.Spec.IP,
			Hostname:    hostname,
			NextServer:  bmh.Spec.NextServer,
			BootFile:    bmh.Spec.BootFile,
			BootFileEFI: bmh.Spec.BootFileEFI,
		}

		// All BMH images use a dynamic iPXE boot script endpoint.
		// The endpoint returns sanboot for CDROM images (and auto-switches
		// to localboot) or exit for localboot. This avoids root_path in
		// DHCP reservations entirely — iPXE fetches the script and acts on it.
		if bmh.Spec.Image != "" && bmh.Spec.Image != "baremetalservices" {
			// Build mkube's own API URL with a resolved IP so iPXE doesn't need DNS.
			mkubeHost := "mkube." + p.deps.Config.DefaultNetwork().DNS.Zone
			if ips, err := net.LookupHost(mkubeHost); err == nil && len(ips) > 0 {
				res.IPXEBootURL = fmt.Sprintf("http://%s:8082/api/v1/ipxe/boot", ips[0])
			} else {
				log.Warnw("failed to resolve mkube hostname for iPXE boot URL, using hostname", "host", mkubeHost, "error", err)
				res.IPXEBootURL = fmt.Sprintf("http://%s:8082/api/v1/ipxe/boot", mkubeHost)
			}
		}

		p.upsertNetworkReservation(ctx, bmh.Spec.Network, res, bmh.Name)

		// Auto-register DNS A record for the data network
		if bmh.Spec.IP != "" && hostname != "" {
			if err := p.deps.NetworkMgr.CleanStaleDNS(ctx, bmh.Spec.Network, hostname, bmh.Spec.IP); err != nil {
				log.Warnw("BMH DNS stale cleanup failed", "bmh", bmh.Name, "network", bmh.Spec.Network, "error", err)
			}
			if err := p.deps.NetworkMgr.RegisterDNS(ctx, bmh.Spec.Network, hostname, bmh.Spec.IP); err != nil {
				log.Warnw("BMH DNS registration failed", "bmh", bmh.Name, "network", bmh.Spec.Network, "error", err)
			} else {
				log.Infow("registered BMH DNS A record", "bmh", bmh.Name, "hostname", hostname, "ip", bmh.Spec.IP, "network", bmh.Spec.Network)
			}
		}
	}

	// Sync secondary NIC reservations (B ports — IP only, no gateway, no PXE).
	// Gateway is set to 0.0.0.0 to suppress the pool default route,
	// preventing B ports from creating competing default routes with the A port.
	for _, nic := range bmh.Spec.NICs {
		nicNetwork := nic.Network
		if nicNetwork == "" {
			nicNetwork = bmh.Spec.Network
		}
		if nicNetwork == "" || nic.MAC == "" {
			continue
		}

		hostname := firstNonEmpty(bmh.Spec.Hostname, bmh.Name)
		nicHostname := hostname + "-" + nic.Role

		res := NetworkDHCPReservation{
			MAC:      nic.MAC,
			IP:       nic.IP,
			Hostname: nicHostname,
			Gateway:  "0.0.0.0", // suppress default route — B port must not have a gateway
			// NextServer, BootFile intentionally empty — no PXE for secondary NICs
		}

		p.upsertNetworkReservation(ctx, nicNetwork, res, bmh.Name)

		// Auto-register DNS A record for the secondary NIC
		if nic.IP != "" {
			if err := p.deps.NetworkMgr.CleanStaleDNS(ctx, nicNetwork, nicHostname, nic.IP); err != nil {
				log.Warnw("BMH NIC DNS stale cleanup failed", "bmh", bmh.Name, "nic", nic.MAC, "error", err)
			}
			if err := p.deps.NetworkMgr.RegisterDNS(ctx, nicNetwork, nicHostname, nic.IP); err != nil {
				log.Warnw("BMH NIC DNS registration failed", "bmh", bmh.Name, "nic", nic.MAC, "error", err)
			} else {
				log.Infow("registered BMH NIC DNS A record", "bmh", bmh.Name, "hostname", nicHostname, "ip", nic.IP, "network", nicNetwork)
			}
		}
	}

	// Sync IPMI network reservation (IPMI MAC → IPMI network)
	if oldIPMINetwork != "" && oldIPMINetwork != bmh.Spec.BMC.Network {
		p.removeBMHFromNetwork(ctx, bmh.Spec.BMC.MAC, oldIPMINetwork)
	}
	if bmh.Spec.BMC.Network != "" && bmh.Spec.BMC.MAC != "" {
		p.upsertNetworkReservation(ctx, bmh.Spec.BMC.Network, NetworkDHCPReservation{
			MAC:      bmh.Spec.BMC.MAC,
			IP:       bmh.Spec.BMC.Address,
			Hostname: firstNonEmpty(bmh.Spec.BMC.Hostname, bmh.Name+"-ipmi"),
		}, bmh.Name)
	}
}

// cleanRemovedNICs removes DHCP reservations and DNS records for secondary NICs
// that were present in the old BMH spec but are missing from the new one.
func (p *MicroKubeProvider) cleanRemovedNICs(ctx context.Context, oldBMH, newBMH *BareMetalHost) {
	newMACs := make(map[string]struct{}, len(newBMH.Spec.NICs))
	for _, nic := range newBMH.Spec.NICs {
		newMACs[strings.ToLower(nic.MAC)] = struct{}{}
	}

	for _, nic := range oldBMH.Spec.NICs {
		if _, kept := newMACs[strings.ToLower(nic.MAC)]; kept {
			continue
		}
		// This NIC was removed — clean up its reservation and DNS
		nicNetwork := nic.Network
		if nicNetwork == "" {
			nicNetwork = oldBMH.Spec.Network
		}
		p.removeBMHFromNetwork(ctx, nic.MAC, nicNetwork)

		if nicNetwork != "" && nic.IP != "" {
			hostname := firstNonEmpty(oldBMH.Spec.Hostname, oldBMH.Name)
			nicHostname := hostname + "-" + nic.Role
			if err := p.deps.NetworkMgr.DeregisterDNS(ctx, nicNetwork, nicHostname, nic.IP); err != nil {
				p.deps.Logger.Warnw("removed NIC DNS deregistration failed",
					"bmh", oldBMH.Name, "nic", nic.MAC, "error", err)
			}
		}

		p.deps.Logger.Infow("cleaned up removed NIC reservation",
			"bmh", oldBMH.Name, "mac", nic.MAC, "network", nicNetwork)
	}
}

// upsertNetworkReservation inserts or updates a DHCP reservation on a Network CRD by MAC.
// Also pushes the reservation directly to the microdns REST API for immediate effect.
// Populates gateway, DNS servers, and domain from the Network CRD when not already set,
// so every reservation is self-contained and doesn't depend on pool fallback.
func (p *MicroKubeProvider) upsertNetworkReservation(ctx context.Context, networkName string, res NetworkDHCPReservation, bmhName string) {
	log := p.deps.Logger

	net, ok := p.networks.Get(networkName)
	if !ok {
		log.Warnw("BMH references unknown network", "bmh", bmhName, "network", networkName)
		return
	}

	// Populate network defaults so reservations are self-contained.
	// Clients with reserved IPs outside the pool range would otherwise
	// get no gateway/DNS and have no route to other networks.
	if res.Gateway == "" && net.Spec.Gateway != "" {
		res.Gateway = net.Spec.Gateway
	}
	if len(res.DNSServers) == 0 && net.Spec.DNS.Server != "" {
		res.DNSServers = []string{net.Spec.DNS.Server}
	}
	if res.Domain == "" && net.Spec.DNS.Zone != "" {
		res.Domain = net.Spec.DNS.Zone
	}

	normalizedMAC := strings.ToLower(res.MAC)
	found := false
	for i, existing := range net.Spec.DHCP.Reservations {
		if strings.ToLower(existing.MAC) == normalizedMAC {
			net.Spec.DHCP.Reservations[i] = res
			found = true
			break
		}
	}
	if !found {
		net.Spec.DHCP.Reservations = append(net.Spec.DHCP.Reservations, res)
	}

	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(ctx, net.Name, net); err != nil {
			log.Warnw("failed to persist network after BMH sync", "network", net.Name, "error", err)
			return
		}
	}

	// Push directly to microdns REST API for immediate effect
	endpoint := p.networkDNSEndpoint(net)
	if endpoint != "" {
		dnsRes := networkReservationToDNS(res)
		if err := p.deps.NetworkMgr.DNSClient().UpsertDHCPReservation(ctx, endpoint, dnsRes); err != nil {
			log.Warnw("failed to upsert DHCP reservation via REST API",
				"bmh", bmhName, "network", net.Name, "mac", res.MAC, "error", err)
		}
	}

	log.Infow("synced BMH to network DHCP reservation",
		"bmh", bmhName, "network", net.Name, "mac", res.MAC, "ip", res.IP, "upsert", !found)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// removeBMHFromNetwork removes a DHCP reservation by MAC from a Network CRD.
// Also removes it from the microdns REST API for immediate effect.
func (p *MicroKubeProvider) removeBMHFromNetwork(ctx context.Context, mac, networkName string) {
	log := p.deps.Logger

	if mac == "" || networkName == "" {
		return
	}

	net, ok := p.networks.Get(networkName)
	if !ok {
		return
	}

	normalizedMAC := strings.ToLower(mac)
	newReservations := make([]NetworkDHCPReservation, 0, len(net.Spec.DHCP.Reservations))
	removed := false
	for _, r := range net.Spec.DHCP.Reservations {
		if strings.ToLower(r.MAC) == normalizedMAC {
			removed = true
			continue
		}
		newReservations = append(newReservations, r)
	}

	if !removed {
		return
	}

	net.Spec.DHCP.Reservations = newReservations

	// Persist updated network to NATS
	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(ctx, net.Name, net); err != nil {
			log.Warnw("failed to persist network after BMH removal", "network", net.Name, "error", err)
			return
		}
	}

	// Remove from microdns REST API for immediate effect
	endpoint := p.networkDNSEndpoint(net)
	if endpoint != "" {
		if err := p.deps.NetworkMgr.DNSClient().DeleteDHCPReservation(ctx, endpoint, mac); err != nil {
			log.Warnw("failed to delete DHCP reservation via REST API",
				"network", net.Name, "mac", mac, "error", err)
		}
	}

	log.Infow("removed BMH DHCP reservation from network", "network", net.Name, "mac", mac)
}

// networkDNSEndpoint returns the microdns REST API endpoint for a network,
// or empty string if no endpoint is available.
func (p *MicroKubeProvider) networkDNSEndpoint(net *Network) string {
	if net.Spec.DNS.Endpoint != "" {
		return net.Spec.DNS.Endpoint
	}
	if net.Spec.DNS.Server != "" {
		return "http://" + net.Spec.DNS.Server + ":8080"
	}
	return ""
}


// ─── Table API ──────────────────────────────────────────────────────────────

var serverNameRe = regexp.MustCompile(`^(.*?)(\d+)$`)

func bmhListToTable(hosts []BareMetalHost) *metav1.Table {
	// Natural sort: server1, server2, ..., server8, server30
	sort.Slice(hosts, func(i, j int) bool {
		mi := serverNameRe.FindStringSubmatch(hosts[i].Name)
		mj := serverNameRe.FindStringSubmatch(hosts[j].Name)
		if mi != nil && mj != nil && mi[1] == mj[1] {
			ni, _ := strconv.Atoi(mi[2])
			nj, _ := strconv.Atoi(mj[2])
			return ni < nj
		}
		return hosts[i].Name < hosts[j].Name
	})

	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string"},
			{Name: "State", Type: "string"},
			{Name: "Power", Type: "string"},
			{Name: "Network", Type: "string"},
			{Name: "Image", Type: "string"},
			{Name: "IP", Type: "string"},
			{Name: "IPMI", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range hosts {
		h := &hosts[i]

		power := "off"
		if h.Status.PoweredOn {
			power = "on"
		}

		image := h.Spec.Image
		if h.Spec.Disk != "" {
			image = "disk:" + h.Spec.Disk
		} else if h.Spec.Template != "" {
			image = "tpl:" + h.Spec.Template
			if h.Spec.Image != "" {
				image = h.Spec.Image + " tpl:" + h.Spec.Template
			}
		} else if image == "" {
			image = "localboot"
		}

		state := h.Status.State
		if state == "" {
			state = "-"
		}

		age := "<unknown>"
		if !h.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(h.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              h.Name,
				"namespace":         h.Namespace,
				"creationTimestamp": h.CreationTimestamp.Format(time.RFC3339),
			},
		})
		network := h.Spec.Network
		if network == "" {
			network = "-"
		}

		ip := h.Spec.IP
		if ip == "" {
			ip = h.Status.IP
		}

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				h.Name,
				h.Status.Phase,
				state,
				power,
				network,
				image,
				ip,
				h.Spec.BMC.Address,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// LoadBMHFromStore loads BMH objects from NATS store into the in-memory map.
func (p *MicroKubeProvider) LoadBMHFromStore(ctx context.Context) {
	if p.deps.Store == nil {
		p.deps.Logger.Warnw("LoadBMHFromStore: store is nil")
		return
	}
	if p.deps.Store.BareMetalHosts == nil {
		p.deps.Logger.Warnw("LoadBMHFromStore: BareMetalHosts bucket is nil")
		return
	}

	keys, err := p.deps.Store.BareMetalHosts.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list BMH from store", "error", err)
		return
	}

	p.deps.Logger.Infow("LoadBMHFromStore: found keys", "count", len(keys))

	loaded := 0
	for _, key := range keys {
		var bmh BareMetalHost
		if _, err := p.deps.Store.BareMetalHosts.GetJSON(ctx, key, &bmh); err != nil {
			p.deps.Logger.Warnw("failed to read BMH from store", "key", key, "error", err)
			continue
		}
		mapKey := bmh.Namespace + "/" + bmh.Name
		p.bareMetalHosts.Set(mapKey, &bmh)
		loaded++
	}

	p.deps.Logger.Infow("loaded BMH from store", "keys", len(keys), "loaded", loaded, "mapSize", p.bareMetalHosts.Len())
}

// handleReloadBMH is a debug endpoint that reloads BMH from NATS store.
func (p *MicroKubeProvider) handleReloadBMH(w http.ResponseWriter, r *http.Request) {
	storeNil := p.deps.Store == nil
	bucketNil := storeNil || p.deps.Store.BareMetalHosts == nil
	beforeCount := p.bareMetalHosts.Len()

	// Directly try Keys and report
	var keysErr string
	var keysList []string
	var getErrors []string
	if !storeNil && !bucketNil {
		keys, err := p.deps.Store.BareMetalHosts.Keys(r.Context(), "")
		if err != nil {
			keysErr = err.Error()
		} else {
			keysList = keys
			// Try reading first few
			for i, key := range keys {
				if i >= 3 {
					break
				}
				var bmh BareMetalHost
				if _, err := p.deps.Store.BareMetalHosts.GetJSON(r.Context(), key, &bmh); err != nil {
					getErrors = append(getErrors, key+": "+err.Error())
				}
			}
		}
	}

	p.LoadBMHFromStore(r.Context())
	afterCount := p.bareMetalHosts.Len()

	resp := map[string]interface{}{
		"storeNil":    storeNil,
		"bucketNil":   bucketNil,
		"beforeCount": beforeCount,
		"afterCount":  afterCount,
		"keysErr":     keysErr,
		"keysCount":   len(keysList),
		"keysSample":  keysList,
		"getErrors":   getErrors,
	}
	podWriteJSON(w, http.StatusOK, resp)
}

// handleWatchBMH streams BMH events as newline-delimited JSON (K8s watch format).
func (p *MicroKubeProvider) handleWatchBMH(w http.ResponseWriter, r *http.Request, nsFilter string) {
	if p.deps.Store == nil || p.deps.Store.BareMetalHosts == nil {
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

	// Send existing BMH objects as ADDED events first (snapshot)
	enc := json.NewEncoder(w)
	snapshot := make([]*BareMetalHost, 0)
	for _, bmh := range p.bareMetalHosts.Snapshot() {
		if nsFilter != "" && bmh.Namespace != nsFilter {
			continue
		}
		snapshot = append(snapshot, bmh.DeepCopy())
	}
	for _, enriched := range snapshot {
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		evt := K8sWatchEvent{Type: "ADDED", Object: enriched}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Open watch on the NATS KV store
	events, err := p.deps.Store.BareMetalHosts.WatchAll(ctx)
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

			var bmh BareMetalHost
			if evt.Type == store.EventDelete {
				ns, name := parseStoreKey(evt.Key)
				if nsFilter != "" && ns != nsFilter {
					continue
				}
				bmh = BareMetalHost{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &bmh); err != nil {
					continue
				}
				if nsFilter != "" && bmh.Namespace != nsFilter {
					continue
				}
				bmh.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &bmh,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// bmhReferencesNetwork returns true if the BMH has any association with the
// given network name — data network, IPMI/BMC network, or metadata namespace.
// This is used to implement "join"-style queries: querying g10 returns every
// physical server that has at least one NIC on g10, regardless of where
// it was originally discovered.
func bmhReferencesNetwork(bmh *BareMetalHost, network string) bool {
	if bmh.Namespace == network {
		return true
	}
	if bmh.Spec.Network != "" && bmh.Spec.Network == network {
		return true
	}
	if bmh.Spec.BMC.Network != "" && bmh.Spec.BMC.Network == network {
		return true
	}
	// Check secondary NICs (B ports)
	for _, nic := range bmh.Spec.NICs {
		nicNet := nic.Network
		if nicNet == "" {
			nicNet = bmh.Spec.Network
		}
		if nicNet == network {
			return true
		}
	}
	for _, nic := range bmh.Status.NetworkInterfaces {
		if nic.Network == network {
			return true
		}
	}
	return false
}
