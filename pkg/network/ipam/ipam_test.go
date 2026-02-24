package ipam

import (
	"net"
	"testing"
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
		got := IPToUint32(ip)
		if got != tt.want {
			t.Errorf("IPToUint32(%s) = %d, want %d", tt.ip, got, tt.want)
		}

		back := Uint32ToIP(tt.want)
		if !back.Equal(ip.To4()) {
			t.Errorf("Uint32ToIP(%d) = %s, want %s", tt.want, back, tt.ip)
		}
	}
}

func TestAllocateIP(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("172.20.0.0/24")
	gw := net.ParseIP("172.20.0.1")
	a.AddPool("test", subnet, gw)

	// First allocation should be .2
	ip1, err := a.Allocate("test", "veth-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip1.String() != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", ip1)
	}

	// Second should be .3
	ip2, err := a.Allocate("test", "veth-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip2.String() != "172.20.0.3" {
		t.Errorf("expected 172.20.0.3, got %s", ip2)
	}

	// Verify Get works
	got := a.Get("test", "veth-0")
	if !got.Equal(ip1) {
		t.Errorf("Get veth-0: expected %s, got %s", ip1, got)
	}
}

func TestAllocateIPExhaustion(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("172.20.0.0/30")
	gw := net.ParseIP("172.20.0.1")
	a.AddPool("test", subnet, gw)

	// Allocate .2 (the only usable address since .1 is gateway)
	ip, err := a.Allocate("test", "veth-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", ip)
	}

	// Should fail — pool exhausted
	_, err = a.Allocate("test", "veth-1")
	if err == nil {
		t.Error("expected exhaustion error")
	}
}

func TestRelease(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("172.20.0.0/30")
	gw := net.ParseIP("172.20.0.1")
	a.AddPool("test", subnet, gw)

	ip, _ := a.Allocate("test", "veth-0")
	if ip.String() != "172.20.0.2" {
		t.Fatalf("expected 172.20.0.2, got %s", ip)
	}

	// Release and reallocate
	a.Release("test", "veth-0")

	ip2, err := a.Allocate("test", "veth-1")
	if err != nil {
		t.Fatalf("after release, allocate should succeed: %v", err)
	}
	if ip2.String() != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2 to be reallocated, got %s", ip2)
	}
}

func TestRecord(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("10.0.0.0/24")
	gw := net.ParseIP("10.0.0.1")
	a.AddPool("net", subnet, gw)

	// Pre-record some allocations (simulating sync from device)
	a.Record("net", "veth-a", net.ParseIP("10.0.0.5"))
	a.Record("net", "veth-b", net.ParseIP("10.0.0.6"))

	allocs := a.PoolAllocations("net")
	if len(allocs) != 2 {
		t.Fatalf("expected 2 allocations, got %d", len(allocs))
	}

	// New allocation should skip recorded IPs
	ip, err := a.Allocate("net", "veth-c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() == "10.0.0.5" || ip.String() == "10.0.0.6" {
		t.Errorf("should not allocate already-recorded IP, got %s", ip)
	}
}

func TestPoolForIP(t *testing.T) {
	a := NewAllocator()
	_, s1, _ := net.ParseCIDR("10.0.0.0/24")
	_, s2, _ := net.ParseCIDR("172.16.0.0/24")
	a.AddPool("net1", s1, net.ParseIP("10.0.0.1"))
	a.AddPool("net2", s2, net.ParseIP("172.16.0.1"))

	if name := a.PoolForIP(net.ParseIP("10.0.0.42")); name != "net1" {
		t.Errorf("expected net1, got %q", name)
	}
	if name := a.PoolForIP(net.ParseIP("172.16.0.99")); name != "net2" {
		t.Errorf("expected net2, got %q", name)
	}
	if name := a.PoolForIP(net.ParseIP("192.168.1.1")); name != "" {
		t.Errorf("expected empty, got %q", name)
	}
}

func TestAllAllocations(t *testing.T) {
	a := NewAllocator()
	_, s1, _ := net.ParseCIDR("10.0.0.0/24")
	_, s2, _ := net.ParseCIDR("172.16.0.0/24")
	a.AddPool("net1", s1, net.ParseIP("10.0.0.1"))
	a.AddPool("net2", s2, net.ParseIP("172.16.0.1"))

	_, _ = a.Allocate("net1", "veth-a")
	_, _ = a.Allocate("net2", "veth-b")

	all := a.AllAllocations()
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
	if _, ok := all["veth-a"]; !ok {
		t.Error("missing veth-a")
	}
	if _, ok := all["veth-b"]; !ok {
		t.Error("missing veth-b")
	}
}

func TestAllocateUnknownPool(t *testing.T) {
	a := NewAllocator()
	_, err := a.Allocate("nonexistent", "key")
	if err == nil {
		t.Error("expected error for unknown pool")
	}
}

func TestAllocateStatic(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.200.0/24")
	gw := net.ParseIP("192.168.200.1")
	a.AddPool("gt", subnet, gw)

	// Allocate a specific IP
	err := a.AllocateStatic("gt", "veth-mdns-0", net.ParseIP("192.168.200.199"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := a.Get("gt", "veth-mdns-0")
	if got.String() != "192.168.200.199" {
		t.Errorf("expected 192.168.200.199, got %s", got)
	}

	// Dynamic allocation should skip the statically allocated IP
	ip, err := a.Allocate("gt", "veth-dyn-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() == "192.168.200.199" {
		t.Error("dynamic allocation should skip statically allocated IP")
	}
}

func TestAllocateStaticDuplicate(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.200.0/24")
	gw := net.ParseIP("192.168.200.1")
	a.AddPool("gt", subnet, gw)

	// First static allocation succeeds
	if err := a.AllocateStatic("gt", "veth-0", net.ParseIP("192.168.200.10")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Same IP for different key should fail
	err := a.AllocateStatic("gt", "veth-1", net.ParseIP("192.168.200.10"))
	if err == nil {
		t.Error("expected error for duplicate static IP")
	}
}

func TestAllocateStaticGateway(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.200.0/24")
	gw := net.ParseIP("192.168.200.1")
	a.AddPool("gt", subnet, gw)

	err := a.AllocateStatic("gt", "veth-0", net.ParseIP("192.168.200.1"))
	if err == nil {
		t.Error("expected error for gateway IP")
	}
}

func TestAllocateStaticOutOfSubnet(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.200.0/24")
	gw := net.ParseIP("192.168.200.1")
	a.AddPool("gt", subnet, gw)

	err := a.AllocateStatic("gt", "veth-0", net.ParseIP("10.0.0.5"))
	if err == nil {
		t.Error("expected error for IP outside subnet")
	}
}

func TestAllocateStaticUnknownPool(t *testing.T) {
	a := NewAllocator()

	err := a.AllocateStatic("nonexistent", "veth-0", net.ParseIP("192.168.200.10"))
	if err == nil {
		t.Error("expected error for unknown pool")
	}
}

func TestAllocateWithRange(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.11.0/24")
	gw := net.ParseIP("192.168.11.1")
	a.AddPool("g11", subnet, gw, PoolOpts{
		AllocStart: net.ParseIP("192.168.11.200"),
		AllocEnd:   net.ParseIP("192.168.11.210"),
	})

	// First allocation should be .200
	ip1, err := a.Allocate("g11", "veth-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip1.String() != "192.168.11.200" {
		t.Errorf("expected 192.168.11.200, got %s", ip1)
	}

	// Second should be .201
	ip2, err := a.Allocate("g11", "veth-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip2.String() != "192.168.11.201" {
		t.Errorf("expected 192.168.11.201, got %s", ip2)
	}
}

func TestAllocateWithRangeExhaustion(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.11.0/24")
	gw := net.ParseIP("192.168.11.1")
	a.AddPool("g11", subnet, gw, PoolOpts{
		AllocStart: net.ParseIP("192.168.11.200"),
		AllocEnd:   net.ParseIP("192.168.11.201"),
	})

	// Allocate both IPs in the range
	ip1, err := a.Allocate("g11", "veth-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip1.String() != "192.168.11.200" {
		t.Errorf("expected 192.168.11.200, got %s", ip1)
	}

	ip2, err := a.Allocate("g11", "veth-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip2.String() != "192.168.11.201" {
		t.Errorf("expected 192.168.11.201, got %s", ip2)
	}

	// Third allocation should fail — range exhausted
	_, err = a.Allocate("g11", "veth-2")
	if err == nil {
		t.Error("expected exhaustion error for constrained range")
	}
}

func TestAllocateWithRangeWraparound(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.11.0/24")
	gw := net.ParseIP("192.168.11.1")
	a.AddPool("g11", subnet, gw, PoolOpts{
		AllocStart: net.ParseIP("192.168.11.200"),
		AllocEnd:   net.ParseIP("192.168.11.202"),
	})

	// Allocate all 3 IPs: .200, .201, .202
	_, _ = a.Allocate("g11", "veth-0")
	_, _ = a.Allocate("g11", "veth-1")
	_, _ = a.Allocate("g11", "veth-2")

	// Release .200
	a.Release("g11", "veth-0")

	// Next allocation should wrap around and get .200
	ip, err := a.Allocate("g11", "veth-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() != "192.168.11.200" {
		t.Errorf("expected 192.168.11.200 after wraparound, got %s", ip)
	}
}

func TestAllocateWithRangeSkipsServerIPs(t *testing.T) {
	a := NewAllocator()
	_, subnet, _ := net.ParseCIDR("192.168.11.0/24")
	gw := net.ParseIP("192.168.11.1")
	a.AddPool("g11", subnet, gw, PoolOpts{
		AllocStart: net.ParseIP("192.168.11.200"),
		AllocEnd:   net.ParseIP("192.168.11.210"),
	})

	// Pre-record a static allocation in the range (simulating sync from device)
	a.Record("g11", "static-server", net.ParseIP("192.168.11.200"))

	// Dynamic allocation should skip .200
	ip, err := a.Allocate("g11", "veth-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip.String() != "192.168.11.201" {
		t.Errorf("expected 192.168.11.201, got %s", ip)
	}
}

func TestMaxUsableIP(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"192.168.11.0/24", "192.168.11.254"},
		{"192.168.200.0/24", "192.168.200.254"},
		{"10.0.0.0/24", "10.0.0.254"},
		{"172.20.0.0/30", "172.20.0.2"},   // 4 IPs: .0 net, .1, .2, .3 bcast → max usable = .2
		{"10.0.0.0/16", "10.0.255.254"},
	}
	for _, tt := range tests {
		_, subnet, _ := net.ParseCIDR(tt.cidr)
		got := MaxUsableIP(subnet).String()
		if got != tt.want {
			t.Errorf("MaxUsableIP(%s) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}

func TestDNSServerIP(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"192.168.11.0/24", "192.168.11.252"},  // .255 bcast, .254 max, .252 = max-2
		{"192.168.200.0/24", "192.168.200.252"},
		{"192.168.1.0/24", "192.168.1.252"},
		{"10.0.0.0/16", "10.0.255.252"},
	}
	for _, tt := range tests {
		_, subnet, _ := net.ParseCIDR(tt.cidr)
		got := DNSServerIP(subnet).String()
		if got != tt.want {
			t.Errorf("DNSServerIP(%s) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}
