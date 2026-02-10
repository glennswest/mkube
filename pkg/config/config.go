package config

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for microkube.
type Config struct {
	NodeName   string         `yaml:"nodeName"`
	Standalone bool           `yaml:"standalone"`
	KubeConfig string         `yaml:"kubeconfig"`
	RouterOS   RouterOSConfig `yaml:"routeros"`
	Networks   []NetworkDef   `yaml:"networks"`
	Storage    StorageConfig  `yaml:"storage"`
	Lifecycle  LifecycleConfig `yaml:"lifecycle"`
	Registry   RegistryConfig `yaml:"registry"`

	// Deprecated: single-network config for backward compatibility.
	// If present and Networks is empty, it is migrated into Networks.
	Network *legacyNetworkConfig `yaml:"network,omitempty"`
}

// NetworkDef defines a network that containers can be placed on.
type NetworkDef struct {
	Name       string    `yaml:"name"`
	Bridge     string    `yaml:"bridge"`
	CIDR       string    `yaml:"cidr"`
	Gateway    string    `yaml:"gateway"`
	VLAN       int       `yaml:"vlan,omitempty"`
	DNS        DNSConfig `yaml:"dns"`
}

// DNSConfig specifies the MicroDNS instance for a network.
type DNSConfig struct {
	Endpoint string `yaml:"endpoint"` // e.g. "http://192.168.200.199:8080"
	Zone     string `yaml:"zone"`     // e.g. "gt.lo"
	Server   string `yaml:"server"`   // DNS server IP for containers, e.g. "192.168.200.199"
}

// legacyNetworkConfig is the old single-network format, kept for backward compat.
type legacyNetworkConfig struct {
	PodCIDR     string   `yaml:"podCIDR"`
	ServiceCIDR string   `yaml:"serviceCIDR"`
	GatewayIP   string   `yaml:"gatewayIP"`
	BridgeName  string   `yaml:"bridgeName"`
	VLAN        int      `yaml:"vlan"`
	DNSServers  []string `yaml:"dnsServers"`
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

type StorageConfig struct {
	// Paths on the RouterOS filesystem
	BasePath     string `yaml:"basePath"`     // root for container volumes
	TarballCache string `yaml:"tarballCache"` // cache for downloaded image tarballs

	// Garbage collection
	GCIntervalMinutes int  `yaml:"gcIntervalMinutes"`
	GCKeepLastN       int  `yaml:"gcKeepLastN"`       // keep last N unused images
	GCDryRun          bool `yaml:"gcDryRun"`           // log only, don't delete
}

type LifecycleConfig struct {
	// Boot ordering
	BootManifestPath string `yaml:"bootManifestPath"` // path to boot-order YAML
	WatchdogInterval int    `yaml:"watchdogIntervalSeconds"`

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
	// Local addresses that resolve to this registry (used by storage manager)
	LocalAddresses []string `yaml:"localAddresses"` // e.g. ["192.168.200.2:5000"]
	// Image watcher: poll upstream registries for new digests and auto-pull
	WatchImages        []WatchImage `yaml:"watchImages"`
	WatchPollSeconds   int          `yaml:"watchPollSeconds"` // default 120
}

// WatchImage defines an upstream image to watch for changes.
// When a new digest is detected, the image is pulled into the local registry
// and a PushEvent is emitted for mkube-update to detect.
type WatchImage struct {
	// Upstream is the full image reference to poll (e.g., "ghcr.io/glenneth/microdns:latest")
	Upstream string `yaml:"upstream"`
	// LocalRepo is the repo name in the local registry (e.g., "microdns")
	LocalRepo string `yaml:"localRepo"`
}

// DefaultNetwork returns the first configured network (the default).
func (c *Config) DefaultNetwork() NetworkDef {
	if len(c.Networks) > 0 {
		return c.Networks[0]
	}
	return NetworkDef{}
}

// FindNetwork looks up a network by name. Returns the default if name is empty.
func (c *Config) FindNetwork(name string) (NetworkDef, bool) {
	if name == "" && len(c.Networks) > 0 {
		return c.Networks[0], true
	}
	for _, n := range c.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return NetworkDef{}, false
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
		Storage: StorageConfig{
			BasePath:           "/raid1/images",
			TarballCache:       "/raid1/cache",
			GCIntervalMinutes:  30,
			GCKeepLastN:        5,
		},
		Lifecycle: LifecycleConfig{
			BootManifestPath:   "/etc/microkube/boot-order.yaml",
			WatchdogInterval:   5,
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

	// Migrate legacy single-network config
	if cfg.Network != nil {
		migrated := NetworkDef{
			Name:    "containers",
			Bridge:  cfg.Network.BridgeName,
			CIDR:    cfg.Network.PodCIDR,
			Gateway: cfg.Network.GatewayIP,
			VLAN:    cfg.Network.VLAN,
		}
		if len(cfg.Network.DNSServers) > 0 {
			migrated.DNS.Server = cfg.Network.DNSServers[0]
		}
		cfg.Networks = []NetworkDef{migrated}
		cfg.Network = nil
	}

	// Apply default network if none configured
	if len(cfg.Networks) == 0 {
		cfg.Networks = []NetworkDef{
			{
				Name:    "containers",
				Bridge:  "containers",
				CIDR:    "192.168.200.0/24",
				Gateway: "192.168.200.1",
				DNS: DNSConfig{
					Endpoint: "http://192.168.200.199:8080",
					Zone:     "gt.lo",
					Server:   "192.168.200.199",
				},
			},
		}
	}

	// CLI flag overrides
	applyFlagOverrides(cfg, flags)

	return cfg, nil
}

func applyFlagOverrides(cfg *Config, flags *pflag.FlagSet) {
	// Only override config-file values with flags explicitly set on the CLI.
	// Using flags.Changed() avoids flag defaults clobbering YAML values.
	if flags.Changed("node-name") {
		cfg.NodeName, _ = flags.GetString("node-name")
	}
	if flags.Changed("standalone") {
		cfg.Standalone, _ = flags.GetBool("standalone")
	}
	if flags.Changed("kubeconfig") {
		cfg.KubeConfig, _ = flags.GetString("kubeconfig")
	}
	if flags.Changed("routeros-address") {
		cfg.RouterOS.Address, _ = flags.GetString("routeros-address")
	}
	if flags.Changed("routeros-rest-url") {
		cfg.RouterOS.RESTURL, _ = flags.GetString("routeros-rest-url")
	}
	if flags.Changed("routeros-user") {
		cfg.RouterOS.User, _ = flags.GetString("routeros-user")
	}
	if flags.Changed("routeros-password") {
		cfg.RouterOS.Password, _ = flags.GetString("routeros-password")
	}
	// pod-cidr and bridge-name override the first network
	if flags.Changed("pod-cidr") {
		if len(cfg.Networks) > 0 {
			cfg.Networks[0].CIDR, _ = flags.GetString("pod-cidr")
		}
	}
	if flags.Changed("bridge-name") {
		if len(cfg.Networks) > 0 {
			cfg.Networks[0].Bridge, _ = flags.GetString("bridge-name")
		}
	}
	if flags.Changed("storage-path") {
		cfg.Storage.BasePath, _ = flags.GetString("storage-path")
	}
	if flags.Changed("tarball-cache") {
		cfg.Storage.TarballCache, _ = flags.GetString("tarball-cache")
	}
	if flags.Changed("gc-interval-minutes") {
		cfg.Storage.GCIntervalMinutes, _ = flags.GetInt("gc-interval-minutes")
	}
	if flags.Changed("enable-registry") {
		cfg.Registry.Enabled, _ = flags.GetBool("enable-registry")
	}
}
