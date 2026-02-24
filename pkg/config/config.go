package config

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/glennswest/mkube/pkg/network/ipam"
)

// Config is the top-level configuration for mkube.
type Config struct {
	NodeName   string          `yaml:"nodeName"`
	Standalone bool            `yaml:"standalone"`
	KubeConfig string          `yaml:"kubeconfig"`
	Backend    string          `yaml:"backend"` // "routeros" (default) or "stormbase"
	RouterOS   RouterOSConfig  `yaml:"routeros"`
	StormBase  StormBaseConfig `yaml:"stormbase"`
	Networks   []NetworkDef    `yaml:"networks"`
	Storage    StorageConfig   `yaml:"storage"`
	Lifecycle  LifecycleConfig `yaml:"lifecycle"`
	Registry   RegistryConfig  `yaml:"registry"`
	DZO        DZOConfig       `yaml:"dzo"`
	Namespace  NamespaceConfig `yaml:"namespace"`
	Logging    LoggingConfig   `yaml:"logging"`
	NATS       NATSConfig      `yaml:"nats"`
	BMH        BMHConfig       `yaml:"bmh"`

	// Deprecated: single-network config for backward compatibility.
	// If present and Networks is empty, it is migrated into Networks.
	Network *legacyNetworkConfig `yaml:"network,omitempty"`
}

// IsStormBase returns true if the backend is stormbase.
func (c *Config) IsStormBase() bool {
	return c.Backend == "stormbase"
}

// DZOConfig configures the Domain Zone Operator.
type DZOConfig struct {
	Enabled       bool   `yaml:"enabled"`
	ListenAddr    string `yaml:"listenAddr"`    // e.g. ":8082"
	StatePath     string `yaml:"statePath"`     // e.g. "/etc/mkube/dzo-state.yaml"
	MicroDNSImage string `yaml:"microdnsImage"` // e.g. "192.168.200.2:5000/microdns:latest"
	DefaultMode   string `yaml:"defaultMode"`   // "open" or "nested"
}

// LoggingConfig configures the connection to micrologs.
type LoggingConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"` // e.g. "http://logging.kube.gt.lo:8084"
}

// NATSConfig configures the NATS JetStream connection for persistent state.
type NATSConfig struct {
	URL      string `yaml:"url"`      // e.g. "nats://nats.gt.lo:4222"
	Replicas int    `yaml:"replicas"` // 1 for single-node, 3 for cluster
}

// BMHConfig configures BareMetalHost management.
type BMHConfig struct {
	PXEManagerURL string `yaml:"pxeManagerURL"` // default: http://pxe.g10.lo:8080
	DHCPLeaseURL  string `yaml:"dhcpLeaseURL"`  // default: http://dns.g11.lo:8080
	WatchInterval int    `yaml:"watchInterval"`  // seconds, default: 30
}

// NamespaceConfig configures the namespace manager.
type NamespaceConfig struct {
	StatePath   string `yaml:"statePath"`   // e.g. "/etc/mkube/namespace-state.yaml"
	DefaultMode string `yaml:"defaultMode"` // "open" or "nested", overrides DZO defaultMode
}

// NetworkDef defines a network that containers can be placed on.
type NetworkDef struct {
	Name      string    `yaml:"name"`
	Bridge    string    `yaml:"bridge"`
	CIDR      string    `yaml:"cidr"`
	Gateway   string    `yaml:"gateway"`
	VLAN      int       `yaml:"vlan,omitempty"`
	DNS       DNSConfig `yaml:"dns"`
	IPAMStart string    `yaml:"ipamStart,omitempty"` // first IP for container IPAM allocation
	IPAMEnd   string    `yaml:"ipamEnd,omitempty"`   // last IP for container IPAM allocation
}

// DNSConfig specifies the MicroDNS instance for a network.
type DNSConfig struct {
	Endpoint      string         `yaml:"endpoint"` // e.g. "http://192.168.200.199:8080"
	Zone          string         `yaml:"zone"`     // e.g. "gt.lo"
	Server        string         `yaml:"server"`   // DNS server IP for containers, e.g. "192.168.200.199"
	DHCP          DHCPConfig     `yaml:"dhcp"`     // DHCP server config for this network
	StaticRecords []StaticRecord `yaml:"staticRecords,omitempty"` // infrastructure hosts registered at startup
}

// DHCPConfig specifies DHCP settings for a MicroDNS instance.
type DHCPConfig struct {
	Enabled       bool              `yaml:"enabled"`
	RangeStart    string            `yaml:"rangeStart"`    // e.g. "192.168.11.10"
	RangeEnd      string            `yaml:"rangeEnd"`      // e.g. "192.168.11.200"
	LeaseTime     int               `yaml:"leaseTime"`     // seconds, default 3600
	NextServer    string            `yaml:"nextServer"`    // PXE server IP
	BootFile      string            `yaml:"bootFile"`      // PXE boot file
	ServerNetwork string            `yaml:"serverNetwork"` // serve DHCP from this network's DNS container (relay topology)
	Reservations  []DHCPReservation `yaml:"reservations"`
}

// StaticRecord is a DNS record registered in microdns at startup.
// Used for infrastructure hosts (routers, gateways) that aren't containers.
type StaticRecord struct {
	Name string `yaml:"name"` // hostname, e.g. "rose1"
	IP   string `yaml:"ip"`   // e.g. "192.168.10.1"
}

// DHCPReservation is a static DHCP lease for a known MAC address.
type DHCPReservation struct {
	MAC      string `yaml:"mac"`
	IP       string `yaml:"ip"`
	Hostname string `yaml:"hostname"`
}

// ComputedDNSServerIP derives the MicroDNS static IP from the network CIDR.
// It returns broadcast - 3 (max usable - 2), leaving room for routers.
// If the CIDR cannot be parsed, it falls back to the configured Server field.
func (n *NetworkDef) ComputedDNSServerIP() string {
	if n.CIDR == "" {
		return n.DNS.Server
	}
	_, subnet, err := net.ParseCIDR(n.CIDR)
	if err != nil {
		return n.DNS.Server
	}
	return ipam.DNSServerIP(subnet).String()
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

// StormBaseConfig holds connection settings for a stormd gRPC endpoint.
type StormBaseConfig struct {
	Address    string `yaml:"address"`    // e.g. "stormd.local:7443"
	CACert     string `yaml:"caCert"`     // path to CA cert PEM
	ClientCert string `yaml:"clientCert"` // path to client cert PEM
	ClientKey  string `yaml:"clientKey"`  // path to client key PEM
	Insecure   bool   `yaml:"insecure"`   // skip TLS (dev/test only)
}

type StorageConfig struct {
	// Paths on the RouterOS filesystem
	BasePath     string `yaml:"basePath"`     // root for container volumes
	TarballCache string `yaml:"tarballCache"` // cache for downloaded image tarballs

	// When set, write tarballs to local disk instead of uploading via REST API.
	// This is the mkube container's root-dir as seen by the RouterOS host,
	// e.g. "raid1/images/kube.gt.lo". A file written inside the container at
	// /raid1/cache/foo.tar becomes visible to RouterOS at
	// raid1/images/kube.gt.lo/raid1/cache/foo.tar.
	SelfRootDir string `yaml:"selfRootDir"`

	// Garbage collection
	GCIntervalMinutes int  `yaml:"gcIntervalMinutes"`
	GCKeepLastN       int  `yaml:"gcKeepLastN"` // keep last N unused images
	GCDryRun          bool `yaml:"gcDryRun"`    // log only, don't delete
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
	WatchImages      []WatchImage `yaml:"watchImages"`
	WatchPollSeconds int          `yaml:"watchPollSeconds"` // default 120
	// Upstream sync: push locally-pushed images to GHCR as backup
	UpstreamSyncEnabled bool   `yaml:"upstreamSyncEnabled"` // master toggle
	UpstreamToken       string `yaml:"upstreamToken"`       // inline GHCR PAT (write:packages scope)
	UpstreamTokenFile   string `yaml:"upstreamTokenFile"`   // path to file containing token
}

// WatchImage defines an upstream image to watch for changes.
// When a new digest is detected, the image is pulled into the local registry
// and a PushEvent is emitted for mkube-update to detect.
type WatchImage struct {
	// Upstream is the full image reference to poll (e.g., "ghcr.io/glennswest/microdns:latest")
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
		NodeName:   "mkube-node",
		Standalone: false,
		RouterOS: RouterOSConfig{
			Address:        "192.168.200.1:8728",
			RESTURL:        "https://192.168.200.1/rest",
			User:           "admin",
			InsecureVerify: true,
		},
		Storage: StorageConfig{
			BasePath:          "/raid1/images",
			TarballCache:      "/raid1/cache",
			GCIntervalMinutes: 30,
			GCKeepLastN:       5,
		},
		Lifecycle: LifecycleConfig{
			BootManifestPath: "/etc/mkube/boot-order.yaml",
			WatchdogInterval: 5,
			MaxRestarts:      5,
			RestartCooldown:  10,
		},
		Registry: RegistryConfig{
			Enabled:    true,
			ListenAddr: ":5000",
			StorePath:  "/raid1/registry",
		},
		BMH: BMHConfig{
			PXEManagerURL: "http://pxe.g10.lo:8080",
			DHCPLeaseURL:  "http://dns.g11.lo:8080",
			WatchInterval: 30,
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
	if flags.Changed("backend") {
		cfg.Backend, _ = flags.GetString("backend")
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
