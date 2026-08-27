package dns

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

	"go.uber.org/zap"
)

// ─── Flexible Record Types (all record types, not just A) ───────────────────

// FullRecordData is like RecordData but handles all record types (MX, SRV, CAA, etc.)
// Data is a string for simple types (A, AAAA, CNAME, NS, PTR, TXT) and an object for complex types.
type FullRecordData struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// FullRecord is a DNS record that supports all record data types.
type FullRecord struct {
	ID        string         `json:"id"`
	ZoneID    string         `json:"zone_id,omitempty"`
	Name      string         `json:"name"`
	TTL       int            `json:"ttl"`
	Type      string         `json:"type"`
	Data      FullRecordData `json:"data"`
	Enabled   bool           `json:"enabled"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

// DHCPLease represents an active DHCP lease from a microdns instance.
type DHCPLease struct {
	ID         string `json:"id"`
	IP         string `json:"ip_addr"`
	MAC        string `json:"mac_addr"`
	Hostname   string `json:"hostname"`
	LeaseStart string `json:"lease_start"`
	LeaseEnd   string `json:"lease_end"`
	PoolID     string `json:"pool_id"`
	State      string `json:"state"`
}

// Client is a REST client for MicroDNS instances.
// It is stateless per call — each call takes the endpoint URL as a parameter,
// so a single client can talk to multiple MicroDNS servers.
//
// Operations gate on a TTL-cached GET /api/v1/health alive probe
// (endpointAlive), so a dead instance costs one short probe per TTL
// window instead of a full HTTP timeout on every call.
type Client struct {
	http *http.Client
	log  *zap.SugaredLogger

	mu     sync.Mutex
	health map[string]endpointHealth // endpoint -> last alive-probe result
}

// endpointHealth is the cached result of the last /api/v1/health probe.
type endpointHealth struct {
	alive     bool
	checkedAt time.Time
}

const (
	healthTTL          = 15 * time.Second
	healthProbeTimeout = 1500 * time.Millisecond
)

// Zone represents a MicroDNS zone.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RecordData represents the typed data payload in a MicroDNS record.
type RecordData struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// UnmarshalJSON accepts both payload shapes microdns serves: a string for
// A/AAAA/CNAME/NS/PTR/TXT, and an object for MX/SRV/CAA. Decoding the object
// shapes into a string used to fail the ENTIRE record listing, which broke
// every stale-record cleanup and BMH DNS sync against any zone holding one
// such record — thousands of warn lines per hour, and stale records never
// cleaned. Object payloads are preserved as their compact JSON; consumers
// that match A-record strings simply never match them.
func (d *RecordData) UnmarshalJSON(b []byte) error {
	var probe struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	d.Type = probe.Type
	var s string
	if err := json.Unmarshal(probe.Data, &s); err == nil {
		d.Data = s
		return nil
	}
	d.Data = string(probe.Data)
	return nil
}

// Record represents a MicroDNS DNS record.
type Record struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	Type string     `json:"type"`
	Data RecordData `json:"data"`
	TTL  int        `json:"ttl"`
}

type createZoneRequest struct {
	Name string `json:"name"`
}

type createRecordRequest struct {
	Name string     `json:"name"`
	TTL  int        `json:"ttl"`
	Data RecordData `json:"data"`
}

// NewClient creates a new MicroDNS REST client.
func NewClient(log *zap.SugaredLogger) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 2,
				MaxConnsPerHost:     4,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		log:    log,
		health: make(map[string]endpointHealth),
	}
}

// endpointAlive reports whether the microdns instance at endpoint is
// serving its API. microdns is database-driven, so a 200 from
// /api/v1/health means the instance and its database are usable.
// Results are cached for healthTTL; only up/down transitions are logged.
func (c *Client) endpointAlive(ctx context.Context, endpoint string) error {
	c.mu.Lock()
	h, cached := c.health[endpoint]
	if cached && time.Since(h.checkedAt) < healthTTL {
		c.mu.Unlock()
		if h.alive {
			return nil
		}
		return fmt.Errorf("endpoint %s failed last health probe, skipping", endpoint)
	}
	c.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()

	var probeErr error
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint+"/api/v1/health", nil)
	if err != nil {
		probeErr = err
	} else if resp, doErr := c.http.Do(req); doErr != nil {
		probeErr = doErr
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			probeErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	alive := probeErr == nil
	c.mu.Lock()
	prev, had := c.health[endpoint]
	c.health[endpoint] = endpointHealth{alive: alive, checkedAt: time.Now()}
	c.mu.Unlock()

	switch {
	case !alive && (!had || prev.alive):
		c.log.Warnw("microdns endpoint down", "endpoint", endpoint, "error", probeErr)
	case alive && had && !prev.alive:
		c.log.Infow("microdns endpoint recovered", "endpoint", endpoint)
	}

	if !alive {
		return fmt.Errorf("endpoint %s not alive: %v", endpoint, probeErr)
	}
	return nil
}

// EnsureZone finds an existing zone by name or creates it.
// Returns the zone UUID.
func (c *Client) EnsureZone(ctx context.Context, endpoint, zoneName string) (string, error) {
	if err := c.endpointAlive(ctx, endpoint); err != nil {
		return "", err
	}

	// List existing zones
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/zones", nil)
	if err != nil {
		return "", fmt.Errorf("building zone list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("listing zones from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading zone list response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("listing zones: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var zones []Zone
	if err := json.Unmarshal(body, &zones); err != nil {
		return "", fmt.Errorf("decoding zones: %w", err)
	}

	// Find existing zone
	for _, z := range zones {
		if z.Name == zoneName {
			c.log.Debugw("found existing zone", "zone", zoneName, "id", z.ID, "endpoint", endpoint)
			return z.ID, nil
		}
	}

	// Create zone
	c.log.Infow("creating zone", "zone", zoneName, "endpoint", endpoint)
	payload, _ := json.Marshal(createZoneRequest{Name: zoneName})

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/v1/zones", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building zone create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp2, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating zone %s at %s: %w", zoneName, endpoint, err)
	}
	defer resp2.Body.Close()

	body, err = io.ReadAll(resp2.Body)
	if err != nil {
		return "", fmt.Errorf("reading zone create response: %w", err)
	}

	if resp2.StatusCode != http.StatusCreated && resp2.StatusCode != http.StatusOK {
		return "", fmt.Errorf("creating zone: HTTP %d: %s", resp2.StatusCode, string(body))
	}

	var created Zone
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("decoding created zone: %w", err)
	}

	c.log.Infow("zone created", "zone", zoneName, "id", created.ID, "endpoint", endpoint)
	return created.ID, nil
}

// RegisterHost creates an A record in the specified zone.
// It is idempotent: if a matching A record (same hostname + IP) already
// exists, the call is a no-op.
func (c *Client) RegisterHost(ctx context.Context, endpoint, zoneID, hostname, ip string, ttl int) error {
	if err := c.endpointAlive(ctx, endpoint); err != nil {
		return err
	}

	// Check for existing record to avoid creating duplicates.
	records, err := c.ListRecords(ctx, endpoint, zoneID)
	if err == nil {
		for _, r := range records {
			if r.Type == "A" && r.Name == hostname && r.Data.Data == ip {
				return nil
			}
		}
	}

	payload, _ := json.Marshal(createRecordRequest{
		Name: hostname,
		TTL:  ttl,
		Data: RecordData{Type: "A", Data: ip},
	})

	url := fmt.Sprintf("%s/api/v1/zones/%s/records", endpoint, zoneID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registering host %s in zone %s: %w", hostname, zoneID, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registering host: HTTP %d: %s", resp.StatusCode, string(body))
	}

	c.log.Infow("DNS record registered", "hostname", hostname, "ip", ip, "zone", zoneID)
	return nil
}

// DeleteRecord removes a single DNS record by its ID.
func (c *Client) DeleteRecord(ctx context.Context, endpoint, zoneID, recordID string) error {
	url := fmt.Sprintf("%s/api/v1/zones/%s/records/%s", endpoint, zoneID, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("building delete request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleting record %s: %w", recordID, err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting record %s: HTTP %d", recordID, resp.StatusCode)
	}
	return nil
}

// ListRecords returns all DNS records in a zone.
func (c *Client) ListRecords(ctx context.Context, endpoint, zoneID string) ([]Record, error) {
	if err := c.endpointAlive(ctx, endpoint); err != nil {
		return nil, err
	}

	var all []Record
	const pageSize = 100
	offset := 0

	for {
		url := fmt.Sprintf("%s/api/v1/zones/%s/records?limit=%d&offset=%d", endpoint, zoneID, pageSize, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("building record list request: %w", err)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing records in zone %s: %w", zoneID, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading record list response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing records: HTTP %d: %s", resp.StatusCode, string(body))
		}

		var page []Record
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding records: %w", err)
		}

		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}

	return all, nil
}

// DeregisterHost removes all A records matching the given hostname from a zone.
func (c *Client) DeregisterHost(ctx context.Context, endpoint, zoneID, hostname string) error {
	records, err := c.ListRecords(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Name == hostname && r.Type == "A" {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				return err
			}
			c.log.Infow("DNS record deregistered", "hostname", hostname, "record_id", r.ID, "zone", zoneID)
		}
	}
	return nil
}

// DeregisterHostByIP removes A records matching both hostname and IP from a zone.
// Unlike DeregisterHost which removes all A records for a hostname, this only
// removes the specific record for one IP — used for cleaning up pod-level
// round-robin records without removing other containers' entries.
func (c *Client) DeregisterHostByIP(ctx context.Context, endpoint, zoneID, hostname, ip string) error {
	records, err := c.ListRecords(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Name == hostname && r.Type == "A" && r.Data.Data == ip {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				return err
			}
			c.log.Infow("DNS record deregistered by IP", "hostname", hostname, "ip", ip, "record_id", r.ID, "zone", zoneID)
		}
	}
	return nil
}

// CleanStaleRecords removes A records for a hostname where the IP doesn't
// match the given current IP. Used to clean up stale records when a pod
// gets a new IP on recreation.
func (c *Client) CleanStaleRecords(ctx context.Context, endpoint, zoneID, hostname, currentIP string) error {
	records, err := c.ListRecords(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.Name == hostname && r.Type == "A" && r.Data.Data != currentIP {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				c.log.Warnw("failed to delete stale DNS record",
					"hostname", hostname, "stale_ip", r.Data.Data,
					"current_ip", currentIP, "error", err)
			} else {
				c.log.Infow("removed stale DNS record",
					"hostname", hostname, "stale_ip", r.Data.Data,
					"current_ip", currentIP)
			}
		}
	}
	return nil
}

// ─── DHCP Pool Types & Methods ──────────────────────────────────────────────

// DHCPPool represents a DHCP pool in a microdns instance.
type DHCPPool struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	RangeStart    string   `json:"range_start"`
	RangeEnd      string   `json:"range_end"`
	Subnet        string   `json:"subnet"`
	Gateway       string   `json:"gateway"`
	DNSServers    []string `json:"dns_servers"`
	Domain        string   `json:"domain"`
	DomainSearch  []string `json:"domain_search,omitempty"`
	LeaseTimeSecs int      `json:"lease_time_secs"`
	NextServer    string   `json:"next_server,omitempty"`
	BootFile      string   `json:"boot_file,omitempty"`
	BootFileEFI   string   `json:"boot_file_efi,omitempty"`
	IPXEBootURL   string   `json:"ipxe_boot_url,omitempty"`
	RootPath      string   `json:"root_path,omitempty"`
	NTPServers    []string `json:"ntp_servers,omitempty"`
}

// ListDHCPPools returns all DHCP pools from a microdns instance.
func (c *Client) ListDHCPPools(ctx context.Context, endpoint string) ([]DHCPPool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dhcp/pools", nil)
	if err != nil {
		return nil, fmt.Errorf("building pool list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing DHCP pools from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading pool list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing DHCP pools: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var pools []DHCPPool
	if err := json.Unmarshal(body, &pools); err != nil {
		return nil, fmt.Errorf("decoding DHCP pools: %w", err)
	}
	return pools, nil
}

// CreateDHCPPool creates a DHCP pool on a microdns instance.
func (c *Client) CreateDHCPPool(ctx context.Context, endpoint string, pool DHCPPool) (*DHCPPool, error) {
	payload, _ := json.Marshal(pool)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/v1/dhcp/pools", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building pool create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating DHCP pool at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading pool create response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("creating DHCP pool: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var created DHCPPool
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("decoding created pool: %w", err)
	}

	c.log.Infow("DHCP pool created", "name", created.Name, "subnet", created.Subnet, "endpoint", endpoint)
	return &created, nil
}

// DeleteDHCPPool removes a DHCP pool by ID.
func (c *Client) DeleteDHCPPool(ctx context.Context, endpoint, poolID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint+"/api/v1/dhcp/pools/"+poolID, nil)
	if err != nil {
		return fmt.Errorf("building pool delete request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleting DHCP pool %s: %w", poolID, err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting DHCP pool %s: HTTP %d", poolID, resp.StatusCode)
	}
	return nil
}

// ─── DHCP Reservation Types & Methods ───────────────────────────────────────

// DHCPReservation represents a static DHCP reservation in a microdns instance.
type DHCPReservation struct {
	MAC         string   `json:"mac"`
	IP          string   `json:"ip"`
	Hostname    string   `json:"hostname,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	DNSServers  []string `json:"dns_servers,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	NextServer  string   `json:"next_server,omitempty"`
	BootFile    string   `json:"boot_file,omitempty"`
	BootFileEFI string   `json:"boot_file_efi,omitempty"`
	IPXEBootURL string   `json:"ipxe_boot_url,omitempty"`
	RootPath    *string  `json:"root_path"`
}

// StringPtr returns a pointer to s. Used for fields like RootPath
// where the pointer distinguishes "set to empty" from "not present".
// An empty string serializes as "" in JSON, which tells microdns to
// suppress the pool default (e.g. no iSCSI root_path for localboot).
func StringPtr(s string) *string {
	return &s
}

// ListDHCPReservations returns all DHCP reservations from a microdns instance.
func (c *Client) ListDHCPReservations(ctx context.Context, endpoint string) ([]DHCPReservation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dhcp/reservations", nil)
	if err != nil {
		return nil, fmt.Errorf("building reservation list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing DHCP reservations from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading reservation list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing DHCP reservations: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var reservations []DHCPReservation
	if err := json.Unmarshal(body, &reservations); err != nil {
		return nil, fmt.Errorf("decoding DHCP reservations: %w", err)
	}
	return reservations, nil
}

// UpsertDHCPReservation creates or updates a DHCP reservation by MAC.
// If the MAC already exists (409 Conflict on POST), it PATCHes the existing entry.
func (c *Client) UpsertDHCPReservation(ctx context.Context, endpoint string, res DHCPReservation) error {
	payload, _ := json.Marshal(res)

	// Try POST first
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/v1/dhcp/reservations", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building reservation create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("creating DHCP reservation at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		c.log.Infow("DHCP reservation created", "mac", res.MAC, "ip", res.IP, "endpoint", endpoint)
		return nil
	}

	if resp.StatusCode == http.StatusConflict {
		// MAC exists — DELETE then re-POST for a full replace.
		// PATCH can't clear nullable fields (serde Option<Option<T>> ignores JSON null).
		mac := strings.ToLower(res.MAC)
		delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete,
			endpoint+"/api/v1/dhcp/reservations/"+mac, nil)
		if err != nil {
			return fmt.Errorf("building reservation delete request: %w", err)
		}
		delResp, err := c.http.Do(delReq)
		if err != nil {
			return fmt.Errorf("deleting DHCP reservation %s for re-create: %w", mac, err)
		}
		delResp.Body.Close()

		// Re-POST with full reservation
		createReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			endpoint+"/api/v1/dhcp/reservations", bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("building reservation re-create request: %w", err)
		}
		createReq.Header.Set("Content-Type", "application/json")

		createResp, err := c.http.Do(createReq)
		if err != nil {
			return fmt.Errorf("re-creating DHCP reservation %s: %w", mac, err)
		}
		defer createResp.Body.Close()
		body, _ := io.ReadAll(createResp.Body)

		if createResp.StatusCode != http.StatusCreated && createResp.StatusCode != http.StatusOK {
			return fmt.Errorf("re-creating DHCP reservation: HTTP %d: %s", createResp.StatusCode, string(body))
		}
		c.log.Debugw("DHCP reservation replaced", "mac", res.MAC, "ip", res.IP, "endpoint", endpoint)
		return nil
	}

	return fmt.Errorf("creating DHCP reservation: HTTP %d", resp.StatusCode)
}

// DeleteDHCPReservation removes a DHCP reservation by MAC address.
func (c *Client) DeleteDHCPReservation(ctx context.Context, endpoint, mac string) error {
	mac = strings.ToLower(mac)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		endpoint+"/api/v1/dhcp/reservations/"+mac, nil)
	if err != nil {
		return fmt.Errorf("building reservation delete request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleting DHCP reservation %s: %w", mac, err)
	}
	resp.Body.Close()

	// 404 is OK — reservation already gone
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting DHCP reservation %s: HTTP %d", mac, resp.StatusCode)
	}
	c.log.Infow("DHCP reservation deleted", "mac", mac, "endpoint", endpoint)
	return nil
}

// ─── DNS Forwarder Types & Methods ──────────────────────────────────────────

// DNSForwarder represents a forward zone in a microdns instance.
type DNSForwarder struct {
	Zone    string   `json:"zone"`
	Servers []string `json:"servers"`
}

// ListDNSForwarders returns all DNS forwarders from a microdns instance.
func (c *Client) ListDNSForwarders(ctx context.Context, endpoint string) ([]DNSForwarder, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dns/forwarders", nil)
	if err != nil {
		return nil, fmt.Errorf("building forwarder list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing DNS forwarders from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading forwarder list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing DNS forwarders: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var forwarders []DNSForwarder
	if err := json.Unmarshal(body, &forwarders); err != nil {
		return nil, fmt.Errorf("decoding DNS forwarders: %w", err)
	}
	return forwarders, nil
}

// EnsureDNSForwarder creates a DNS forwarder if it doesn't exist.
// Returns nil on success or if the forwarder already exists (409).
func (c *Client) EnsureDNSForwarder(ctx context.Context, endpoint string, fwd DNSForwarder) error {
	payload, _ := json.Marshal(fwd)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/v1/dns/forwarders", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building forwarder create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("creating DNS forwarder at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 201 Created or 409 Conflict (already exists) are both success
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		if resp.StatusCode != http.StatusConflict {
			c.log.Infow("DNS forwarder created", "zone", fwd.Zone, "servers", fwd.Servers, "endpoint", endpoint)
		}
		return nil
	}

	return fmt.Errorf("creating DNS forwarder: HTTP %d: %s", resp.StatusCode, string(body))
}

// DeleteDNSForwarder removes a DNS forwarder by zone name.
func (c *Client) DeleteDNSForwarder(ctx context.Context, endpoint, zone string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		endpoint+"/api/v1/dns/forwarders/"+zone, nil)
	if err != nil {
		return fmt.Errorf("building forwarder delete request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deleting DNS forwarder %s: %w", zone, err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting DNS forwarder %s: HTTP %d", zone, resp.StatusCode)
	}
	return nil
}

// ─── Full Record Methods (proxy support) ────────────────────────────────────

// ListFullRecords returns all DNS records in a zone with full type support.
func (c *Client) ListFullRecords(ctx context.Context, endpoint, zoneID string) ([]FullRecord, error) {
	var all []FullRecord
	const pageSize = 100
	offset := 0

	for {
		url := fmt.Sprintf("%s/api/v1/zones/%s/records?limit=%d&offset=%d", endpoint, zoneID, pageSize, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("building record list request: %w", err)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing records in zone %s: %w", zoneID, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading record list response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing records: HTTP %d: %s", resp.StatusCode, string(body))
		}

		var page []FullRecord
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding records: %w", err)
		}

		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}

	return all, nil
}

// GetFullRecord returns a single DNS record by ID with full type support.
func (c *Client) GetFullRecord(ctx context.Context, endpoint, zoneID, recordID string) (*FullRecord, error) {
	url := fmt.Sprintf("%s/api/v1/zones/%s/records/%s", endpoint, zoneID, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building record get request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting record %s: %w", recordID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading record response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("record %s not found", recordID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting record: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rec FullRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decoding record: %w", err)
	}
	return &rec, nil
}

// CreateFullRecord creates a DNS record with full type support.
// The payload should contain name, ttl, data (RecordData object), and optionally enabled.
func (c *Client) CreateFullRecord(ctx context.Context, endpoint, zoneID string, payload interface{}) (*FullRecord, error) {
	data, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/v1/zones/%s/records", endpoint, zoneID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("building record create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating record: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading create response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("creating record: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rec FullRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decoding created record: %w", err)
	}
	return &rec, nil
}

// UpdateFullRecord updates a DNS record by ID with full type support.
func (c *Client) UpdateFullRecord(ctx context.Context, endpoint, zoneID, recordID string, payload interface{}) (*FullRecord, error) {
	data, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/v1/zones/%s/records/%s", endpoint, zoneID, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("building record update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updating record %s: %w", recordID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading update response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updating record: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rec FullRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decoding updated record: %w", err)
	}
	return &rec, nil
}

// ─── DHCP Lease Methods ─────────────────────────────────────────────────────

// ListDHCPLeases returns all active DHCP leases from a microdns instance.
func (c *Client) ListDHCPLeases(ctx context.Context, endpoint string) ([]DHCPLease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/leases", nil)
	if err != nil {
		return nil, fmt.Errorf("building lease list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing DHCP leases from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading lease list response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing DHCP leases: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var leases []DHCPLease
	if err := json.Unmarshal(body, &leases); err != nil {
		return nil, fmt.Errorf("decoding DHCP leases: %w", err)
	}
	return leases, nil
}

// ─── Single-Resource GET Methods (proxy support) ────────────────────────────

// GetDHCPPool returns a single DHCP pool by ID.
func (c *Client) GetDHCPPool(ctx context.Context, endpoint, poolID string) (*DHCPPool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dhcp/pools/"+poolID, nil)
	if err != nil {
		return nil, fmt.Errorf("building pool get request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting DHCP pool %s: %w", poolID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading pool response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("DHCP pool %s not found", poolID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting DHCP pool: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var pool DHCPPool
	if err := json.Unmarshal(body, &pool); err != nil {
		return nil, fmt.Errorf("decoding DHCP pool: %w", err)
	}
	return &pool, nil
}

// UpdateDHCPPool updates a DHCP pool by ID.
func (c *Client) UpdateDHCPPool(ctx context.Context, endpoint, poolID string, pool DHCPPool) (*DHCPPool, error) {
	payload, _ := json.Marshal(pool)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint+"/api/v1/dhcp/pools/"+poolID, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building pool update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updating DHCP pool %s: %w", poolID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading pool update response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updating DHCP pool: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var updated DHCPPool
	if err := json.Unmarshal(body, &updated); err != nil {
		return nil, fmt.Errorf("decoding updated pool: %w", err)
	}
	return &updated, nil
}

// GetDHCPReservation returns a single DHCP reservation by MAC address.
func (c *Client) GetDHCPReservation(ctx context.Context, endpoint, mac string) (*DHCPReservation, error) {
	mac = strings.ToLower(mac)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dhcp/reservations/"+mac, nil)
	if err != nil {
		return nil, fmt.Errorf("building reservation get request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting DHCP reservation %s: %w", mac, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading reservation response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("DHCP reservation %s not found", mac)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting DHCP reservation: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res DHCPReservation
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decoding DHCP reservation: %w", err)
	}
	return &res, nil
}

// GetDNSForwarder returns a single DNS forwarder by zone name.
func (c *Client) GetDNSForwarder(ctx context.Context, endpoint, zone string) (*DNSForwarder, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/dns/forwarders/"+zone, nil)
	if err != nil {
		return nil, fmt.Errorf("building forwarder get request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting DNS forwarder %s: %w", zone, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading forwarder response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("DNS forwarder %s not found", zone)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting DNS forwarder: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var fwd DNSForwarder
	if err := json.Unmarshal(body, &fwd); err != nil {
		return nil, fmt.Errorf("decoding DNS forwarder: %w", err)
	}
	return &fwd, nil
}

// ─── Health Check ───────────────────────────────────────────────────────────

// HealthCheck probes a microdns REST API health endpoint.
// Returns nil if the instance is healthy, error otherwise.
func (c *Client) HealthCheck(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/zones", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Close cleans up the client.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}
