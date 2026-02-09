package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func newTestFlags() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("config", "/nonexistent/config.yaml", "")
	cmd.Flags().String("kubeconfig", "", "")
	cmd.Flags().String("node-name", "", "")
	cmd.Flags().Bool("standalone", false, "")
	cmd.Flags().Bool("enable-registry", true, "")
	cmd.Flags().String("routeros-address", "", "")
	cmd.Flags().String("routeros-rest-url", "", "")
	cmd.Flags().String("routeros-user", "", "")
	cmd.Flags().String("routeros-password", "", "")
	cmd.Flags().String("pod-cidr", "", "")
	cmd.Flags().String("bridge-name", "", "")
	cmd.Flags().String("storage-path", "", "")
	cmd.Flags().String("tarball-cache", "", "")
	cmd.Flags().Int("gc-interval-minutes", 0, "")
	return cmd
}

func TestLoadDefaults(t *testing.T) {
	cmd := newTestFlags()
	cfg, err := Load(cmd.Flags())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.NodeName != "mikrotik-node" {
		t.Errorf("expected default NodeName 'mikrotik-node', got %q", cfg.NodeName)
	}
	if cfg.RouterOS.Address != "192.168.200.1:8728" {
		t.Errorf("expected default RouterOS address, got %q", cfg.RouterOS.Address)
	}
	if len(cfg.Networks) != 1 {
		t.Fatalf("expected 1 default network, got %d", len(cfg.Networks))
	}
	net0 := cfg.Networks[0]
	if net0.Name != "containers" {
		t.Errorf("expected default network name 'containers', got %q", net0.Name)
	}
	if net0.CIDR != "192.168.200.0/24" {
		t.Errorf("expected default CIDR, got %q", net0.CIDR)
	}
	if net0.Bridge != "containers" {
		t.Errorf("expected default bridge name, got %q", net0.Bridge)
	}
	if net0.Gateway != "192.168.200.1" {
		t.Errorf("expected default gateway, got %q", net0.Gateway)
	}
	if net0.DNS.Zone != "gt.lo" {
		t.Errorf("expected default DNS zone 'gt.lo', got %q", net0.DNS.Zone)
	}
	if net0.DNS.Server != "192.168.200.199" {
		t.Errorf("expected default DNS server '192.168.200.199', got %q", net0.DNS.Server)
	}
	if net0.DNS.Endpoint != "http://192.168.200.199:8080" {
		t.Errorf("expected default DNS endpoint, got %q", net0.DNS.Endpoint)
	}
	if cfg.Storage.GCIntervalMinutes != 30 {
		t.Errorf("expected default GC interval 30, got %d", cfg.Storage.GCIntervalMinutes)
	}
	if !cfg.Registry.Enabled {
		t.Error("expected registry enabled by default")
	}
}

func TestLoadFromYAML_MultiNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
nodeName: test-node
standalone: true
routeros:
  address: "10.0.0.1:8728"
  user: testuser
networks:
  - name: containers
    bridge: containers
    cidr: "192.168.200.0/24"
    gateway: "192.168.200.1"
    dns:
      endpoint: "http://192.168.200.199:8080"
      zone: "gt.lo"
      server: "192.168.200.199"
  - name: gw
    bridge: bridge-lan
    cidr: "192.168.1.0/24"
    gateway: "192.168.1.1"
    dns:
      endpoint: "http://192.168.1.199:8080"
      zone: "gw.lo"
      server: "192.168.1.199"
storage:
  gcIntervalMinutes: 60
registry:
  enabled: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := newTestFlags()
	cmd.Flags().Set("config", cfgPath)

	cfg, err := Load(cmd.Flags())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.NodeName != "test-node" {
		t.Errorf("expected NodeName 'test-node', got %q", cfg.NodeName)
	}
	if !cfg.Standalone {
		t.Error("expected standalone=true from YAML")
	}
	if len(cfg.Networks) != 2 {
		t.Fatalf("expected 2 networks, got %d", len(cfg.Networks))
	}
	if cfg.Networks[0].Name != "containers" {
		t.Errorf("expected first network 'containers', got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[1].Name != "gw" {
		t.Errorf("expected second network 'gw', got %q", cfg.Networks[1].Name)
	}
	if cfg.Networks[1].DNS.Zone != "gw.lo" {
		t.Errorf("expected zone 'gw.lo', got %q", cfg.Networks[1].DNS.Zone)
	}
	if cfg.Storage.GCIntervalMinutes != 60 {
		t.Errorf("expected GC interval 60 from YAML, got %d", cfg.Storage.GCIntervalMinutes)
	}
	if cfg.Registry.Enabled {
		t.Error("expected registry disabled from YAML")
	}
}

func TestLoadFromYAML_LegacyMigration(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
nodeName: legacy-node
network:
  podCIDR: "10.244.0.0/16"
  gatewayIP: "10.244.0.1"
  bridgeName: test-bridge
  dnsServers:
    - "8.8.8.8"
    - "1.1.1.1"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := newTestFlags()
	cmd.Flags().Set("config", cfgPath)

	cfg, err := Load(cmd.Flags())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Networks) != 1 {
		t.Fatalf("expected 1 migrated network, got %d", len(cfg.Networks))
	}
	net0 := cfg.Networks[0]
	if net0.CIDR != "10.244.0.0/16" {
		t.Errorf("expected migrated CIDR, got %q", net0.CIDR)
	}
	if net0.Bridge != "test-bridge" {
		t.Errorf("expected migrated bridge, got %q", net0.Bridge)
	}
	if net0.Gateway != "10.244.0.1" {
		t.Errorf("expected migrated gateway, got %q", net0.Gateway)
	}
	if net0.DNS.Server != "8.8.8.8" {
		t.Errorf("expected migrated DNS server '8.8.8.8', got %q", net0.DNS.Server)
	}
	if cfg.Network != nil {
		t.Error("expected legacy Network to be nil after migration")
	}
}

func TestFlagOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
nodeName: yaml-node
routeros:
  address: "10.0.0.1:8728"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := newTestFlags()
	cmd.Flags().Set("config", cfgPath)
	cmd.Flags().Set("node-name", "flag-node")
	cmd.Flags().Set("standalone", "true")
	cmd.Flags().Set("routeros-address", "192.168.1.1:8728")

	cfg, err := Load(cmd.Flags())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.NodeName != "flag-node" {
		t.Errorf("expected flag override NodeName 'flag-node', got %q", cfg.NodeName)
	}
	if !cfg.Standalone {
		t.Error("expected standalone=true from flag override")
	}
	if cfg.RouterOS.Address != "192.168.1.1:8728" {
		t.Errorf("expected flag override address, got %q", cfg.RouterOS.Address)
	}
}

func TestFlagOverrides_NetworkFlags(t *testing.T) {
	cmd := newTestFlags()
	cmd.Flags().Set("pod-cidr", "10.0.0.0/16")
	cmd.Flags().Set("bridge-name", "custom-bridge")

	cfg, err := Load(cmd.Flags())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Networks[0].CIDR != "10.0.0.0/16" {
		t.Errorf("expected pod-cidr flag to override first network CIDR, got %q", cfg.Networks[0].CIDR)
	}
	if cfg.Networks[0].Bridge != "custom-bridge" {
		t.Errorf("expected bridge-name flag to override first network bridge, got %q", cfg.Networks[0].Bridge)
	}
}

func TestFindNetwork(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkDef{
			{Name: "containers", Bridge: "containers"},
			{Name: "gw", Bridge: "bridge-lan"},
		},
	}

	// Find by name
	n, ok := cfg.FindNetwork("gw")
	if !ok {
		t.Fatal("expected to find network 'gw'")
	}
	if n.Bridge != "bridge-lan" {
		t.Errorf("expected bridge 'bridge-lan', got %q", n.Bridge)
	}

	// Empty name returns default
	n, ok = cfg.FindNetwork("")
	if !ok {
		t.Fatal("expected default network for empty name")
	}
	if n.Name != "containers" {
		t.Errorf("expected default network 'containers', got %q", n.Name)
	}

	// Not found
	_, ok = cfg.FindNetwork("nonexistent")
	if ok {
		t.Error("expected not found for 'nonexistent'")
	}
}

func TestDefaultNetwork(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkDef{
			{Name: "first", Bridge: "br0"},
		},
	}
	if cfg.DefaultNetwork().Name != "first" {
		t.Errorf("expected 'first', got %q", cfg.DefaultNetwork().Name)
	}

	empty := &Config{}
	if empty.DefaultNetwork().Name != "" {
		t.Errorf("expected empty default for no networks")
	}
}
