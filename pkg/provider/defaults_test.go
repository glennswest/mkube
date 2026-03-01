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

	// 1 console + 3 dns = 4
	if len(cms) != 4 {
		t.Fatalf("expected 4 configmaps, got %d", len(cms))
	}

	// Verify DNS ConfigMaps
	for i, net := range cfg.Networks {
		cm := cms[i+1] // skip console
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
		// Extract just the forward_zones section for checking
		fwdIdx := strings.Index(toml, "[dns.recursor.forward_zones]")
		apiIdx := strings.Index(toml, "[api.rest]")
		fwdSection := ""
		if fwdIdx >= 0 && apiIdx > fwdIdx {
			fwdSection = toml[fwdIdx:apiIdx]
		}
		// Should NOT contain its own zone as a forward zone
		if strings.Contains(fwdSection, `"`+net.DNS.Zone+`"`) {
			t.Errorf("cm[%d] should not forward to own zone %s", i, net.DNS.Zone)
		}
		// Should contain all peer zones in forward section
		for _, peer := range cfg.Networks {
			if peer.Name == net.Name {
				continue
			}
			if !strings.Contains(fwdSection, `"`+peer.DNS.Zone+`"`) {
				t.Errorf("cm[%d] missing forward zone for %s", i, peer.DNS.Zone)
			}
			if !strings.Contains(fwdSection, peer.DNS.Server+":53") {
				t.Errorf("cm[%d] missing server %s for zone %s", i, peer.DNS.Server, peer.DNS.Zone)
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
	// Only console ConfigMap — no DNS
	if len(cms) != 1 {
		t.Fatalf("expected 1 configmap (console only), got %d", len(cms))
	}
}
