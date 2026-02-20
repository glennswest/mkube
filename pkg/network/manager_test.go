package network

import (
	"net"
	"testing"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/network/ipam"
)

func TestGetAllocations(t *testing.T) {
	alloc := ipam.NewAllocator()
	_, s, _ := net.ParseCIDR("172.20.0.0/24")
	alloc.AddPool("test", s, net.ParseIP("172.20.0.1"))
	alloc.Record("test", "veth-a", net.ParseIP("172.20.0.2"))
	alloc.Record("test", "veth-b", net.ParseIP("172.20.0.3"))

	mgr := &Manager{
		networks: map[string]*networkState{
			"test": {def: config.NetworkDef{Name: "test"}},
		},
		ipam: alloc,
	}

	allocs := mgr.GetAllocations()
	if len(allocs) != 2 {
		t.Fatalf("expected 2 allocations, got %d", len(allocs))
	}
	if allocs["veth-a"] != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", allocs["veth-a"])
	}
}

func TestResolveNetwork(t *testing.T) {
	ns1 := &networkState{def: config.NetworkDef{Name: "containers"}}
	ns2 := &networkState{def: config.NetworkDef{Name: "gw"}}

	mgr := &Manager{
		networks: map[string]*networkState{
			"containers": ns1,
			"gw":         ns2,
		},
		netOrder: []string{"containers", "gw"},
	}

	// Default (empty name)
	got, err := mgr.resolveNetwork("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.def.Name != "containers" {
		t.Errorf("expected 'containers', got %q", got.def.Name)
	}

	// By name
	got, err = mgr.resolveNetwork("gw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.def.Name != "gw" {
		t.Errorf("expected 'gw', got %q", got.def.Name)
	}

	// Not found
	_, err = mgr.resolveNetwork("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent network")
	}
}

func TestExtractHostname(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"veth_default_myapp_0", "myapp"},
		{"veth_gw_dns_0", "dns"},
		{"veth_g10_redis_1", "redis"},
		{"something", "something"},
	}
	for _, tt := range tests {
		got := extractHostname(tt.input)
		if got != tt.want {
			t.Errorf("extractHostname(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
