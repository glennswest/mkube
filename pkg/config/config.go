package config

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for mikrotik-kube.
type Config struct {
	NodeName   string         `yaml:"nodeName"`
	Standalone bool           `yaml:"standalone"`
	KubeConfig string         `yaml:"kubeconfig"`
	RouterOS   RouterOSConfig `yaml:"routeros"`
	Network    NetworkConfig  `yaml:"network"`
	Storage    StorageConfig  `yaml:"storage"`
	Systemd    SystemdConfig  `yaml:"systemd"`
	Registry   RegistryConfig `yaml:"registry"`
}

type RouterOSConfig struct {
	// Protocol API (routeros protocol on port 8728/8729)
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`

	// REST API (HTTPS on port 443)
	RESTURL string `yaml:"restUrl"`

	// TLS
	UseTLS         bool   `yaml:"useTLS"`
	CACert         string `yaml:"caCert"`
	InsecureVerify bool   `yaml:"insecureVerify"`
}

type NetworkConfig struct {
	// IPAM
	PodCIDR     string `yaml:"podCIDR"`     // e.g. "172.20.0.0/16"
	ServiceCIDR string `yaml:"serviceCIDR"` // optional, for service IPs
	GatewayIP   string `yaml:"gatewayIP"`   // bridge gateway, auto-derived if empty

	// RouterOS bridge
	BridgeName string `yaml:"bridgeName"` // RouterOS bridge interface name
	VLAN       int    `yaml:"vlan"`       // optional VLAN tag

	// DNS
	DNSServers []string `yaml:"dnsServers"`
}

type StorageConfig struct {
	// Paths on the RouterOS filesystem
	BasePath     string `yaml:"basePath"`     // root for container volumes
	TarballCache string `yaml:"tarballCache"` // cache for downloaded image tarballs

	// Garbage collection
	GCIntervalMinutes int  `yaml:"gcIntervalMinutes"`
	GCKeepLastN       int  `yaml:"gcKeepLastN"`       // keep last N unused images
	GCDryRun          bool `yaml:"gcDryRun"`           // log only, don't delete
}

type SystemdConfig struct {
	// Boot ordering
	BootManifestPath string `yaml:"bootManifestPath"` // path to boot-order YAML
	WatchdogInterval int    `yaml:"watchdogIntervalSeconds"`

	// Health checks
	DefaultHealthCheckPath string `yaml:"defaultHealthCheckPath"` // e.g. "/healthz"
	DefaultHealthCheckPort int    `yaml:"defaultHealthCheckPort"`

	// Restart policy
	MaxRestarts     int `yaml:"maxRestarts"`     // per container before giving up
	RestartCooldown int `yaml:"restartCooldown"` // seconds between restart attempts
}

type RegistryConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listenAddr"` // e.g. ":5000"
	StorePath  string `yaml:"storePath"`  // on-disk storage for registry blobs
	// Pull-through cache config
	PullThrough        bool     `yaml:"pullThrough"`
	UpstreamRegistries []string `yaml:"upstreamRegistries"` // e.g. ["docker.io", "ghcr.io"]
}

// Load reads config from file and overrides with CLI flags.
func Load(flags *pflag.FlagSet) (*Config, error) {
	configPath, _ := flags.GetString("config")

	cfg := &Config{
		NodeName:   "mikrotik-node",
		Standalone: false,
		RouterOS: RouterOSConfig{
			Address:        "192.168.200.1:8728",
			RESTURL:        "https://192.168.200.1/rest",
			User:           "admin",
			InsecureVerify: true,
		},
		Network: NetworkConfig{
			PodCIDR:    "192.168.200.0/24",
			GatewayIP:  "192.168.200.1",
			BridgeName: "containers",
			DNSServers: []string{"8.8.8.8", "1.1.1.1"},
		},
		Storage: StorageConfig{
			BasePath:           "/raid1/images",
			TarballCache:       "/raid1/cache",
			GCIntervalMinutes:  30,
			GCKeepLastN:        5,
		},
		Systemd: SystemdConfig{
			BootManifestPath:   "/etc/mikrotik-kube/boot-order.yaml",
			WatchdogInterval:   15,
			MaxRestarts:        5,
			RestartCooldown:    10,
		},
		Registry: RegistryConfig{
			Enabled:    true,
			ListenAddr: ":5000",
			StorePath:  "/raid1/registry",
		},
	}

	// Load from file if it exists
	if data, err := os.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	// CLI flag overrides
	applyFlagOverrides(cfg, flags)

	return cfg, nil
}

func applyFlagOverrides(cfg *Config, flags *pflag.FlagSet) {
	if v, _ := flags.GetString("node-name"); v != "" {
		cfg.NodeName = v
	}
	if v, _ := flags.GetBool("standalone"); v {
		cfg.Standalone = true
	}
	if v, _ := flags.GetString("kubeconfig"); v != "" {
		cfg.KubeConfig = v
	}
	if v, _ := flags.GetString("routeros-address"); v != "" {
		cfg.RouterOS.Address = v
	}
	if v, _ := flags.GetString("routeros-rest-url"); v != "" {
		cfg.RouterOS.RESTURL = v
	}
	if v, _ := flags.GetString("routeros-user"); v != "" {
		cfg.RouterOS.User = v
	}
	if v, _ := flags.GetString("routeros-password"); v != "" {
		cfg.RouterOS.Password = v
	}
	if v, _ := flags.GetString("pod-cidr"); v != "" {
		cfg.Network.PodCIDR = v
	}
	if v, _ := flags.GetString("bridge-name"); v != "" {
		cfg.Network.BridgeName = v
	}
	if v, _ := flags.GetString("storage-path"); v != "" {
		cfg.Storage.BasePath = v
	}
	if v, _ := flags.GetString("tarball-cache"); v != "" {
		cfg.Storage.TarballCache = v
	}
	if v, _ := flags.GetInt("gc-interval-minutes"); v > 0 {
		cfg.Storage.GCIntervalMinutes = v
	}
	if v, _ := flags.GetBool("enable-registry"); !v {
		cfg.Registry.Enabled = false
	}
}
