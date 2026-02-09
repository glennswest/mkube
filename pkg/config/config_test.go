package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestLoadDefaults(t *testing.T) {
	// Load with a non-existent config file — should use defaults
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
	if cfg.Network.PodCIDR != "192.168.200.0/24" {
		t.Errorf("expected default PodCIDR, got %q", cfg.Network.PodCIDR)
	}
	if cfg.Network.BridgeName != "containers" {
		t.Errorf("expected default bridge name, got %q", cfg.Network.BridgeName)
	}
	if cfg.Storage.GCIntervalMinutes != 30 {
		t.Errorf("expected default GC interval 30, got %d", cfg.Storage.GCIntervalMinutes)
	}
	if !cfg.Registry.Enabled {
		t.Error("expected registry enabled by default")
	}
}

func TestLoadFromYAML(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	content := `
nodeName: test-node
standalone: true
routeros:
  address: "10.0.0.1:8728"
  user: testuser
network:
  podCIDR: "10.244.0.0/16"
  bridgeName: test-bridge
storage:
  gcIntervalMinutes: 60
registry:
  enabled: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().String("config", cfgPath, "")
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
	if cfg.RouterOS.Address != "10.0.0.1:8728" {
		t.Errorf("expected RouterOS address from YAML, got %q", cfg.RouterOS.Address)
	}
	if cfg.Network.PodCIDR != "10.244.0.0/16" {
		t.Errorf("expected PodCIDR from YAML, got %q", cfg.Network.PodCIDR)
	}
	if cfg.Storage.GCIntervalMinutes != 60 {
		t.Errorf("expected GC interval 60 from YAML, got %d", cfg.Storage.GCIntervalMinutes)
	}
	if cfg.Registry.Enabled {
		t.Error("expected registry disabled from YAML")
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

	cmd := &cobra.Command{}
	cmd.Flags().String("config", cfgPath, "")
	cmd.Flags().String("kubeconfig", "", "")
	cmd.Flags().String("node-name", "flag-node", "")
	cmd.Flags().Bool("standalone", true, "")
	cmd.Flags().Bool("enable-registry", true, "")
	cmd.Flags().String("routeros-address", "192.168.1.1:8728", "")
	cmd.Flags().String("routeros-rest-url", "", "")
	cmd.Flags().String("routeros-user", "", "")
	cmd.Flags().String("routeros-password", "", "")
	cmd.Flags().String("pod-cidr", "", "")
	cmd.Flags().String("bridge-name", "", "")
	cmd.Flags().String("storage-path", "", "")
	cmd.Flags().String("tarball-cache", "", "")
	cmd.Flags().Int("gc-interval-minutes", 0, "")

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
