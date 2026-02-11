package namespace

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/glenneth/microkube/pkg/config"
)

// Manager manages namespaces. It uses a ZoneResolver (typically the DZO operator)
// to create and look up DNS zones for each namespace.
type Manager struct {
	cfg      config.NamespaceConfig
	dzoCfg   config.DZOConfig
	networks []config.NetworkDef
	zones    ZoneResolver
	log      *zap.SugaredLogger

	mu    sync.RWMutex
	state *State
}

// NewManager creates a namespace manager.
func NewManager(
	cfg config.NamespaceConfig,
	dzoCfg config.DZOConfig,
	networks []config.NetworkDef,
	zones ZoneResolver,
	log *zap.SugaredLogger,
) *Manager {
	return &Manager{
		cfg:      cfg,
		dzoCfg:   dzoCfg,
		networks: networks,
		zones:    zones,
		log:      log.Named("namespace"),
		state:    NewState(),
	}
}

// Bootstrap loads persisted state (migrating from DZO state if needed),
// creates default open namespaces for each network, and verifies zones.
func (m *Manager) Bootstrap(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Try loading namespace-specific state
	if err := m.loadState(); err != nil {
		m.log.Infow("no namespace state, attempting DZO state migration", "error", err)
		m.state = NewState()

		// 2. Migrate from DZO state if present
		if migrated := m.migrateDZOState(); migrated {
			m.log.Infow("migrated namespaces from DZO state")
		}
	}

	// 3. Create default open namespaces for networks that don't have one
	for _, net := range m.networks {
		if net.DNS.Zone == "" {
			continue
		}
		nsName := net.Name
		if _, exists := m.state.Namespaces[nsName]; !exists {
			m.state.Namespaces[nsName] = &Namespace{
				Name:    nsName,
				Domain:  net.DNS.Zone,
				Zone:    net.DNS.Zone,
				Network: net.Name,
				Mode:    ModeOpen,
			}
			m.log.Infow("created default namespace", "name", nsName, "domain", net.DNS.Zone, "network", net.Name)
		}
	}

	// 4. Verify zones exist via ZoneResolver
	for _, ns := range m.state.Namespaces {
		if _, _, err := m.zones.GetZoneEndpoint(ns.Zone); err != nil {
			m.log.Warnw("namespace zone not found, will attempt to ensure on use",
				"namespace", ns.Name, "zone", ns.Zone, "error", err)
		}
	}

	// 5. Persist
	if err := m.saveState(); err != nil {
		m.log.Warnw("failed to save state after bootstrap", "error", err)
	}

	m.log.Infow("bootstrap complete", "namespaces", len(m.state.Namespaces))
	return nil
}

// migrateDZOState reads the DZO state file and extracts namespaces from it.
func (m *Manager) migrateDZOState() bool {
	dzoPath := m.dzoCfg.StatePath
	if dzoPath == "" {
		dzoPath = "/etc/microkube/dzo-state.yaml"
	}

	data, err := os.ReadFile(dzoPath)
	if err != nil {
		return false
	}

	// Parse just the namespaces field from DZO state
	var dzoState struct {
		Namespaces map[string]*struct {
			Name       string `yaml:"name"`
			Domain     string `yaml:"domain"`
			Zone       string `yaml:"zone"`
			Network    string `yaml:"network"`
			Mode       string `yaml:"mode"`
			Containers []string `yaml:"containers"`
		} `yaml:"namespaces"`
	}
	if err := yaml.Unmarshal(data, &dzoState); err != nil {
		return false
	}

	if len(dzoState.Namespaces) == 0 {
		return false
	}

	for name, ns := range dzoState.Namespaces {
		m.state.Namespaces[name] = &Namespace{
			Name:       ns.Name,
			Domain:     ns.Domain,
			Zone:       ns.Zone,
			Network:    ns.Network,
			Mode:       NetworkingMode(ns.Mode),
			Containers: ns.Containers,
		}
	}
	return true
}

// ─── Namespace CRUD ─────────────────────────────────────────────────────────

// ListNamespaces returns all namespaces.
func (m *Manager) ListNamespaces() []*Namespace {
	m.mu.RLock()
	defer m.mu.RUnlock()

	nss := make([]*Namespace, 0, len(m.state.Namespaces))
	for _, ns := range m.state.Namespaces {
		nss = append(nss, ns)
	}
	return nss
}

// GetNamespace returns a namespace by name.
func (m *Manager) GetNamespace(name string) (*Namespace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ns, ok := m.state.Namespaces[name]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", name)
	}
	return ns, nil
}

// CreateNamespace creates a namespace, auto-creating its zone via ZoneResolver if needed.
func (m *Manager) CreateNamespace(ctx context.Context, name, domain, networkName string, mode NetworkingMode, dedicated bool) (*Namespace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.state.Namespaces[name]; exists {
		return nil, fmt.Errorf("namespace %q already exists", name)
	}

	// Defaults
	if mode == "" {
		mode = NetworkingMode(m.defaultMode())
		if mode == "" {
			mode = ModeNested
		}
	}
	if networkName == "" {
		networkName = m.defaultNetwork()
	}
	if domain == "" {
		// Auto-derive: {name}.{network-default-zone}
		net := m.findNetwork(networkName)
		if net == nil {
			return nil, fmt.Errorf("network %q not found", networkName)
		}
		domain = name + "." + net.DNS.Zone
	}

	// Ensure zone exists
	if err := m.zones.EnsureZone(ctx, domain, networkName, dedicated); err != nil {
		return nil, fmt.Errorf("ensuring zone %q: %w", domain, err)
	}

	ns := &Namespace{
		Name:    name,
		Domain:  domain,
		Zone:    domain,
		Network: networkName,
		Mode:    mode,
	}

	m.state.Namespaces[name] = ns
	if err := m.saveState(); err != nil {
		m.log.Warnw("failed to save state after namespace create", "error", err)
	}

	m.log.Infow("namespace created", "name", name, "domain", domain, "mode", mode)
	return ns, nil
}

// DeleteNamespace removes a namespace.
func (m *Manager) DeleteNamespace(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, ok := m.state.Namespaces[name]
	if !ok {
		return fmt.Errorf("namespace %q not found", name)
	}

	if len(ns.Containers) > 0 {
		return fmt.Errorf("namespace %q still has %d containers", name, len(ns.Containers))
	}

	delete(m.state.Namespaces, name)
	if err := m.saveState(); err != nil {
		m.log.Warnw("failed to save state after namespace delete", "error", err)
	}

	m.log.Infow("namespace deleted", "name", name)
	return nil
}

// ─── Provider Integration ───────────────────────────────────────────────────

// ResolveNamespace looks up a namespace and returns its zone endpoint and zone ID.
func (m *Manager) ResolveNamespace(name string) (endpoint, zoneID string, err error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ns, ok := m.state.Namespaces[name]
	if !ok {
		return "", "", fmt.Errorf("namespace %q not found", name)
	}

	return m.zones.GetZoneEndpoint(ns.Zone)
}

// AddContainerToNamespace records a container in a namespace.
func (m *Manager) AddContainerToNamespace(nsName, containerName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, ok := m.state.Namespaces[nsName]
	if !ok {
		return
	}

	for _, c := range ns.Containers {
		if c == containerName {
			return
		}
	}
	ns.Containers = append(ns.Containers, containerName)
	_ = m.saveState()
}

// RemoveContainerFromNamespace removes a container from a namespace.
func (m *Manager) RemoveContainerFromNamespace(nsName, containerName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, ok := m.state.Namespaces[nsName]
	if !ok {
		return
	}

	ns.Containers = removeString(ns.Containers, containerName)
	_ = m.saveState()
}

// ─── State Persistence ──────────────────────────────────────────────────────

func (m *Manager) loadState() error {
	path := m.statePath()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing state: %w", err)
	}

	if state.Namespaces == nil {
		state.Namespaces = make(map[string]*Namespace)
	}

	m.state = &state
	m.log.Infow("loaded state", "namespaces", len(state.Namespaces))
	return nil
}

func (m *Manager) saveState() error {
	path := m.statePath()

	data, err := yaml.Marshal(m.state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing state to %s: %w", path, err)
	}
	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func (m *Manager) statePath() string {
	if m.cfg.StatePath != "" {
		return m.cfg.StatePath
	}
	return "/etc/microkube/namespace-state.yaml"
}

func (m *Manager) defaultMode() string {
	if m.cfg.DefaultMode != "" {
		return m.cfg.DefaultMode
	}
	return m.dzoCfg.DefaultMode
}

func (m *Manager) defaultNetwork() string {
	if len(m.networks) > 0 {
		return m.networks[0].Name
	}
	return ""
}

func (m *Manager) findNetwork(name string) *config.NetworkDef {
	for i := range m.networks {
		if m.networks[i].Name == name {
			return &m.networks[i]
		}
	}
	return nil
}

func removeString(ss []string, s string) []string {
	out := make([]string, 0, len(ss))
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
