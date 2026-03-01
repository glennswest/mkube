package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/store"
)

// NodeStatus is written to the NODE_STATUS bucket as a heartbeat.
type NodeStatus struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Backend      string `json:"backend,omitempty"`
	Architecture string `json:"architecture,omitempty"` // "arm64", "amd64"
	Healthy      bool   `json:"healthy"`
	Timestamp    int64  `json:"timestamp"` // unix millis
}

// ClusterStatus is the response for GET /api/v1/cluster/status.
type ClusterStatus struct {
	LocalNode string       `json:"localNode"`
	Peers     []PeerStatus `json:"peers"`
}

// PeerStatus describes a peer node's health.
type PeerStatus struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Backend      string `json:"backend,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Healthy      bool   `json:"healthy"`
	LastSeen     int64  `json:"lastSeen"` // unix millis, 0 if never seen
}

// Manager handles cluster peer health monitoring and node heartbeat.
type Manager struct {
	nodeName string
	arch     string // "arm64", "amd64"
	cfg      config.ClusterConfig
	store    *store.Store
	log      *zap.SugaredLogger
	syncMgr  *SyncManager

	mu       sync.RWMutex
	peerUp   map[string]bool
	lastSeen map[string]time.Time
}

// New creates a cluster manager. arch is the local node's architecture (e.g. "arm64", "amd64").
func New(nodeName string, cfg config.ClusterConfig, st *store.Store, arch string, log *zap.SugaredLogger) *Manager {
	return &Manager{
		nodeName: nodeName,
		arch:     arch,
		cfg:      cfg,
		store:    st,
		log:      log.Named("cluster"),
		peerUp:   make(map[string]bool),
		lastSeen: make(map[string]time.Time),
	}
}

// Start launches the heartbeat and peer monitor goroutines.
func (m *Manager) Start(ctx context.Context) {
	// Initialize sync manager
	m.syncMgr = NewSyncManager(m.nodeName, m.cfg, m.store, m.log)
	m.store.SetSyncHook(m.syncMgr.OnLocalWrite)

	go m.heartbeatLoop(ctx)
	go m.peerMonitorLoop(ctx)
}

// SyncManager returns the sync manager (for registering HTTP handlers).
func (m *Manager) SyncManager() *SyncManager {
	return m.syncMgr
}

// Peers returns the configured peer list.
func (m *Manager) Peers() []config.PeerConfig {
	return m.cfg.Peers
}

// IsPeerHealthy returns true if the named peer is considered healthy.
func (m *Manager) IsPeerHealthy(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.peerUp[name]
}

// HealthyNodes returns sorted list of healthy nodes (local node first).
func (m *Manager) HealthyNodes() []string {
	nodes := []string{m.nodeName}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, peer := range m.cfg.Peers {
		if m.peerUp[peer.Name] {
			nodes = append(nodes, peer.Name)
		}
	}
	sort.Strings(nodes[1:]) // keep local first, sort peers
	return nodes
}

// FailoverTimeout returns the configured failover timeout in seconds (default 300).
func (m *Manager) FailoverTimeout() int {
	if m.cfg.FailoverTimeout > 0 {
		return m.cfg.FailoverTimeout
	}
	return 300
}

// RegisterRoutes registers cluster API endpoints on the mux.
func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cluster/status", m.handleClusterStatus)
	// Sync endpoints
	mux.HandleFunc("POST /api/v1/cluster/sync", m.syncMgr.HandleSyncEvent)
	mux.HandleFunc("GET /api/v1/cluster/full-sync", m.syncMgr.HandleFullSyncExport)
	mux.HandleFunc("POST /api/v1/cluster/full-sync", m.syncMgr.HandleFullSyncImport)
}

// heartbeatLoop writes local NodeStatus to the NODE_STATUS bucket every 15s.
func (m *Manager) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Write immediately on start
	m.writeHeartbeat(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.writeHeartbeat(ctx)
		}
	}
}

func (m *Manager) writeHeartbeat(ctx context.Context) {
	status := NodeStatus{
		Name:         m.nodeName,
		Architecture: m.arch,
		Healthy:      true,
		Timestamp:    time.Now().UnixMilli(),
	}
	if _, err := m.store.NodeStatus.PutJSON(ctx, m.nodeName, status); err != nil {
		m.log.Warnw("failed to write heartbeat", "error", err)
	}
}

// peerMonitorLoop checks each peer's /healthz every 15s.
func (m *Manager) peerMonitorLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, peer := range m.cfg.Peers {
				healthy := m.checkPeer(ctx, client, peer)

				m.mu.Lock()
				wasUp := m.peerUp[peer.Name]
				m.peerUp[peer.Name] = healthy
				if healthy {
					m.lastSeen[peer.Name] = time.Now()
				}
				m.mu.Unlock()

				if !wasUp && healthy {
					m.log.Infow("peer came up, triggering full resync", "peer", peer.Name)
					go m.syncMgr.FullResyncWithPeer(ctx, peer)
				} else if wasUp && !healthy {
					m.log.Warnw("peer went down", "peer", peer.Name)
				}
			}
		}
	}
}

func (m *Manager) checkPeer(ctx context.Context, client *http.Client, peer config.PeerConfig) bool {
	url := peer.Address + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (m *Manager) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := ClusterStatus{
		LocalNode: m.nodeName,
	}
	for _, peer := range m.cfg.Peers {
		ps := PeerStatus{
			Name:    peer.Name,
			Address: peer.Address,
			Healthy: m.peerUp[peer.Name],
		}
		if t, ok := m.lastSeen[peer.Name]; ok {
			ps.LastSeen = t.UnixMilli()
		}
		// Read peer's NodeStatus from bucket for backend/architecture
		var ns NodeStatus
		if _, err := m.store.NodeStatus.GetJSON(r.Context(), peer.Name, &ns); err == nil {
			ps.Backend = ns.Backend
			ps.Architecture = ns.Architecture
		}
		status.Peers = append(status.Peers, ps)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// NodeName returns the local node name.
func (m *Manager) NodeName() string {
	return m.nodeName
}

// AllNodes returns all node names (local + all configured peers, regardless of health).
func (m *Manager) AllNodes() []string {
	nodes := []string{m.nodeName}
	for _, peer := range m.cfg.Peers {
		nodes = append(nodes, peer.Name)
	}
	return nodes
}

// PeerLastSeen returns when a peer was last seen healthy.
func (m *Manager) PeerLastSeen(name string) (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.lastSeen[name]
	return t, ok
}

// PeerDownDuration returns how long the peer has been down, or 0 if up.
func (m *Manager) PeerDownDuration(name string) time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.peerUp[name] {
		return 0
	}
	if t, ok := m.lastSeen[name]; ok {
		return time.Since(t)
	}
	return 0 // never seen = we don't know, don't fail over
}

// Architecture returns the local node's architecture.
func (m *Manager) Architecture() string {
	return m.arch
}

// PeerArchitecture returns the architecture of a peer node by reading its
// NodeStatus from the NODE_STATUS bucket. Returns "" if unknown.
func (m *Manager) PeerArchitecture(name string) string {
	var ns NodeStatus
	if _, err := m.store.NodeStatus.GetJSON(context.Background(), name, &ns); err == nil {
		return ns.Architecture
	}
	return ""
}

// PeerNodeStatus returns the full NodeStatus for a peer by reading from the
// NODE_STATUS bucket. Returns nil if not found.
func (m *Manager) PeerNodeStatus(name string) *NodeStatus {
	var ns NodeStatus
	if _, err := m.store.NodeStatus.GetJSON(context.Background(), name, &ns); err == nil {
		return &ns
	}
	return nil
}

// SetPeerAddress updates a peer's address (for tests).
func (m *Manager) SetPeerAddress(name, address string) {
	for i := range m.cfg.Peers {
		if m.cfg.Peers[i].Name == name {
			m.cfg.Peers[i].Address = address
			return
		}
	}
}

func respondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, msg string) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), status)
}
