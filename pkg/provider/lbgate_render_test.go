package provider

import (
	"strings"
	"testing"

	"github.com/glennswest/mkube/pkg/config"
)

// TestLBGateRendering guards the per-network LB gate: the [dns.loadbalancer]
// section must appear only for networks that opt in via dns.loadBalancer.
func TestLBGateRendering(t *testing.T) {
	cfg := &config.Config{
		Backend: "routeros",
		Networks: []config.NetworkDef{
			{Name: "g9", CIDR: "192.168.9.0/24", DNS: config.DNSConfig{
				Zone: "g9.lo", Server: "192.168.9.252", LoadBalancer: true,
			}},
			{Name: "g10", CIDR: "192.168.10.0/24", DNS: config.DNSConfig{
				Zone: "g10.lo", Server: "192.168.10.252", LoadBalancer: false,
			}},
		},
	}
	cms := generateDefaultConfigMaps(cfg)
	got := map[string]string{}
	for _, cm := range cms {
		got[cm.Namespace] = cm.Data["microdns.toml"]
	}
	if !strings.Contains(got["g9"], "[dns.loadbalancer]") {
		t.Errorf("g9 should have [dns.loadbalancer]; got:\n%s", got["g9"])
	}
	if strings.Contains(got["g10"], "[dns.loadbalancer]") {
		t.Errorf("g10 should NOT have [dns.loadbalancer]; got:\n%s", got["g10"])
	}
	t.Logf("=== g9 microdns.toml ===\n%s", got["g9"])
	t.Logf("=== g10 microdns.toml ===\n%s", got["g10"])
}
