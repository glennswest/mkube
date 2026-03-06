package network

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// NetworkState is the persisted state for the network operator.
type NetworkState struct {
	Switches map[string]*LogicalSwitch `yaml:"switches"`
	Ports    map[string]*LogicalPort   `yaml:"ports"`
}

// NewNetworkState returns an empty initialized state.
func NewNetworkState() *NetworkState {
	return &NetworkState{
		Switches: make(map[string]*LogicalSwitch),
		Ports:    make(map[string]*LogicalPort),
	}
}

// stateStore handles loading and saving NetworkState to a YAML file.
type stateStore struct {
	mu   sync.RWMutex
	path string
	data *NetworkState
}

func newStateStore(path string) *stateStore {
	return &stateStore{
		path: path,
		data: NewNetworkState(),
	}
}

func (s *stateStore) load() error {
	if s.path == "" {
		return nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}

	var state NetworkState
	if err := yaml.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("parsing network state: %w", err)
	}

	if state.Switches == nil {
		state.Switches = make(map[string]*LogicalSwitch)
	}
	if state.Ports == nil {
		state.Ports = make(map[string]*LogicalPort)
	}

	s.mu.Lock()
	s.data = &state
	s.mu.Unlock()
	return nil
}

func (s *stateStore) save() error {
	if s.path == "" {
		return nil
	}

	s.mu.RLock()
	raw, err := yaml.Marshal(s.data)
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshaling network state: %w", err)
	}

	if err := os.WriteFile(s.path, raw, 0644); err != nil {
		return fmt.Errorf("writing network state to %s: %w", s.path, err)
	}
	return nil
}

func (s *stateStore) get() *NetworkState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *stateStore) setSwitch(sw *LogicalSwitch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Switches[sw.Name] = sw
}

func (s *stateStore) removeSwitch(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Switches, name)
}

func (s *stateStore) setPort(p *LogicalPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Ports[p.Name] = p
}

func (s *stateStore) removePort(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Ports, name)
}

func (s *stateStore) removePortsBySwitch(switchName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, port := range s.data.Ports {
		if port.Switch == switchName {
			delete(s.data.Ports, name)
		}
	}
}
