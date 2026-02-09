package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Client is a REST client for MicroDNS instances.
// It is stateless — each call takes the endpoint URL as a parameter,
// so a single client can talk to multiple MicroDNS servers.
type Client struct {
	http *http.Client
	log  *zap.SugaredLogger
}

// Zone represents a MicroDNS zone.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Record represents a MicroDNS DNS record.
type Record struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"record_type"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
}

type createZoneRequest struct {
	Name string `json:"name"`
}

type createRecordRequest struct {
	Name    string `json:"name"`
	Type    string `json:"record_type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

// NewClient creates a new MicroDNS REST client.
func NewClient(log *zap.SugaredLogger) *Client {
	return &Client{
		http: &http.Client{Timeout: 10 * time.Second},
		log:  log,
	}
}

// EnsureZone finds an existing zone by name or creates it.
// Returns the zone UUID.
func (c *Client) EnsureZone(ctx context.Context, endpoint, zoneName string) (string, error) {
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
func (c *Client) RegisterHost(ctx context.Context, endpoint, zoneID, hostname, ip string, ttl int) error {
	payload, _ := json.Marshal(createRecordRequest{
		Name:    hostname,
		Type:    "A",
		Content: ip,
		TTL:     ttl,
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

// DeregisterHost removes all A records matching the given hostname from a zone.
func (c *Client) DeregisterHost(ctx context.Context, endpoint, zoneID, hostname string) error {
	// List records in zone
	url := fmt.Sprintf("%s/api/v1/zones/%s/records", endpoint, zoneID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building record list request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("listing records in zone %s: %w", zoneID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading record list response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listing records: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var records []Record
	if err := json.Unmarshal(body, &records); err != nil {
		return fmt.Errorf("decoding records: %w", err)
	}

	// Delete matching records
	for _, r := range records {
		if r.Name == hostname && r.Type == "A" {
			delURL := fmt.Sprintf("%s/api/v1/zones/%s/records/%s", endpoint, zoneID, r.ID)
			delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
			if err != nil {
				return fmt.Errorf("building delete request: %w", err)
			}

			delResp, err := c.http.Do(delReq)
			if err != nil {
				return fmt.Errorf("deleting record %s: %w", r.ID, err)
			}
			delResp.Body.Close()

			if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("deleting record %s: HTTP %d", r.ID, delResp.StatusCode)
			}

			c.log.Infow("DNS record deregistered", "hostname", hostname, "record_id", r.ID, "zone", zoneID)
		}
	}

	return nil
}

// Close cleans up the client.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}
