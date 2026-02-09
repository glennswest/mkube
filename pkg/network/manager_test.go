package network

import (
	"net"
	"testing"

	"github.com/glenneth/mikrotik-kube/pkg/config"
)

func TestIPConversion(t *testing.T) {
	tests := []struct {
		ip   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"0.0.0.1", 1},
		{"192.168.1.1", 3232235777},
		{"255.255.255.255", 4294967295},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		got := ipToUint32(ip)
		if got != tt.want {
			t.Errorf("ipToUint32(%s) = %d, want %d", tt.ip, got, tt.want)
		}

		back := uint32ToIP(tt.want)
		if !back.Equal(ip.To4()) {
			t.Errorf("uint32ToIP(%d) = %s, want %s", tt.want, back, tt.ip)
		}
	}
}

func newTestNetworkState(cidr, gateway string) *networkState {
	_, subnet, _ := net.ParseCIDR(cidr)
	gw := net.ParseIP(gateway)
	return &networkState{
		def: config.NetworkDef{
			Name:    "test",
			Bridge:  "br-test",
			CIDR:    cidr,
			Gateway: gateway,
		},
		subnet:    subnet,
		gateway:   gw,
		allocated: make(map[string]net.IP),
		nextIP:    2,
	}
}

func TestAllocateIP(t *testing.T) {
	ns := newTestNetworkState("172.20.0.0/24", "172.20.0.1")
	mgr := &Manager{
		networks: map[string]*networkState{"test": ns},
		netOrder: []string{"test"},
		allocs:   make(map[string]*allocation),
	}

	// First allocation should be .2
	ip1, err := mgr.allocateIP(ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip1.String() != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", ip1)
	}
	ns.allocated["veth-0"] = ip1

	// Second should be .3
	ip2, err := mgr.allocateIP(ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip2.String() != "172.20.0.3" {
		t.Errorf("expected 172.20.0.3, got %s", ip2)
	}
	ns.allocated["veth-1"] = ip2

	// Should skip gateway (.1) on wrap-around
	ns.nextIP = 1
	ip3, err := mgr.allocateIP(ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip3.Equal(ns.gateway) {
		t.Error("allocated gateway IP, should have been skipped")
	}
}

func TestAllocateIPExhaustion(t *testing.T) {
	ns := newTestNetworkState("172.20.0.0/30", "172.20.0.1")
	mgr := &Manager{
		networks: map[string]*networkState{"test": ns},
		netOrder: []string{"test"},
		allocs:   make(map[string]*allocation),
	}

	// Allocate .2 (the only usable address since .1 is gateway)
	ip, err := mgr.allocateIP(ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ns.allocated["veth-0"] = ip

	// Should fail — pool exhausted
	_, err = mgr.allocateIP(ns)
	if err == nil {
		t.Error("expected exhaustion error")
	}
}

func TestGetAllocations(t *testing.T) {
	ns := &networkState{
		def:       config.NetworkDef{Name: "test"},
		allocated: map[string]net.IP{
			"veth-a": net.ParseIP("172.20.0.2"),
			"veth-b": net.ParseIP("172.20.0.3"),
		},
	}

	mgr := &Manager{
		networks: map[string]*networkState{"test": ns},
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
		{"veth-myapp-0", "myapp"},
		{"veth-redis-1", "redis"},
		{"something", "something"},
	}
	for _, tt := range tests {
		got := extractHostname(tt.input)
		if got != tt.want {
			t.Errorf("extractHostname(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
