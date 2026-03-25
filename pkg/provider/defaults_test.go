package provider

import (
	"strings"
	"testing"

	"github.com/glennswest/mkube/pkg/config"
)

func TestGenerateDefaultConfigMaps_DNSRecursor(t *testing.T) {
	cfg := &config.Config{
		NodeName: "test-node",
		Networks: []config.NetworkDef{
			{Name: "gt", Gateway: "192.168.200.1", DNS: config.DNSConfig{Zone: "gt.lo", Server: "192.168.200.199"}},
			{Name: "g10", Gateway: "192.168.10.1", DNS: config.DNSConfig{Zone: "g10.lo", Server: "192.168.10.199"}},
			{Name: "g11", Gateway: "192.168.11.1", DNS: config.DNSConfig{Zone: "g11.lo", Server: "192.168.11.199"}},
		},
	}

	cms := generateDefaultConfigMaps(cfg)

	// 3 dns ConfigMaps (console removed — UI is built into mkube)
	if len(cms) != 3 {
		t.Fatalf("expected 3 configmaps, got %d", len(cms))
	}

	// Verify DNS ConfigMaps contain minimal structural config.
	// Forward zones and DHCP pools/reservations are now seeded via REST API,
	// not baked into the TOML.
	for i, net := range cfg.Networks {
		cm := cms[i]
		if cm.Namespace != net.Name {
			t.Errorf("cm[%d] namespace = %q, want %q", i, cm.Namespace, net.Name)
		}
		if cm.Name != "dns-config" {
			t.Errorf("cm[%d] name = %q, want dns-config", i, cm.Name)
		}
		toml := cm.Data["microdns.toml"]
		if !strings.Contains(toml, `mode = "gateway"`) {
			t.Errorf("cm[%d] expected mode = gateway for default (routeros) backend", i)
		}
		if !strings.Contains(toml, "enabled = true") {
			t.Errorf("cm[%d] missing recursor enabled", i)
		}
		// TOML should have empty forward_zones section (seeded via REST API)
		if !strings.Contains(toml, "[dns.recursor.forward_zones]") {
			t.Errorf("cm[%d] missing forward_zones section header", i)
		}
		// Forward zones should NOT contain any peer zones (REST API seeds them)
		fwdIdx := strings.Index(toml, "[dns.recursor.forward_zones]")
		apiIdx := strings.Index(toml, "[api.rest]")
		if fwdIdx >= 0 && apiIdx > fwdIdx {
			fwdSection := toml[fwdIdx:apiIdx]
			for _, peer := range cfg.Networks {
				if peer.Name == net.Name {
					continue
				}
				if strings.Contains(fwdSection, peer.DNS.Server+":53") {
					t.Errorf("cm[%d] should not contain forward zone for %s (seeded via REST API)", i, peer.DNS.Zone)
				}
			}
		}
	}
}

func TestGenerateDefaultConfigMaps_NoDNS(t *testing.T) {
	cfg := &config.Config{
		NodeName: "test-node",
		Networks: []config.NetworkDef{
			{Name: "plain", Gateway: "10.0.0.1"},
		},
	}

	cms := generateDefaultConfigMaps(cfg)
	// No DNS — empty
	if len(cms) != 0 {
		t.Fatalf("expected 0 configmaps, got %d", len(cms))
	}
}
