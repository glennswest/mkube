package dzo

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

// State is the persisted DZO state, serialized to YAML.
type State struct {
	Zones     map[string]*Zone            `yaml:"zones"`
	Instances map[string]*MicroDNSInstance `yaml:"instances"`
}

// NewState returns an empty initialized state.
func NewState() *State {
	return &State{
		Zones:     make(map[string]*Zone),
		Instances: make(map[string]*MicroDNSInstance),
	}
}
