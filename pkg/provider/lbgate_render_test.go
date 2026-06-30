package provider

import (
	"strings"
	"testing"

	"github.com/glennswest/mkube/pkg/config"
)

// TestLBAlwaysEnabled verifies that every generated microdns config carries the
// [dns.loadbalancer] section — LB is on by default on all instances.
func TestLBAlwaysEnabled(t *testing.T) {
	cfg := &config.Config{
		Backend: "routeros",
		Networks: []config.NetworkDef{
			{Name: "g9", CIDR: "192.168.9.0/24", DNS: config.DNSConfig{
				Zone: "g9.lo", Server: "192.168.9.252",
			}},
			{Name: "g10", CIDR: "192.168.10.0/24", DNS: config.DNSConfig{
				Zone: "g10.lo", Server: "192.168.10.252",
			}},
		},
	}
	for _, cm := range generateDefaultConfigMaps(cfg) {
		toml := cm.Data["microdns.toml"]
		if !strings.Contains(toml, "[dns.loadbalancer]") {
			t.Errorf("network %s missing [dns.loadbalancer]; got:\n%s", cm.Namespace, toml)
		}
		if !strings.Contains(toml, "enabled = true") {
			t.Errorf("network %s LB not enabled; got:\n%s", cm.Namespace, toml)
		}
	}
}
