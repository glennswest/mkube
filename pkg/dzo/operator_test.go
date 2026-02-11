package dzo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/dns"
)

// mockMicroDNS creates an httptest server that simulates MicroDNS API.
func mockMicroDNS(t *testing.T) *httptest.Server {
	t.Helper()

	zones := make(map[string]dns.Zone)
	nextID := 1

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/zones", func(w http.ResponseWriter, r *http.Request) {
		zoneList := make([]dns.Zone, 0, len(zones))
		for _, z := range zones {
			zoneList = append(zoneList, z)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(zoneList)
	})

	mux.HandleFunc("POST /api/v1/zones", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		id := jsonID(nextID)
		nextID++

		z := dns.Zone{ID: id, Name: req.Name}
		zones[id] = z

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(z)
	})

	return httptest.NewServer(mux)
}

func jsonID(n int) string {
	return string(rune('a'+n-1)) + "-zone-id"
}

func testOperator(t *testing.T, mdnsURL string, statePath string) *Operator {
	t.Helper()

	logger, _ := zap.NewDevelopment()
	log := logger.Sugar()

	networks := []config.NetworkDef{
		{
			Name:    "gt",
			Bridge:  "bridge-gt",
			CIDR:    "192.168.200.0/24",
			Gateway: "192.168.200.1",
			DNS: config.DNSConfig{
				Endpoint: mdnsURL,
				Zone:     "gt.lo",
				Server:   "192.168.200.199",
			},
		},
		{
			Name:    "g10",
			Bridge:  "bridge",
			CIDR:    "192.168.10.0/24",
			Gateway: "192.168.10.1",
			DNS: config.DNSConfig{
				Endpoint: mdnsURL,
				Zone:     "g10.lo",
				Server:   "192.168.10.199",
			},
		},
	}

	cfg := config.DZOConfig{
		Enabled:       true,
		StatePath:     statePath,
		MicroDNSImage: "192.168.200.2:5000/microdns:latest",
		DefaultMode:   "nested",
	}

	dnsClient := dns.NewClient(log)

	return NewOperator(cfg, networks, dnsClient, nil, nil, nil, log)
}

func TestBootstrap(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)

	ctx := context.Background()
	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Should have 2 zones (gt.lo, g10.lo)
	zones := op.ListZones()
	if len(zones) != 2 {
		t.Fatalf("expected 2 zones, got %d", len(zones))
	}

	// Should have 2 instances
	instances := op.ListInstances()
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}

	// Each zone should have a MicroDNS ID
	for _, z := range zones {
		if z.MicroDNSID == "" {
			t.Errorf("zone %q has no MicroDNS ID", z.Name)
		}
	}

	// State should be persisted
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Error("state file was not created")
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	// Bootstrap twice
	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("first bootstrap failed: %v", err)
	}
	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	// Should still have 2 zones
	if len(op.ListZones()) != 2 {
		t.Fatalf("expected 2 zones after double bootstrap, got %d", len(op.ListZones()))
	}
}

func TestCreateZoneShared(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Create a shared subdomain zone
	zone, err := op.CreateZone(ctx, CreateZoneRequest{
		Name:      "kube.gt.lo",
		Network:   "gt",
		Dedicated: false,
	})
	if err != nil {
		t.Fatalf("create zone failed: %v", err)
	}

	if zone.Name != "kube.gt.lo" {
		t.Errorf("expected zone name kube.gt.lo, got %s", zone.Name)
	}
	if zone.Parent != "gt.lo" {
		t.Errorf("expected parent gt.lo, got %s", zone.Parent)
	}
	if zone.Dedicated {
		t.Error("expected dedicated=false")
	}
	if zone.MicroDNSID == "" {
		t.Error("expected non-empty MicroDNS ID")
	}
	if zone.Endpoint != mdns.URL {
		t.Errorf("expected endpoint %s, got %s", mdns.URL, zone.Endpoint)
	}

	// Should now have 3 zones
	if len(op.ListZones()) != 3 {
		t.Fatalf("expected 3 zones, got %d", len(op.ListZones()))
	}
}

func TestCreateZoneDuplicate(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Try to create a zone that already exists
	_, err := op.CreateZone(ctx, CreateZoneRequest{
		Name:    "gt.lo",
		Network: "gt",
	})
	if err == nil {
		t.Fatal("expected error creating duplicate zone")
	}
}

func TestDeleteZone(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Create a zone then delete it
	_, err := op.CreateZone(ctx, CreateZoneRequest{
		Name:    "test.gt.lo",
		Network: "gt",
	})
	if err != nil {
		t.Fatalf("create zone failed: %v", err)
	}

	if err := op.DeleteZone(ctx, "test.gt.lo"); err != nil {
		t.Fatalf("delete zone failed: %v", err)
	}

	// Verify it's gone
	_, err = op.GetZone("test.gt.lo")
	if err == nil {
		t.Fatal("expected error getting deleted zone")
	}
}

func TestGetZoneEndpoint(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	endpoint, zoneID, err := op.GetZoneEndpoint("gt.lo")
	if err != nil {
		t.Fatalf("GetZoneEndpoint failed: %v", err)
	}
	if endpoint != mdns.URL {
		t.Errorf("expected endpoint %s, got %s", mdns.URL, endpoint)
	}
	if zoneID == "" {
		t.Error("expected non-empty zone ID")
	}

	// Non-existent zone
	_, _, err = op.GetZoneEndpoint("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent zone")
	}
}

func TestEnsureZone(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	op := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// EnsureZone on existing zone should be no-op
	if err := op.EnsureZone(ctx, "gt.lo", "gt", false); err != nil {
		t.Fatalf("EnsureZone on existing zone: %v", err)
	}

	// EnsureZone on new zone should create it
	if err := op.EnsureZone(ctx, "new.gt.lo", "gt", false); err != nil {
		t.Fatalf("EnsureZone on new zone: %v", err)
	}

	// Verify it exists
	_, err := op.GetZone("new.gt.lo")
	if err != nil {
		t.Fatalf("new zone not found after EnsureZone: %v", err)
	}
}

func TestStatePeristence(t *testing.T) {
	mdns := mockMicroDNS(t)
	defer mdns.Close()

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.yaml")

	// Create operator, bootstrap, create zone
	op1 := testOperator(t, mdns.URL, statePath)
	ctx := context.Background()

	if err := op1.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	_, err := op1.CreateZone(ctx, CreateZoneRequest{
		Name:    "persist.gt.lo",
		Network: "gt",
	})
	if err != nil {
		t.Fatalf("create zone failed: %v", err)
	}

	// Create new operator, load state
	op2 := testOperator(t, mdns.URL, statePath)
	if err := op2.Bootstrap(ctx); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}

	// Should have the persisted zone
	_, err = op2.GetZone("persist.gt.lo")
	if err != nil {
		t.Fatalf("persisted zone not found: %v", err)
	}
}

func TestParentZone(t *testing.T) {
	tests := []struct {
		name   string
		parent string
	}{
		{"gt.lo", "lo"},
		{"kube.gt.lo", "gt.lo"},
		{"deep.kube.gt.lo", "kube.gt.lo"},
		{"single", ""},
	}

	for _, tt := range tests {
		got := parentZone(tt.name)
		if got != tt.parent {
			t.Errorf("parentZone(%q) = %q, want %q", tt.name, got, tt.parent)
		}
	}
}
