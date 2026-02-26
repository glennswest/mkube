package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// BareMetalHost represents a physical server managed through pxemanager/IPMI.
type BareMetalHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              BMHSpec   `json:"spec"`
	Status            BMHStatus `json:"status,omitempty"`
}

type BMHSpec struct {
	BMC            BMCDetails `json:"bmc,omitempty"`
	BootMACAddress string     `json:"bootMACAddress"`
	Online         *bool      `json:"online,omitempty"`
	Image          string     `json:"image,omitempty"`
	Network        string     `json:"network,omitempty"`        // network CRD name (e.g. "g10")
	IP             string     `json:"ip,omitempty"`             // static IP for DHCP reservation
	Hostname       string     `json:"hostname,omitempty"`       // hostname for DHCP reservation
	NextServer     string     `json:"nextServer,omitempty"`     // PXE next-server (TFTP)
	BootFile       string     `json:"bootFile,omitempty"`       // PXE boot file (BIOS)
	BootFileEFI    string     `json:"bootFileEfi,omitempty"`    // PXE boot file (UEFI)
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
	Phase        string `json:"phase"`
	PoweredOn    bool   `json:"poweredOn"`
	IP           string `json:"ip,omitempty"`
	LastBoot     string `json:"lastBoot,omitempty"`
	BootCount    int    `json:"bootCount,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
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

	key := ns + "/" + bmh.Name

	if _, exists := p.bareMetalHosts[key]; exists {
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

	p.bareMetalHosts[key] = &bmh

	// Register in pxemanager
	if bmh.Spec.BootMACAddress != "" {
		if err := pxeRegisterHost(r.Context(), p.deps.Config.BMH.PXEManagerURL, bmh.Spec.BootMACAddress, bmh.Name, bmh.Spec.Image); err != nil {
			p.deps.Logger.Warnw("failed to register host in pxemanager", "name", bmh.Name, "error", err)
		}
	}

	// Configure IPMI if BMC details provided
	if bmh.Spec.BMC.Address != "" {
		user := bmh.Spec.BMC.Username
		pass := bmh.Spec.BMC.Password
		if user == "" {
			user = "ADMIN"
		}
		if pass == "" {
			pass = "ADMIN"
		}
		if err := pxeConfigureIPMI(r.Context(), p.deps.Config.BMH.PXEManagerURL, bmh.Name, bmh.Spec.BMC.Address, user, pass); err != nil {
			p.deps.Logger.Warnw("failed to configure IPMI", "name", bmh.Name, "error", err)
		}
	}

	// Sync DHCP reservations to Network CRDs (data + IPMI)
	p.syncBMHToNetwork(r.Context(), &bmh, "", "")

	podWriteJSON(w, http.StatusCreated, &bmh)
}

func (p *MicroKubeProvider) handleGetBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	bmh, ok := p.bareMetalHosts[key]
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	enriched := bmh.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}

	// Enrich with live pxemanager data
	p.enrichBMHStatus(r.Context(), enriched)

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

	items := make([]BareMetalHost, 0, len(p.bareMetalHosts))
	for _, bmh := range p.bareMetalHosts {
		enriched := bmh.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		p.enrichBMHListConcurrent(r.Context(), items)
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
	for _, bmh := range p.bareMetalHosts {
		if !bmhReferencesNetwork(bmh, ns) {
			continue
		}
		enriched := bmh.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		p.enrichBMHListConcurrent(r.Context(), items)
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

	existing, ok := p.bareMetalHosts[key]
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	oldDataNetwork := existing.Spec.Network
	oldIPMINetwork := existing.Spec.BMC.Network

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

	p.reconcileBMHChanges(r.Context(), existing, &bmh)

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, &bmh); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts[key] = &bmh

	// Sync DHCP reservations to Network CRDs (data + IPMI)
	p.syncBMHToNetwork(r.Context(), &bmh, oldDataNetwork, oldIPMINetwork)

	podWriteJSON(w, http.StatusOK, &bmh)
}

func (p *MicroKubeProvider) handlePatchBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.bareMetalHosts[key]
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	// Start from existing, overlay the patch
	oldDataNetwork := existing.Spec.Network
	oldIPMINetwork := existing.Spec.BMC.Network
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

	p.reconcileBMHChanges(r.Context(), existing, merged)

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts[key] = merged

	// Sync DHCP reservations to Network CRDs (data + IPMI)
	p.syncBMHToNetwork(r.Context(), merged, oldDataNetwork, oldIPMINetwork)

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteBMH(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	bmh, ok := p.bareMetalHosts[key]
	if !ok {
		http.Error(w, fmt.Sprintf("BareMetalHost %s not found", key), http.StatusNotFound)
		return
	}

	// Remove DHCP reservations from referenced Network CRDs (data + IPMI)
	p.removeBMHFromNetwork(r.Context(), bmh.Spec.BootMACAddress, bmh.Spec.Network)
	p.removeBMHFromNetwork(r.Context(), bmh.Spec.BMC.MAC, bmh.Spec.BMC.Network)

	delete(p.bareMetalHosts, key)

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

// ─── Reconcile spec changes → pxemanager actions ────────────────────────────

func (p *MicroKubeProvider) reconcileBMHChanges(ctx context.Context, old, new *BareMetalHost) {
	pxeURL := p.deps.Config.BMH.PXEManagerURL
	log := p.deps.Logger

	// Image changed → set_image
	if new.Spec.Image != old.Spec.Image && new.Spec.Image != "" {
		if err := pxeSetImage(ctx, pxeURL, new.Spec.BootMACAddress, new.Spec.Image); err != nil {
			log.Warnw("pxe set_image failed", "host", new.Name, "error", err)
			new.Status.ErrorMessage = fmt.Sprintf("set_image: %v", err)
		} else {
			new.Status.Phase = "Provisioning"
		}
	}

	// Online state changed → IPMI power
	if new.Spec.Online != nil && (old.Spec.Online == nil || *new.Spec.Online != *old.Spec.Online) {
		action := "power_off"
		if *new.Spec.Online {
			action = "power_on"
		}
		if err := pxeIPMIPower(ctx, pxeURL, new.Name, action); err != nil {
			log.Warnw("pxe IPMI power failed", "host", new.Name, "action", action, "error", err)
			new.Status.ErrorMessage = fmt.Sprintf("ipmi %s: %v", action, err)
		} else {
			new.Status.PoweredOn = *new.Spec.Online
			new.Status.ErrorMessage = ""
		}
	}

	// BMC config changed → configure IPMI
	if new.Spec.BMC.Address != "" && new.Spec.BMC.Address != old.Spec.BMC.Address {
		user := new.Spec.BMC.Username
		pass := new.Spec.BMC.Password
		if user == "" {
			user = "ADMIN"
		}
		if pass == "" {
			pass = "ADMIN"
		}
		if err := pxeConfigureIPMI(ctx, pxeURL, new.Name, new.Spec.BMC.Address, user, pass); err != nil {
			log.Warnw("pxe IPMI config failed", "host", new.Name, "error", err)
		}
	}
}

// ─── BMH → Network CRD Sync ─────────────────────────────────────────────────

// syncBMHToNetwork upserts DHCP reservations on the BMH's referenced Network CRDs
// (both data network and IPMI network). Old network names are used to clean up
// reservations when the network reference changes.
func (p *MicroKubeProvider) syncBMHToNetwork(ctx context.Context, bmh *BareMetalHost, oldDataNetwork, oldIPMINetwork string) {
	// Sync data network reservation (boot MAC → data network)
	if oldDataNetwork != "" && oldDataNetwork != bmh.Spec.Network {
		p.removeBMHFromNetwork(ctx, bmh.Spec.BootMACAddress, oldDataNetwork)
	}
	if bmh.Spec.Network != "" && bmh.Spec.BootMACAddress != "" {
		p.upsertNetworkReservation(ctx, bmh.Spec.Network, NetworkDHCPReservation{
			MAC:         bmh.Spec.BootMACAddress,
			IP:          bmh.Spec.IP,
			Hostname:    firstNonEmpty(bmh.Spec.Hostname, bmh.Name),
			NextServer:  bmh.Spec.NextServer,
			BootFile:    bmh.Spec.BootFile,
			BootFileEFI: bmh.Spec.BootFileEFI,
		}, bmh.Name)
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

// upsertNetworkReservation inserts or updates a DHCP reservation on a Network CRD by MAC.
func (p *MicroKubeProvider) upsertNetworkReservation(ctx context.Context, networkName string, res NetworkDHCPReservation, bmhName string) {
	log := p.deps.Logger

	net, ok := p.networks[networkName]
	if !ok {
		log.Warnw("BMH references unknown network", "bmh", bmhName, "network", networkName)
		return
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
func (p *MicroKubeProvider) removeBMHFromNetwork(ctx context.Context, mac, networkName string) {
	log := p.deps.Logger

	if mac == "" || networkName == "" {
		return
	}

	net, ok := p.networks[networkName]
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

	log.Infow("removed BMH DHCP reservation from network", "network", net.Name, "mac", mac)
}

// enrichBMHListConcurrent enriches all BMH items concurrently with a 3s overall timeout.
func (p *MicroKubeProvider) enrichBMHListConcurrent(ctx context.Context, items []BareMetalHost) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p.enrichBMHStatus(ctx, &items[idx])
		}(i)
	}
	wg.Wait()
}

// enrichBMHStatus fetches live data from pxemanager to update status fields.
func (p *MicroKubeProvider) enrichBMHStatus(ctx context.Context, bmh *BareMetalHost) {
	pxeURL := p.deps.Config.BMH.PXEManagerURL
	if pxeURL == "" || bmh.Spec.BootMACAddress == "" {
		return
	}

	host, err := pxeGetHost(ctx, pxeURL, bmh.Spec.BootMACAddress)
	if err != nil {
		return
	}

	if host.LastBoot != nil {
		bmh.Status.LastBoot = *host.LastBoot
	}
	bmh.Status.BootCount = host.BootCount
	if host.CurrentImage != "" && bmh.Spec.Image == "" {
		bmh.Spec.Image = host.CurrentImage
	}

	// Check IPMI power status
	powered, err := pxeIPMIStatus(ctx, pxeURL, bmh.Name)
	if err == nil {
		bmh.Status.PoweredOn = powered
	}
}

// ─── Table API ──────────────────────────────────────────────────────────────

func bmhListToTable(hosts []BareMetalHost) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string"},
			{Name: "Power", Type: "string"},
			{Name: "Network", Type: "string"},
			{Name: "Image", Type: "string"},
			{Name: "IP", Type: "string"},
			{Name: "MAC", Type: "string"},
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
		if image == "" {
			image = "localboot"
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

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				h.Name,
				h.Status.Phase,
				power,
				network,
				image,
				h.Status.IP,
				h.Spec.BootMACAddress,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── pxemanager Client ──────────────────────────────────────────────────────

// pxeHost is the JSON structure returned by pxemanager GET /api/hosts.
type pxeHost struct {
	MAC          string  `json:"mac"`
	Hostname     string  `json:"hostname"`
	CurrentImage string  `json:"current_image"`
	LastBoot     *string `json:"last_boot"`
	BootCount    int     `json:"boot_count"`
	IPMIIP       *string `json:"ipmi_ip"`
}

var pxeHTTPClient = &http.Client{Timeout: 10 * time.Second}

func pxeRegisterHost(ctx context.Context, pxeURL, mac, hostname, image string) error {
	if image == "" {
		image = "localboot"
	}
	body, _ := json.Marshal(map[string]string{
		"mac":           mac,
		"hostname":      hostname,
		"current_image": image,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", pxeURL+"/api/hosts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pxe register: HTTP %d", resp.StatusCode)
	}
	return nil
}

func pxeSetImage(ctx context.Context, pxeURL, mac, image string) error {
	form := fmt.Sprintf("image=%s", image)
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/host?mac=%s&action=set_image", pxeURL, mac),
		strings.NewReader(form))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pxe set_image: HTTP %d", resp.StatusCode)
	}
	return nil
}

func pxeIPMIPower(ctx context.Context, pxeURL, hostname, action string) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/host/ipmi?host=%s&action=%s", pxeURL, hostname, action), nil)
	if err != nil {
		return err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pxe ipmi %s: HTTP %d", action, resp.StatusCode)
	}
	return nil
}

func pxeIPMIStatus(ctx context.Context, pxeURL, hostname string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/host/ipmi/status?host=%s", pxeURL, hostname), nil)
	if err != nil {
		return false, err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)) == "on", nil
}

func pxeConfigureIPMI(ctx context.Context, pxeURL, hostname, ipmiIP, user, pass string) error {
	form := fmt.Sprintf("ipmi_ip=%s&ipmi_username=%s&ipmi_password=%s", ipmiIP, user, pass)
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/host/ipmi/config?host=%s", pxeURL, hostname),
		strings.NewReader(form))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pxe ipmi config: HTTP %d", resp.StatusCode)
	}
	return nil
}

func pxeGetHost(ctx context.Context, pxeURL, mac string) (*pxeHost, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", pxeURL+"/api/hosts", nil)
	if err != nil {
		return nil, err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var hosts []pxeHost
	if err := json.NewDecoder(resp.Body).Decode(&hosts); err != nil {
		return nil, err
	}

	normalizedMAC := strings.ToLower(mac)
	for _, h := range hosts {
		if strings.ToLower(h.MAC) == normalizedMAC {
			return &h, nil
		}
	}
	return nil, fmt.Errorf("host with MAC %s not found in pxemanager", mac)
}

// LoadBMHFromStore loads BMH objects from NATS store into the in-memory map.
func (p *MicroKubeProvider) LoadBMHFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.BareMetalHosts == nil {
		return
	}

	keys, err := p.deps.Store.BareMetalHosts.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list BMH from store", "error", err)
		return
	}

	for _, key := range keys {
		var bmh BareMetalHost
		if _, err := p.deps.Store.BareMetalHosts.GetJSON(ctx, key, &bmh); err != nil {
			p.deps.Logger.Warnw("failed to read BMH from store", "key", key, "error", err)
			continue
		}
		mapKey := bmh.Namespace + "/" + bmh.Name
		p.bareMetalHosts[mapKey] = &bmh
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded BMH from store", "count", len(keys))
	}
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

	// Send existing BMH objects as ADDED events first
	enc := json.NewEncoder(w)
	for _, bmh := range p.bareMetalHosts {
		if nsFilter != "" && bmh.Namespace != nsFilter {
			continue
		}
		enriched := bmh.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"}
		p.enrichBMHStatus(ctx, enriched)
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
				p.enrichBMHStatus(ctx, &bmh)
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
	return false
}
