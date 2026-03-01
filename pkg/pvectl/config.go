// Package pvectl deploys OCI image binaries into Proxmox LXC containers.
// It creates Debian-based LXC containers, extracts Go binaries from OCI images,
// pushes them into the container, and installs systemd services.
package pvectl

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DeployConfig describes a complete LXC container deployment.
type DeployConfig struct {
	// Proxmox host connection
	PVEHost    string `yaml:"pveHost"`    // SSH host e.g. "pvex.gw.lo"
	PVEUser    string `yaml:"pveUser"`    // SSH user e.g. "root"
	PVEAPIUser string `yaml:"pveAPIUser"` // API user e.g. "mkube@pve!mkube-token"
	PVEAPIToken string `yaml:"pveAPIToken"` // API token secret (or $ENV_VAR)
	PVENode    string `yaml:"pveNode"`    // PVE node name e.g. "pvex"

	// Container identity
	VMID     int    `yaml:"vmid"`     // container VMID e.g. 119
	Hostname string `yaml:"hostname"` // container hostname

	// OCI image
	ImageRef   string `yaml:"imageRef"`   // OCI image ref e.g. "ghcr.io/glennswest/mkube-registry:latest"
	BinaryName string `yaml:"binaryName"` // binary name in image e.g. "mkube-registry"
	BinaryPath string `yaml:"binaryPath"` // install path e.g. "/usr/local/bin/mkube-registry"

	// Container resources
	Memory   int    `yaml:"memory"`   // MB, default 512
	Cores    int    `yaml:"cores"`    // default 2
	DiskSize string `yaml:"diskSize"` // e.g. "4G"
	Storage  string `yaml:"storage"`  // e.g. "local-lvm"

	// Networking
	Bridge  string `yaml:"bridge"`  // e.g. "vmbr0"
	IP      string `yaml:"ip"`      // e.g. "192.168.1.161/24"
	Gateway string `yaml:"gateway"` // e.g. "192.168.1.1"
	DNS     string `yaml:"dns"`     // e.g. "192.168.1.52"

	// Service configuration
	ServiceName string            `yaml:"serviceName"` // systemd unit name
	ExecArgs    string            `yaml:"execArgs"`    // extra args for binary
	EnvVars     map[string]string `yaml:"envVars"`     // environment variables
	ConfigFiles map[string]string `yaml:"configFiles"` // path → content to push into container

	// Template override (empty = auto-detect Debian 12)
	Template string `yaml:"template"` // e.g. "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst"
}

// LoadConfig reads a YAML deploy config from disk with environment variable expansion.
func LoadConfig(path string) (*DeployConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	// Expand environment variables (e.g. $PVE_API_TOKEN)
	expanded := os.ExpandEnv(string(data))

	var cfg DeployConfig
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}

	return &cfg, nil
}

func (c *DeployConfig) applyDefaults() {
	if c.PVEUser == "" {
		c.PVEUser = "root"
	}
	if c.Memory == 0 {
		c.Memory = 512
	}
	if c.Cores == 0 {
		c.Cores = 2
	}
	if c.DiskSize == "" {
		c.DiskSize = "4G"
	}
	if c.Storage == "" {
		c.Storage = "local-lvm"
	}
	if c.Bridge == "" {
		c.Bridge = "vmbr0"
	}
	if c.BinaryPath == "" && c.BinaryName != "" {
		c.BinaryPath = "/usr/local/bin/" + c.BinaryName
	}
	if c.ServiceName == "" && c.BinaryName != "" {
		c.ServiceName = c.BinaryName
	}
}

// Validate checks required fields are set.
func (c *DeployConfig) Validate() error {
	var missing []string
	if c.PVEHost == "" {
		missing = append(missing, "pveHost")
	}
	if c.PVENode == "" {
		missing = append(missing, "pveNode")
	}
	if c.VMID == 0 {
		missing = append(missing, "vmid")
	}
	if c.Hostname == "" {
		missing = append(missing, "hostname")
	}
	if c.ImageRef == "" {
		missing = append(missing, "imageRef")
	}
	if c.BinaryName == "" {
		missing = append(missing, "binaryName")
	}
	if c.IP == "" {
		missing = append(missing, "ip")
	}
	if c.Gateway == "" {
		missing = append(missing, "gateway")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// DiskSizeGB extracts the numeric GB value from DiskSize (e.g. "4G" → "4").
func (c *DeployConfig) DiskSizeGB() string {
	s := strings.TrimSuffix(c.DiskSize, "G")
	s = strings.TrimSuffix(s, "g")
	return s
}

// Net0Spec returns the Proxmox net0 spec string.
func (c *DeployConfig) Net0Spec() string {
	return fmt.Sprintf("name=eth0,bridge=%s,ip=%s,gw=%s", c.Bridge, c.IP, c.Gateway)
}
