package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Client is a REST client for MicroDNS instances.
// It is stateless — each call takes the endpoint URL as a parameter,
// so a single client can talk to multiple MicroDNS servers.
//
// Use BeginBatch/EndBatch to cache record lists across multiple calls,
// avoiding repeated HTTP GETs of the same zone in a single reconcile.
// During batch mode, endpoints that return errors are blacklisted for
// the remainder of the batch to avoid repeated 10s timeouts.
type Client struct {
	http *http.Client
	log  *zap.SugaredLogger

	mu             sync.Mutex
	batchMode      bool
	recordCache    map[string][]Record // "endpoint:zoneID" -> records
	failedEndpoints map[string]bool    // endpoints that timed out this batch
}

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
		http: &http.Client{Timeout: 3 * time.Second},
		log:  log,
	}
}

// BeginBatch enables record caching. While batch mode is active,
// ListRecords results are cached per zone and reused by RegisterHost,
// CleanStaleRecords, etc. This avoids O(N) HTTP GETs during reconcile.
// Call EndBatch when done to release the cache.
func (c *Client) BeginBatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batchMode = true
	c.recordCache = make(map[string][]Record)
	c.failedEndpoints = make(map[string]bool)
}

// EndBatch disables record caching and clears the cache.
func (c *Client) EndBatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batchMode = false
	c.recordCache = nil
	c.failedEndpoints = nil
}

// invalidateCache removes the cached records for a zone.
// Called after mutations (create, delete) to keep the cache consistent.
func (c *Client) invalidateCache(endpoint, zoneID string) {
	if c.recordCache != nil {
		delete(c.recordCache, endpoint+":"+zoneID)
	}
}

// listRecordsCached returns cached records if in batch mode, otherwise fetches fresh.
// In batch mode, endpoints that previously failed are skipped immediately.
func (c *Client) listRecordsCached(ctx context.Context, endpoint, zoneID string) ([]Record, error) {
	c.mu.Lock()
	if c.batchMode {
		// Skip endpoints that already failed this batch
		if c.failedEndpoints[endpoint] {
			c.mu.Unlock()
			return nil, fmt.Errorf("endpoint %s previously failed this batch, skipping", endpoint)
		}
		key := endpoint + ":" + zoneID
		if cached, ok := c.recordCache[key]; ok {
			c.mu.Unlock()
			return cached, nil
		}
		c.mu.Unlock()
		// Fetch and cache
		records, err := c.ListRecords(ctx, endpoint, zoneID)
		if err != nil {
			// Blacklist endpoint for remainder of this batch
			c.mu.Lock()
			if c.failedEndpoints != nil {
				c.failedEndpoints[endpoint] = true
			}
			c.mu.Unlock()
			return nil, err
		}
		c.mu.Lock()
		if c.recordCache != nil {
			c.recordCache[key] = records
		}
		c.mu.Unlock()
		return records, nil
	}
	c.mu.Unlock()
	return c.ListRecords(ctx, endpoint, zoneID)
}

// EnsureZone finds an existing zone by name or creates it.
// Returns the zone UUID.
func (c *Client) EnsureZone(ctx context.Context, endpoint, zoneName string) (string, error) {
	// Skip endpoints known to be unreachable this batch
	c.mu.Lock()
	if c.batchMode && c.failedEndpoints[endpoint] {
		c.mu.Unlock()
		return "", fmt.Errorf("endpoint %s previously failed this batch, skipping", endpoint)
	}
	c.mu.Unlock()

	// List existing zones
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/zones", nil)
	if err != nil {
		return "", fmt.Errorf("building zone list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.mu.Lock()
		if c.failedEndpoints != nil {
			c.failedEndpoints[endpoint] = true
		}
		c.mu.Unlock()
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
	// Skip endpoints known to be unreachable this batch
	c.mu.Lock()
	if c.batchMode && c.failedEndpoints[endpoint] {
		c.mu.Unlock()
		return fmt.Errorf("endpoint %s previously failed this batch, skipping", endpoint)
	}
	c.mu.Unlock()

	// Check for existing record to avoid creating duplicates.
	records, err := c.listRecordsCached(ctx, endpoint, zoneID)
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
		c.mu.Lock()
		if c.failedEndpoints != nil {
			c.failedEndpoints[endpoint] = true
		}
		c.mu.Unlock()
		return fmt.Errorf("registering host %s in zone %s: %w", hostname, zoneID, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registering host: HTTP %d: %s", resp.StatusCode, string(body))
	}

	c.invalidateCache(endpoint, zoneID)
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
	records, err := c.listRecordsCached(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	deleted := false
	for _, r := range records {
		if r.Name == hostname && r.Type == "A" {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				return err
			}
			deleted = true
			c.log.Infow("DNS record deregistered", "hostname", hostname, "record_id", r.ID, "zone", zoneID)
		}
	}

	if deleted {
		c.invalidateCache(endpoint, zoneID)
	}
	return nil
}

// DeregisterHostByIP removes A records matching both hostname and IP from a zone.
// Unlike DeregisterHost which removes all A records for a hostname, this only
// removes the specific record for one IP — used for cleaning up pod-level
// round-robin records without removing other containers' entries.
func (c *Client) DeregisterHostByIP(ctx context.Context, endpoint, zoneID, hostname, ip string) error {
	records, err := c.listRecordsCached(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	deleted := false
	for _, r := range records {
		if r.Name == hostname && r.Type == "A" && r.Data.Data == ip {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				return err
			}
			deleted = true
			c.log.Infow("DNS record deregistered by IP", "hostname", hostname, "ip", ip, "record_id", r.ID, "zone", zoneID)
		}
	}

	if deleted {
		c.invalidateCache(endpoint, zoneID)
	}
	return nil
}

// CleanStaleRecords removes A records for a hostname where the IP doesn't
// match the given current IP. Used to clean up stale records when a pod
// gets a new IP on recreation.
func (c *Client) CleanStaleRecords(ctx context.Context, endpoint, zoneID, hostname, currentIP string) error {
	records, err := c.listRecordsCached(ctx, endpoint, zoneID)
	if err != nil {
		return err
	}

	deleted := false
	for _, r := range records {
		if r.Name == hostname && r.Type == "A" && r.Data.Data != currentIP {
			if err := c.DeleteRecord(ctx, endpoint, zoneID, r.ID); err != nil {
				c.log.Warnw("failed to delete stale DNS record",
					"hostname", hostname, "stale_ip", r.Data.Data,
					"current_ip", currentIP, "error", err)
			} else {
				deleted = true
				c.log.Infow("removed stale DNS record",
					"hostname", hostname, "stale_ip", r.Data.Data,
					"current_ip", currentIP)
			}
		}
	}
	if deleted {
		c.invalidateCache(endpoint, zoneID)
	}
	return nil
}

// Close cleans up the client.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}
