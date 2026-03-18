package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/dns"
)

// ─── Proxy Resource Types ───────────────────────────────────────────────────
// These are kube-style wrappers around microdns resources.
// microdns is the source of truth — no NATS persistence needed.

// DNSRecord wraps a microdns DNS record.
type DNSRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSRecordSpec `json:"spec"`
}

type DNSRecordSpec struct {
	Hostname string      `json:"hostname"`
	Type     string      `json:"type"`
	Data     interface{} `json:"data"`
	TTL      int         `json:"ttl"`
	Enabled  *bool       `json:"enabled,omitempty"`
}

type DNSRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DNSRecord `json:"items"`
}

// DHCPPoolResource wraps a microdns DHCP pool.
type DHCPPoolResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DHCPPoolSpec `json:"spec"`
}

type DHCPPoolSpec struct {
	Name          string   `json:"name,omitempty"`
	RangeStart    string   `json:"rangeStart"`
	RangeEnd      string   `json:"rangeEnd"`
	Subnet        string   `json:"subnet"`
	Gateway       string   `json:"gateway"`
	DNSServers    []string `json:"dnsServers,omitempty"`
	Domain        string   `json:"domain,omitempty"`
	DomainSearch  []string `json:"domainSearch,omitempty"`
	LeaseTimeSecs int      `json:"leaseTimeSecs"`
	NextServer    string   `json:"nextServer,omitempty"`
	BootFile      string   `json:"bootFile,omitempty"`
	BootFileEFI   string   `json:"bootFileEfi,omitempty"`
	IPXEBootURL   string   `json:"ipxeBootUrl,omitempty"`
	RootPath      string   `json:"rootPath,omitempty"`
}

type DHCPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DHCPPoolResource `json:"items"`
}

// DHCPReservationResource wraps a microdns DHCP reservation.
type DHCPReservationResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DHCPReservationSpec `json:"spec"`
}

type DHCPReservationSpec struct {
	MAC         string `json:"mac"`
	IP          string `json:"ip"`
	Hostname    string `json:"hostname,omitempty"`
	NextServer  string `json:"nextServer,omitempty"`
	BootFile    string `json:"bootFile,omitempty"`
	BootFileEFI string `json:"bootFileEfi,omitempty"`
	IPXEBootURL string `json:"ipxeBootUrl,omitempty"`
	RootPath    string `json:"rootPath,omitempty"`
}

type DHCPReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DHCPReservationResource `json:"items"`
}

// DHCPLeaseResource wraps a microdns DHCP lease (read-only).
type DHCPLeaseResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DHCPLeaseSpec `json:"spec"`
}

type DHCPLeaseSpec struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac"`
	Hostname string `json:"hostname,omitempty"`
	Expires  string `json:"expires"`
	State    string `json:"state"`
	PoolID   string `json:"poolID,omitempty"`
}

type DHCPLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DHCPLeaseResource `json:"items"`
}

// DNSForwarderResource wraps a microdns DNS forwarder.
type DNSForwarderResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSForwarderSpec `json:"spec"`
}

type DNSForwarderSpec struct {
	Zone    string   `json:"zone"`
	Servers []string `json:"servers"`
}

type DNSForwarderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DNSForwarderResource `json:"items"`
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// resolveDNSEndpoint maps a namespace (network name) to a microdns REST endpoint.
// Returns the endpoint URL, the network, and true on success.
// On failure, writes an HTTP error and returns false.
func (p *MicroKubeProvider) resolveDNSEndpoint(w http.ResponseWriter, ns string) (string, *Network, bool) {
	net, ok := p.networks[ns]
	if !ok {
		http.Error(w, fmt.Sprintf("network %q not found", ns), http.StatusNotFound)
		return "", nil, false
	}
	endpoint := p.networkDNSEndpoint(net)
	if endpoint == "" {
		http.Error(w, fmt.Sprintf("network %q has no DNS endpoint configured", ns), http.StatusServiceUnavailable)
		return "", nil, false
	}
	return endpoint, net, true
}

// resolveZoneID resolves the DNS zone ID for a network, ensuring the zone exists.
func (p *MicroKubeProvider) resolveZoneID(w http.ResponseWriter, r *http.Request, endpoint string, net *Network) (string, bool) {
	zoneName := net.Spec.DNS.Zone
	if zoneName == "" {
		http.Error(w, fmt.Sprintf("network %q has no DNS zone configured", net.Name), http.StatusBadRequest)
		return "", false
	}
	zoneID, err := p.deps.NetworkMgr.DNSClient().EnsureZone(r.Context(), endpoint, zoneName)
	if err != nil {
		http.Error(w, fmt.Sprintf("resolving zone %q: %v", zoneName, err), http.StatusBadGateway)
		return "", false
	}
	return zoneID, true
}

// macToDashes converts "aa:bb:cc:dd:ee:ff" → "aa-bb-cc-dd-ee-ff"
func macToDashes(mac string) string {
	return strings.ReplaceAll(strings.ToLower(mac), ":", "-")
}

// macToColons converts "aa-bb-cc-dd-ee-ff" → "aa:bb:cc:dd:ee:ff"
func macToColons(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", ":")
}

// formatRecordData returns a human-readable string for the data column in tables.
func formatRecordData(data dns.FullRecordData) string {
	switch s := data.Data.(type) {
	case string:
		return s
	case map[string]interface{}:
		switch data.Type {
		case "MX":
			return fmt.Sprintf("%v %v", s["preference"], s["exchange"])
		case "SRV":
			return fmt.Sprintf("%v %v %v %v", s["priority"], s["weight"], s["port"], s["target"])
		case "CAA":
			return fmt.Sprintf("%v %v %v", s["flags"], s["tag"], s["value"])
		default:
			b, _ := json.Marshal(s)
			return string(b)
		}
	default:
		return fmt.Sprintf("%v", data.Data)
	}
}

// ─── DNS Record Handlers ───────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListDNSRecords(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	records, err := p.deps.NetworkMgr.DNSClient().ListFullRecords(r.Context(), endpoint, zoneID)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing DNS records: %v", err), http.StatusBadGateway)
		return
	}

	items := make([]DNSRecord, 0, len(records))
	for _, rec := range records {
		items = append(items, fullRecordToResource(rec, ns))
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dnsRecordListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, DNSRecordList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DNSRecordList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetDNSRecord(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	rec, err := p.deps.NetworkMgr.DNSClient().GetFullRecord(r.Context(), endpoint, zoneID, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("DNS record %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	res := fullRecordToResource(*rec, ns)
	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dnsRecordListToTable([]DNSRecord{res}))
		return
	}
	podWriteJSON(w, http.StatusOK, res)
}

func (p *MicroKubeProvider) handleCreateDNSRecord(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	var res DNSRecord
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	enabled := true
	if res.Spec.Enabled != nil {
		enabled = *res.Spec.Enabled
	}

	payload := map[string]interface{}{
		"name":    res.Spec.Hostname,
		"ttl":     res.Spec.TTL,
		"data":    map[string]interface{}{"type": res.Spec.Type, "data": res.Spec.Data},
		"enabled": enabled,
	}

	rec, err := p.deps.NetworkMgr.DNSClient().CreateFullRecord(r.Context(), endpoint, zoneID, payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("creating DNS record: %v", err), http.StatusBadGateway)
		return
	}

	result := fullRecordToResource(*rec, ns)
	podWriteJSON(w, http.StatusCreated, result)
}

func (p *MicroKubeProvider) handleUpdateDNSRecord(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name") // record UUID
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	var res DNSRecord
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	payload := map[string]interface{}{
		"ttl":  res.Spec.TTL,
		"data": map[string]interface{}{"type": res.Spec.Type, "data": res.Spec.Data},
	}
	if res.Spec.Enabled != nil {
		payload["enabled"] = *res.Spec.Enabled
	}

	rec, err := p.deps.NetworkMgr.DNSClient().UpdateFullRecord(r.Context(), endpoint, zoneID, name, payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("updating DNS record: %v", err), http.StatusBadGateway)
		return
	}

	result := fullRecordToResource(*rec, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handlePatchDNSRecord(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name") // record UUID
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	// GET current record from microdns
	existing, err := p.deps.NetworkMgr.DNSClient().GetFullRecord(r.Context(), endpoint, zoneID, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("DNS record %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	// Start from existing, overlay patch fields
	merged := fullRecordToResource(*existing, ns)
	if err := json.NewDecoder(r.Body).Decode(&merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	payload := map[string]interface{}{
		"ttl":  merged.Spec.TTL,
		"data": map[string]interface{}{"type": merged.Spec.Type, "data": merged.Spec.Data},
	}
	if merged.Spec.Enabled != nil {
		payload["enabled"] = *merged.Spec.Enabled
	}

	rec, err := p.deps.NetworkMgr.DNSClient().UpdateFullRecord(r.Context(), endpoint, zoneID, name, payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("patching DNS record: %v", err), http.StatusBadGateway)
		return
	}

	result := fullRecordToResource(*rec, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handleDeleteDNSRecord(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name") // record UUID
	endpoint, net, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}
	zoneID, ok := p.resolveZoneID(w, r, endpoint, net)
	if !ok {
		return
	}

	if err := p.deps.NetworkMgr.DNSClient().DeleteRecord(r.Context(), endpoint, zoneID, name); err != nil {
		http.Error(w, fmt.Sprintf("deleting DNS record: %v", err), http.StatusBadGateway)
		return
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("dnsrecord %q deleted", name),
	})
}

func fullRecordToResource(rec dns.FullRecord, ns string) DNSRecord {
	return DNSRecord{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DNSRecord"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      rec.ID,
			Namespace: ns,
		},
		Spec: DNSRecordSpec{
			Hostname: rec.Name,
			Type:     rec.Type,
			Data:     rec.Data.Data,
			TTL:      rec.TTL,
			Enabled:  &rec.Enabled,
		},
	}
}

// ─── DHCP Pool Handlers ────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListDHCPPools(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	pools, err := p.deps.NetworkMgr.DNSClient().ListDHCPPools(r.Context(), endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing DHCP pools: %v", err), http.StatusBadGateway)
		return
	}

	items := make([]DHCPPoolResource, 0, len(pools))
	for _, pool := range pools {
		items = append(items, dhcpPoolToResource(pool, ns))
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dhcpPoolListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, DHCPPoolList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPPoolList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetDHCPPool(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	pool, err := p.deps.NetworkMgr.DNSClient().GetDHCPPool(r.Context(), endpoint, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("DHCP pool %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	res := dhcpPoolToResource(*pool, ns)
	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dhcpPoolListToTable([]DHCPPoolResource{res}))
		return
	}
	podWriteJSON(w, http.StatusOK, res)
}

func (p *MicroKubeProvider) handleCreateDHCPPool(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	var res DHCPPoolResource
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	pool := dns.DHCPPool{
		Name:          res.Spec.Name,
		RangeStart:    res.Spec.RangeStart,
		RangeEnd:      res.Spec.RangeEnd,
		Subnet:        res.Spec.Subnet,
		Gateway:       res.Spec.Gateway,
		DNSServers:    res.Spec.DNSServers,
		Domain:        res.Spec.Domain,
		DomainSearch:  res.Spec.DomainSearch,
		LeaseTimeSecs: res.Spec.LeaseTimeSecs,
		NextServer:    res.Spec.NextServer,
		BootFile:      res.Spec.BootFile,
		BootFileEFI:   res.Spec.BootFileEFI,
		IPXEBootURL:   res.Spec.IPXEBootURL,
		RootPath:      res.Spec.RootPath,
	}

	created, err := p.deps.NetworkMgr.DNSClient().CreateDHCPPool(r.Context(), endpoint, pool)
	if err != nil {
		http.Error(w, fmt.Sprintf("creating DHCP pool: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpPoolToResource(*created, ns)
	podWriteJSON(w, http.StatusCreated, result)
}

func (p *MicroKubeProvider) handleUpdateDHCPPool(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	var res DHCPPoolResource
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	pool := dns.DHCPPool{
		Name:          res.Spec.Name,
		RangeStart:    res.Spec.RangeStart,
		RangeEnd:      res.Spec.RangeEnd,
		Subnet:        res.Spec.Subnet,
		Gateway:       res.Spec.Gateway,
		DNSServers:    res.Spec.DNSServers,
		Domain:        res.Spec.Domain,
		DomainSearch:  res.Spec.DomainSearch,
		LeaseTimeSecs: res.Spec.LeaseTimeSecs,
		NextServer:    res.Spec.NextServer,
		BootFile:      res.Spec.BootFile,
		BootFileEFI:   res.Spec.BootFileEFI,
		IPXEBootURL:   res.Spec.IPXEBootURL,
		RootPath:      res.Spec.RootPath,
	}

	updated, err := p.deps.NetworkMgr.DNSClient().UpdateDHCPPool(r.Context(), endpoint, name, pool)
	if err != nil {
		http.Error(w, fmt.Sprintf("updating DHCP pool: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpPoolToResource(*updated, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handlePatchDHCPPool(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	existing, err := p.deps.NetworkMgr.DNSClient().GetDHCPPool(r.Context(), endpoint, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("DHCP pool %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	// Start from existing, overlay patch fields
	merged := dhcpPoolToResource(*existing, ns)
	if err := json.NewDecoder(r.Body).Decode(&merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	pool := dns.DHCPPool{
		Name:          merged.Spec.Name,
		RangeStart:    merged.Spec.RangeStart,
		RangeEnd:      merged.Spec.RangeEnd,
		Subnet:        merged.Spec.Subnet,
		Gateway:       merged.Spec.Gateway,
		DNSServers:    merged.Spec.DNSServers,
		Domain:        merged.Spec.Domain,
		LeaseTimeSecs: merged.Spec.LeaseTimeSecs,
		NextServer:    merged.Spec.NextServer,
		BootFile:      merged.Spec.BootFile,
		BootFileEFI:   merged.Spec.BootFileEFI,
		IPXEBootURL:   merged.Spec.IPXEBootURL,
		RootPath:      merged.Spec.RootPath,
	}

	updated, err := p.deps.NetworkMgr.DNSClient().UpdateDHCPPool(r.Context(), endpoint, name, pool)
	if err != nil {
		http.Error(w, fmt.Sprintf("patching DHCP pool: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpPoolToResource(*updated, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handleDeleteDHCPPool(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	if err := p.deps.NetworkMgr.DNSClient().DeleteDHCPPool(r.Context(), endpoint, name); err != nil {
		http.Error(w, fmt.Sprintf("deleting DHCP pool: %v", err), http.StatusBadGateway)
		return
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("dhcppool %q deleted", name),
	})
}

func dhcpPoolToResource(pool dns.DHCPPool, ns string) DHCPPoolResource {
	return DHCPPoolResource{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPPool"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pool.ID,
			Namespace: ns,
		},
		Spec: DHCPPoolSpec{
			Name:          pool.Name,
			RangeStart:    pool.RangeStart,
			RangeEnd:      pool.RangeEnd,
			Subnet:        pool.Subnet,
			Gateway:       pool.Gateway,
			DNSServers:    pool.DNSServers,
			Domain:        pool.Domain,
			DomainSearch:  pool.DomainSearch,
			LeaseTimeSecs: pool.LeaseTimeSecs,
			NextServer:    pool.NextServer,
			BootFile:      pool.BootFile,
			BootFileEFI:   pool.BootFileEFI,
			IPXEBootURL:   pool.IPXEBootURL,
			RootPath:      pool.RootPath,
		},
	}
}

// ─── DHCP Reservation Handlers ─────────────────────────────────────────────

func (p *MicroKubeProvider) handleListDHCPReservations(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	reservations, err := p.deps.NetworkMgr.DNSClient().ListDHCPReservations(r.Context(), endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing DHCP reservations: %v", err), http.StatusBadGateway)
		return
	}

	items := make([]DHCPReservationResource, 0, len(reservations))
	for _, res := range reservations {
		items = append(items, dhcpReservationToResource(res, ns))
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dhcpReservationListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, DHCPReservationList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPReservationList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetDHCPReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name") // MAC with dashes
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	mac := macToColons(name)
	res, err := p.deps.NetworkMgr.DNSClient().GetDHCPReservation(r.Context(), endpoint, mac)
	if err != nil {
		http.Error(w, fmt.Sprintf("DHCP reservation %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	result := dhcpReservationToResource(*res, ns)
	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dhcpReservationListToTable([]DHCPReservationResource{result}))
		return
	}
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handleCreateDHCPReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	var res DHCPReservationResource
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	reservation := dns.DHCPReservation{
		MAC:         res.Spec.MAC,
		IP:          res.Spec.IP,
		Hostname:    res.Spec.Hostname,
		NextServer:  res.Spec.NextServer,
		BootFile:    res.Spec.BootFile,
		BootFileEFI: res.Spec.BootFileEFI,
		IPXEBootURL: res.Spec.IPXEBootURL,
		RootPath:    res.Spec.RootPath,
	}

	if err := p.deps.NetworkMgr.DNSClient().UpsertDHCPReservation(r.Context(), endpoint, reservation); err != nil {
		http.Error(w, fmt.Sprintf("creating DHCP reservation: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpReservationToResource(reservation, ns)
	podWriteJSON(w, http.StatusCreated, result)
}

func (p *MicroKubeProvider) handleUpdateDHCPReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	var res DHCPReservationResource
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	reservation := dns.DHCPReservation{
		MAC:         res.Spec.MAC,
		IP:          res.Spec.IP,
		Hostname:    res.Spec.Hostname,
		NextServer:  res.Spec.NextServer,
		BootFile:    res.Spec.BootFile,
		BootFileEFI: res.Spec.BootFileEFI,
		IPXEBootURL: res.Spec.IPXEBootURL,
		RootPath:    res.Spec.RootPath,
	}

	if err := p.deps.NetworkMgr.DNSClient().UpsertDHCPReservation(r.Context(), endpoint, reservation); err != nil {
		http.Error(w, fmt.Sprintf("updating DHCP reservation: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpReservationToResource(reservation, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handlePatchDHCPReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	mac := macToColons(name)
	existing, err := p.deps.NetworkMgr.DNSClient().GetDHCPReservation(r.Context(), endpoint, mac)
	if err != nil {
		http.Error(w, fmt.Sprintf("DHCP reservation %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	// Start from existing, overlay patch fields
	merged := dhcpReservationToResource(*existing, ns)
	if err := json.NewDecoder(r.Body).Decode(&merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	reservation := dns.DHCPReservation{
		MAC:         merged.Spec.MAC,
		IP:          merged.Spec.IP,
		Hostname:    merged.Spec.Hostname,
		NextServer:  merged.Spec.NextServer,
		BootFile:    merged.Spec.BootFile,
		BootFileEFI: merged.Spec.BootFileEFI,
		IPXEBootURL: merged.Spec.IPXEBootURL,
		RootPath:    merged.Spec.RootPath,
	}
	if err := p.deps.NetworkMgr.DNSClient().UpsertDHCPReservation(r.Context(), endpoint, reservation); err != nil {
		http.Error(w, fmt.Sprintf("patching DHCP reservation: %v", err), http.StatusBadGateway)
		return
	}

	result := dhcpReservationToResource(reservation, ns)
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handleDeleteDHCPReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	mac := macToColons(name)
	if err := p.deps.NetworkMgr.DNSClient().DeleteDHCPReservation(r.Context(), endpoint, mac); err != nil {
		http.Error(w, fmt.Sprintf("deleting DHCP reservation: %v", err), http.StatusBadGateway)
		return
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("dhcpreservation %q deleted", name),
	})
}

func dhcpReservationToResource(res dns.DHCPReservation, ns string) DHCPReservationResource {
	return DHCPReservationResource{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPReservation"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      macToDashes(res.MAC),
			Namespace: ns,
		},
		Spec: DHCPReservationSpec{
			MAC:         res.MAC,
			IP:          res.IP,
			Hostname:    res.Hostname,
			NextServer:  res.NextServer,
			BootFile:    res.BootFile,
			BootFileEFI: res.BootFileEFI,
			IPXEBootURL: res.IPXEBootURL,
			RootPath:    res.RootPath,
		},
	}
}

// ─── DHCP Lease Handlers (read-only) ───────────────────────────────────────

func (p *MicroKubeProvider) handleListDHCPLeases(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	leases, err := p.deps.NetworkMgr.DNSClient().ListDHCPLeases(r.Context(), endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing DHCP leases: %v", err), http.StatusBadGateway)
		return
	}

	items := make([]DHCPLeaseResource, 0, len(leases))
	for _, lease := range leases {
		items = append(items, dhcpLeaseToResource(lease, ns))
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dhcpLeaseListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, DHCPLeaseList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPLeaseList"},
		Items:    items,
	})
}

func dhcpLeaseToResource(lease dns.DHCPLease, ns string) DHCPLeaseResource {
	// Parse lease_end to show as human-readable expiry
	expires := lease.LeaseEnd
	if t, err := time.Parse(time.RFC3339Nano, lease.LeaseEnd); err == nil {
		remaining := time.Until(t)
		if remaining > 0 {
			expires = formatAge(remaining)
		} else {
			expires = "expired"
		}
	}

	return DHCPLeaseResource{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DHCPLease"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      lease.ID,
			Namespace: ns,
		},
		Spec: DHCPLeaseSpec{
			IP:       lease.IP,
			MAC:      lease.MAC,
			Hostname: lease.Hostname,
			Expires:  expires,
			State:    lease.State,
			PoolID:   lease.PoolID,
		},
	}
}

// ─── DNS Forwarder Handlers ────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListDNSForwarders(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	forwarders, err := p.deps.NetworkMgr.DNSClient().ListDNSForwarders(r.Context(), endpoint)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing DNS forwarders: %v", err), http.StatusBadGateway)
		return
	}

	items := make([]DNSForwarderResource, 0, len(forwarders))
	for _, fwd := range forwarders {
		items = append(items, dnsForwarderToResource(fwd, ns))
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dnsForwarderListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, DNSForwarderList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DNSForwarderList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetDNSForwarder(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	fwd, err := p.deps.NetworkMgr.DNSClient().GetDNSForwarder(r.Context(), endpoint, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("DNS forwarder %q not found: %v", name, err), http.StatusNotFound)
		return
	}

	result := dnsForwarderToResource(*fwd, ns)
	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, dnsForwarderListToTable([]DNSForwarderResource{result}))
		return
	}
	podWriteJSON(w, http.StatusOK, result)
}

func (p *MicroKubeProvider) handleCreateDNSForwarder(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	var res DNSForwarderResource
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	fwd := dns.DNSForwarder{
		Zone:    res.Spec.Zone,
		Servers: res.Spec.Servers,
	}

	if err := p.deps.NetworkMgr.DNSClient().EnsureDNSForwarder(r.Context(), endpoint, fwd); err != nil {
		http.Error(w, fmt.Sprintf("creating DNS forwarder: %v", err), http.StatusBadGateway)
		return
	}

	result := dnsForwarderToResource(fwd, ns)
	podWriteJSON(w, http.StatusCreated, result)
}

func (p *MicroKubeProvider) handleDeleteDNSForwarder(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	endpoint, _, ok := p.resolveDNSEndpoint(w, ns)
	if !ok {
		return
	}

	if err := p.deps.NetworkMgr.DNSClient().DeleteDNSForwarder(r.Context(), endpoint, name); err != nil {
		http.Error(w, fmt.Sprintf("deleting DNS forwarder: %v", err), http.StatusBadGateway)
		return
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("dnsforwarder %q deleted", name),
	})
}

func dnsForwarderToResource(fwd dns.DNSForwarder, ns string) DNSForwarderResource {
	return DNSForwarderResource{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DNSForwarder"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fwd.Zone,
			Namespace: ns,
		},
		Spec: DNSForwarderSpec{
			Zone:    fwd.Zone,
			Servers: fwd.Servers,
		},
	}
}

// ─── Table Formatters ──────────────────────────────────────────────────────

func dnsRecordListToTable(records []DNSRecord) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Hostname", Type: "string"},
			{Name: "Type", Type: "string"},
			{Name: "Data", Type: "string"},
			{Name: "TTL", Type: "integer"},
			{Name: "Enabled", Type: "string"},
		},
	}

	for i := range records {
		rec := &records[i]
		enabled := "true"
		if rec.Spec.Enabled != nil && !*rec.Spec.Enabled {
			enabled = "false"
		}

		dataStr := fmt.Sprintf("%v", rec.Spec.Data)

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":      rec.Name,
				"namespace": rec.Namespace,
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []interface{}{rec.Name, rec.Spec.Hostname, rec.Spec.Type, dataStr, rec.Spec.TTL, enabled},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

func dhcpPoolListToTable(pools []DHCPPoolResource) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Pool", Type: "string"},
			{Name: "Subnet", Type: "string"},
			{Name: "Range", Type: "string"},
			{Name: "Gateway", Type: "string"},
			{Name: "Domain", Type: "string"},
			{Name: "Lease", Type: "string"},
		},
	}

	for i := range pools {
		pool := &pools[i]
		poolName := pool.Spec.Name
		if poolName == "" {
			poolName = pool.Name
		}
		rangeStr := pool.Spec.RangeStart + " - " + pool.Spec.RangeEnd
		lease := fmt.Sprintf("%ds", pool.Spec.LeaseTimeSecs)

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":      pool.Name,
				"namespace": pool.Namespace,
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []interface{}{poolName, pool.Name, pool.Spec.Subnet, rangeStr, pool.Spec.Gateway, pool.Spec.Domain, lease},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

func dhcpReservationListToTable(reservations []DHCPReservationResource) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "MAC", Type: "string"},
			{Name: "IP", Type: "string"},
			{Name: "Hostname", Type: "string"},
			{Name: "PXE", Type: "string"},
		},
	}

	for i := range reservations {
		res := &reservations[i]
		pxe := "-"
		if res.Spec.NextServer != "" {
			pxe = res.Spec.NextServer
			if res.Spec.BootFile != "" {
				pxe += " " + res.Spec.BootFile
			}
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":      res.Name,
				"namespace": res.Namespace,
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []interface{}{res.Name, res.Spec.MAC, res.Spec.IP, res.Spec.Hostname, pxe},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

func dhcpLeaseListToTable(leases []DHCPLeaseResource) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "IP", Type: "string"},
			{Name: "MAC", Type: "string"},
			{Name: "Hostname", Type: "string"},
			{Name: "Expires", Type: "string"},
			{Name: "State", Type: "string"},
		},
	}

	for i := range leases {
		lease := &leases[i]
		hostname := lease.Spec.Hostname
		if hostname == "" {
			hostname = "-"
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":      lease.Name,
				"namespace": lease.Namespace,
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []interface{}{lease.Name, lease.Spec.IP, lease.Spec.MAC, hostname, lease.Spec.Expires, lease.Spec.State},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

func dnsForwarderListToTable(forwarders []DNSForwarderResource) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{APIVersion: "meta.k8s.io/v1", Kind: "Table"},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Zone", Type: "string"},
			{Name: "Servers", Type: "string"},
		},
	}

	for i := range forwarders {
		fwd := &forwarders[i]
		servers := strings.Join(fwd.Spec.Servers, ", ")

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":      fwd.Name,
				"namespace": fwd.Namespace,
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []interface{}{fwd.Name, fwd.Spec.Zone, servers},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}
