package dzo

// NetworkingMode defines how a namespace connects to the network.
type NetworkingMode string

const (
	// ModeOpen uses existing flat network directly; namespace domain = network's zone.
	ModeOpen NetworkingMode = "open"
	// ModeNested creates a subdomain on the parent network, shared bridge, separate zone.
	ModeNested NetworkingMode = "nested"
)

// Zone represents a DNS zone managed by a MicroDNS instance.
type Zone struct {
	Name       string `json:"name" yaml:"name"`             // "gt.lo", "kube.gt.lo"
	Parent     string `json:"parent" yaml:"parent"`         // "gt.lo" for subdomains, empty for top-level
	MicroDNSID string `json:"microdnsId" yaml:"microdnsId"` // zone UUID on the MicroDNS instance
	Endpoint   string `json:"endpoint" yaml:"endpoint"`     // "http://192.168.200.199:8080"
	Network    string `json:"network" yaml:"network"`       // "gt"
	Dedicated  bool   `json:"dedicated" yaml:"dedicated"`   // true = has its own MicroDNS container
}

// MicroDNSInstance represents a MicroDNS container, either inherited or DZO-managed.
type MicroDNSInstance struct {
	Name        string   `json:"name" yaml:"name"`               // RouterOS container name
	Network     string   `json:"network" yaml:"network"`         // "gt"
	IP          string   `json:"ip" yaml:"ip"`                   // "192.168.200.198"
	Endpoint    string   `json:"endpoint" yaml:"endpoint"`       // "http://192.168.200.198:8080"
	Zones       []string `json:"zones" yaml:"zones"`             // zone names served
	VethName    string   `json:"vethName" yaml:"vethName"`       // "veth-mdns-kube-gt"
	ContainerID string   `json:"containerId" yaml:"containerId"` // RouterOS .id
	Managed     bool     `json:"managed" yaml:"managed"`         // true if DZO created it
	Image       string   `json:"image" yaml:"image"`             // "192.168.200.2:5000/microdns:latest"
}

// Namespace groups containers under a domain on a specific network.
type Namespace struct {
	Name       string         `json:"name" yaml:"name"`             // "kube", "infra"
	Domain     string         `json:"domain" yaml:"domain"`         // "kube.gt.lo"
	Zone       string         `json:"zone" yaml:"zone"`             // zone name (= domain)
	Network    string         `json:"network" yaml:"network"`       // "gt"
	Mode       NetworkingMode `json:"mode" yaml:"mode"`             // "open" or "nested"
	Containers []string       `json:"containers" yaml:"containers"` // container names in this namespace
}

// State is the persisted DZO state, serialized to YAML.
type State struct {
	Zones      map[string]*Zone             `yaml:"zones"`
	Instances  map[string]*MicroDNSInstance  `yaml:"instances"`
	Namespaces map[string]*Namespace        `yaml:"namespaces"`
}

// NewState returns an empty initialized state.
func NewState() *State {
	return &State{
		Zones:      make(map[string]*Zone),
		Instances:  make(map[string]*MicroDNSInstance),
		Namespaces: make(map[string]*Namespace),
	}
}
