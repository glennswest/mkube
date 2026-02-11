package namespace

import "context"

// NetworkingMode defines how a namespace connects to the network.
type NetworkingMode string

const (
	// ModeOpen uses existing flat network directly; namespace domain = network's zone.
	ModeOpen NetworkingMode = "open"
	// ModeNested creates a subdomain on the parent network, shared bridge, separate zone.
	ModeNested NetworkingMode = "nested"
)

// Namespace groups containers under a domain on a specific network.
type Namespace struct {
	Name       string         `json:"name" yaml:"name"`             // "kube", "infra"
	Domain     string         `json:"domain" yaml:"domain"`         // "kube.gt.lo"
	Zone       string         `json:"zone" yaml:"zone"`             // zone name (= domain)
	Network    string         `json:"network" yaml:"network"`       // "gt"
	Mode       NetworkingMode `json:"mode" yaml:"mode"`             // "open" or "nested"
	Containers []string       `json:"containers" yaml:"containers"` // container names in this namespace
}

// State is the persisted namespace state.
type State struct {
	Namespaces map[string]*Namespace `yaml:"namespaces"`
}

// NewState returns an empty initialized state.
func NewState() *State {
	return &State{
		Namespaces: make(map[string]*Namespace),
	}
}

// ZoneResolver provides access to DZO zone operations without importing the dzo package.
type ZoneResolver interface {
	// GetZoneEndpoint returns the MicroDNS endpoint and zone ID for a given zone name.
	GetZoneEndpoint(zoneName string) (endpoint, zoneID string, err error)
	// EnsureZone creates a zone if it doesn't exist. Idempotent.
	EnsureZone(ctx context.Context, zoneName, network string, dedicated bool) error
}
