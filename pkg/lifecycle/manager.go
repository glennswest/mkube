package lifecycle

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/routeros"
)

// Manager handles the full container lifecycle on RouterOS:
//   - Keepalive: restarts start-on-boot containers that stop
//   - Probes: K8s-style startup/liveness/readiness probes
//   - Boot ordering: containers start in priority order
//   - Restart policies: configurable backoff and max restarts
type Manager struct {
	cfg config.LifecycleConfig
	ros *routeros.Client
	log *zap.SugaredLogger

	mu    sync.RWMutex
	units map[string]*ContainerUnit
}

// ProbeConfig defines a single health probe.
type ProbeConfig struct {
	Type                string   // "http", "tcp", "exec"
	Path                string   // HTTP path (for type=http)
	Port                int
	Command             []string // exec probe
	InitialDelaySeconds int
	PeriodSeconds       int // default 10
	TimeoutSeconds      int // default 1
	FailureThreshold    int // default 3
	SuccessThreshold    int // default 1
}

// ProbeState tracks consecutive successes/failures for a single probe.
type ProbeState struct {
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	LastProbeTime        time.Time
}

// ProbeSet holds optional startup, liveness, and readiness probes.
type ProbeSet struct {
	Startup   *ProbeConfig
	Liveness  *ProbeConfig
	Readiness *ProbeConfig
}

// ContainerUnit represents a managed container with lifecycle properties.
type ContainerUnit struct {
	Name          string
	ContainerID   string
	ContainerIP   string // IP address of the container (probes target this)
	RestartPolicy string // "Always", "OnFailure", "Never"
	DependsOn     []string
	Priority      int

	// Lifecycle config
	StartOnBoot bool
	Managed     bool // true if created by CreatePod (vs discovered)
	Probes      *ProbeSet

	// Legacy — kept for backward compat with old registration calls
	HealthCheck *HealthCheck

	// Runtime state
	RestartCount    int
	LastRestartAt   time.Time
	LastHealthCheck time.Time
	Healthy         bool
	Ready           bool
	StartupComplete bool
	Status          string // "running", "stopped", "restarting", "failed"

	// Per-probe state
	StartupState   ProbeState
	LivenessState  ProbeState
	ReadinessState ProbeState
}

// HealthCheck is the legacy probe format (kept for backward compat).
type HealthCheck struct {
	Type     string
	Path     string
	Port     int
	Interval int
	Timeout  int
	Retries  int
}

// NewManager creates a new lifecycle manager.
func NewManager(cfg config.LifecycleConfig, ros *routeros.Client, log *zap.SugaredLogger) *Manager {
	return &Manager{
		cfg:   cfg,
		ros:   ros,
		log:   log,
		units: make(map[string]*ContainerUnit),
	}
}

// Register adds a container to the lifecycle manager.
func (m *Manager) Register(name string, unit ContainerUnit) {
	m.mu.Lock()
	defer m.mu.Unlock()

	unit.Healthy = true
	unit.Status = "running"

	// If no explicit startup probe, mark startup as complete
	if unit.Probes == nil || unit.Probes.Startup == nil {
		unit.StartupComplete = true
	}

	// Default readiness to true until a readiness probe says otherwise
	unit.Ready = true

	m.units[name] = &unit
	m.log.Infow("registered container unit", "name", name, "priority", unit.Priority,
		"startOnBoot", unit.StartOnBoot, "hasProbes", unit.Probes != nil)
}

// Unregister removes a container from the lifecycle manager.
func (m *Manager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.units, name)
	m.log.Infow("unregistered container unit", "name", name)
}

// GetUnitReady returns the readiness state of a unit.
func (m *Manager) GetUnitReady(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if unit, ok := m.units[name]; ok {
		return unit.Ready
	}
	return false
}

// ─── Boot Ordering ──────────────────────────────────────────────────────────

// BootSequence returns containers sorted by boot priority, respecting
// dependency ordering (topological sort with priority as tiebreaker).
func (m *Manager) BootSequence() []*ContainerUnit {
	m.mu.RLock()
	defer m.mu.RUnlock()

	units := make([]*ContainerUnit, 0, len(m.units))
	for _, u := range m.units {
		units = append(units, u)
	}

	return topoSort(units, m.units)
}

// ExecuteBootSequence starts all registered containers in dependency order.
func (m *Manager) ExecuteBootSequence(ctx context.Context) error {
	sequence := m.BootSequence()
	m.log.Infow("executing boot sequence", "containers", len(sequence))

	for i, unit := range sequence {
		m.log.Infow("booting container",
			"order", i+1,
			"name", unit.Name,
			"priority", unit.Priority,
			"depends_on", unit.DependsOn,
		)

		if err := m.ros.StartContainer(ctx, unit.ContainerID); err != nil {
			m.log.Errorw("failed to start container during boot",
				"name", unit.Name, "error", err)
			continue
		}

		// Wait for container to be healthy before proceeding to dependents
		if unit.HealthCheck != nil {
			if err := m.waitForHealthy(ctx, unit, 30*time.Second); err != nil {
				m.log.Warnw("container not healthy after boot, continuing",
					"name", unit.Name, "error", err)
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	m.log.Info("boot sequence complete")
	return nil
}

// ─── Watchdog (Three-Phase Probe Loop) ──────────────────────────────────────

// RunWatchdog runs the three-phase probe loop: keepalive → startup → liveness+readiness.
func (m *Manager) RunWatchdog(ctx context.Context) {
	interval := time.Duration(m.cfg.WatchdogInterval) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.log.Infow("watchdog started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			m.log.Info("watchdog shutting down")
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *Manager) checkAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	for name, unit := range m.units {
		// Phase 1: KEEPALIVE — restart stopped start-on-boot containers
		if unit.StartOnBoot {
			m.handleKeepalive(ctx, name, unit)
		}

		// Phase 2: STARTUP PROBES — for units not yet startup-complete
		if !unit.StartupComplete && unit.Probes != nil && unit.Probes.Startup != nil {
			probe := unit.Probes.Startup
			if m.shouldRunProbe(probe, &unit.StartupState, now) {
				ok := m.runProbe(ctx, probe, unit.ContainerIP)
				unit.StartupState.LastProbeTime = now
				if ok {
					unit.StartupState.ConsecutiveSuccesses++
					unit.StartupState.ConsecutiveFailures = 0
					threshold := probe.SuccessThreshold
					if threshold == 0 {
						threshold = 1
					}
					if unit.StartupState.ConsecutiveSuccesses >= threshold {
						unit.StartupComplete = true
						m.log.Infow("startup probe passed", "name", name)
					}
				} else {
					unit.StartupState.ConsecutiveFailures++
					unit.StartupState.ConsecutiveSuccesses = 0
					threshold := probe.FailureThreshold
					if threshold == 0 {
						threshold = 3
					}
					if unit.StartupState.ConsecutiveFailures >= threshold {
						m.log.Warnw("startup probe failed, restarting", "name", name,
							"failures", unit.StartupState.ConsecutiveFailures)
						m.restartUnit(ctx, unit)
						unit.StartupState = ProbeState{}
						unit.StartupComplete = false
					}
				}
			}
			continue // skip liveness/readiness until startup completes
		}

		// Phase 3: LIVENESS + READINESS
		if unit.Probes != nil && unit.Probes.Liveness != nil {
			probe := unit.Probes.Liveness
			if m.shouldRunProbe(probe, &unit.LivenessState, now) {
				ok := m.runProbe(ctx, probe, unit.ContainerIP)
				unit.LivenessState.LastProbeTime = now
				unit.LastHealthCheck = now
				if ok {
					unit.LivenessState.ConsecutiveSuccesses++
					unit.LivenessState.ConsecutiveFailures = 0
					if !unit.Healthy {
						unit.Healthy = true
						unit.Status = "running"
						m.log.Infow("container recovered (liveness)", "name", name)
					}
				} else {
					unit.LivenessState.ConsecutiveFailures++
					unit.LivenessState.ConsecutiveSuccesses = 0
					threshold := probe.FailureThreshold
					if threshold == 0 {
						threshold = 3
					}
					if unit.LivenessState.ConsecutiveFailures >= threshold {
						unit.Healthy = false
						m.log.Warnw("liveness probe failed, restarting", "name", name,
							"failures", unit.LivenessState.ConsecutiveFailures)
						m.handleUnhealthy(ctx, unit)
						unit.LivenessState = ProbeState{}
					}
				}
			}
		} else if unit.HealthCheck != nil {
			// Legacy health check path
			m.runLegacyHealthCheck(ctx, name, unit, now)
		}

		if unit.Probes != nil && unit.Probes.Readiness != nil {
			probe := unit.Probes.Readiness
			if m.shouldRunProbe(probe, &unit.ReadinessState, now) {
				ok := m.runProbe(ctx, probe, unit.ContainerIP)
				unit.ReadinessState.LastProbeTime = now
				if ok {
					unit.ReadinessState.ConsecutiveSuccesses++
					unit.ReadinessState.ConsecutiveFailures = 0
					threshold := probe.SuccessThreshold
					if threshold == 0 {
						threshold = 1
					}
					if unit.ReadinessState.ConsecutiveSuccesses >= threshold && !unit.Ready {
						unit.Ready = true
						m.log.Infow("container became ready", "name", name)
					}
				} else {
					unit.ReadinessState.ConsecutiveFailures++
					unit.ReadinessState.ConsecutiveSuccesses = 0
					threshold := probe.FailureThreshold
					if threshold == 0 {
						threshold = 3
					}
					if unit.ReadinessState.ConsecutiveFailures >= threshold && unit.Ready {
						unit.Ready = false
						m.log.Warnw("readiness probe failed, marking not-ready", "name", name)
						// No restart — readiness failures only affect Ready state
					}
				}
			}
		}
	}
}

// handleKeepalive checks if a start-on-boot container is stopped and restarts it.
func (m *Manager) handleKeepalive(ctx context.Context, name string, unit *ContainerUnit) {
	ct, err := m.ros.GetContainer(ctx, unit.Name)
	if err != nil {
		return
	}
	if ct.IsStopped() && unit.Status != "failed" {
		cooldown := time.Duration(m.cfg.RestartCooldown) * time.Second
		if cooldown == 0 {
			cooldown = 10 * time.Second
		}
		if time.Since(unit.LastRestartAt) < cooldown {
			return
		}
		m.log.Infow("keepalive: restarting stopped container", "name", name)
		if err := m.ros.StartContainer(ctx, ct.ID); err != nil {
			m.log.Errorw("keepalive: failed to start container", "name", name, "error", err)
			return
		}
		unit.RestartCount++
		unit.LastRestartAt = time.Now()
		unit.Status = "running"
	}
}

// shouldRunProbe returns true if enough time has passed since the last probe.
func (m *Manager) shouldRunProbe(cfg *ProbeConfig, state *ProbeState, now time.Time) bool {
	period := time.Duration(cfg.PeriodSeconds) * time.Second
	if period == 0 {
		period = 10 * time.Second
	}
	// Respect initial delay on first run
	if state.LastProbeTime.IsZero() {
		delay := time.Duration(cfg.InitialDelaySeconds) * time.Second
		// We don't track container start time per-probe, so just run after first period
		_ = delay
		return true
	}
	return now.Sub(state.LastProbeTime) >= period
}

// runProbe executes a single probe against a container's IP.
func (m *Manager) runProbe(ctx context.Context, cfg *ProbeConfig, containerIP string) bool {
	if containerIP == "" {
		return false
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 1 * time.Second
	}

	switch cfg.Type {
	case "http":
		return m.httpProbe(ctx, containerIP, cfg.Port, cfg.Path, timeout)
	case "tcp":
		return m.tcpProbe(ctx, containerIP, cfg.Port, timeout)
	default:
		return false
	}
}

func (m *Manager) httpProbe(ctx context.Context, ip string, port int, path string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s:%d%s", ip, port, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func (m *Manager) tcpProbe(ctx context.Context, ip string, port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// runLegacyHealthCheck handles the old HealthCheck format.
func (m *Manager) runLegacyHealthCheck(ctx context.Context, name string, unit *ContainerUnit, now time.Time) {
	healthy := m.performLegacyHealthCheck(ctx, unit)
	wasHealthy := unit.Healthy
	unit.Healthy = healthy
	unit.LastHealthCheck = now

	if wasHealthy && !healthy {
		m.log.Warnw("container became unhealthy", "name", name)
		m.handleUnhealthy(ctx, unit)
	} else if !wasHealthy && healthy {
		m.log.Infow("container recovered", "name", name)
		unit.Status = "running"
	}
}

func (m *Manager) performLegacyHealthCheck(ctx context.Context, unit *ContainerUnit) bool {
	ip := unit.ContainerIP
	if ip == "" {
		ip = "localhost"
	}

	switch unit.HealthCheck.Type {
	case "http":
		timeout := time.Duration(unit.HealthCheck.Timeout) * time.Second
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		return m.httpProbe(ctx, ip, unit.HealthCheck.Port, unit.HealthCheck.Path, timeout)
	case "tcp":
		timeout := time.Duration(unit.HealthCheck.Timeout) * time.Second
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		return m.tcpProbe(ctx, ip, unit.HealthCheck.Port, timeout)
	default:
		return m.statusCheck(ctx, unit)
	}
}

func (m *Manager) statusCheck(ctx context.Context, unit *ContainerUnit) bool {
	ct, err := m.ros.GetContainer(ctx, unit.Name)
	if err != nil {
		return false
	}
	return ct.IsRunning()
}

// ─── Restart Handling ───────────────────────────────────────────────────────

func (m *Manager) restartUnit(ctx context.Context, unit *ContainerUnit) {
	maxRestarts := m.cfg.MaxRestarts
	if maxRestarts == 0 {
		maxRestarts = 5
	}

	if unit.RestartCount >= maxRestarts {
		m.log.Errorw("container exceeded max restarts, marking as failed",
			"name", unit.Name, "restarts", unit.RestartCount, "max", maxRestarts)
		unit.Status = "failed"
		return
	}

	cooldown := time.Duration(m.cfg.RestartCooldown) * time.Second
	if cooldown == 0 {
		cooldown = 10 * time.Second
	}
	if time.Since(unit.LastRestartAt) < cooldown {
		return
	}

	m.log.Infow("restarting container", "name", unit.Name, "attempt", unit.RestartCount+1)
	unit.Status = "restarting"

	_ = m.ros.StopContainer(ctx, unit.ContainerID)
	time.Sleep(2 * time.Second)

	if err := m.ros.StartContainer(ctx, unit.ContainerID); err != nil {
		m.log.Errorw("failed to restart container", "name", unit.Name, "error", err)
	}

	unit.RestartCount++
	unit.LastRestartAt = time.Now()
}

func (m *Manager) handleUnhealthy(ctx context.Context, unit *ContainerUnit) {
	switch unit.RestartPolicy {
	case "Always", "OnFailure":
		m.restartUnit(ctx, unit)
	case "Never":
		m.log.Infow("container unhealthy but restart policy is Never", "name", unit.Name)
		unit.Status = "stopped"
	}
}

// ─── Discovery Sync ─────────────────────────────────────────────────────────

// SyncDiscoveredContainers registers discovered containers for keepalive
// and auto-probes. Existing registrations are not overwritten.
func (m *Manager) SyncDiscoveredContainers(units []ContainerUnit) {
	m.mu.Lock()
	defer m.mu.Unlock()

	added := 0
	for _, unit := range units {
		if _, exists := m.units[unit.Name]; exists {
			continue
		}
		u := unit // copy
		u.Healthy = true
		u.Status = "running"
		if u.Probes == nil || u.Probes.Startup == nil {
			u.StartupComplete = true
		}
		u.Ready = true
		m.units[u.Name] = &u
		added++
	}

	if added > 0 {
		m.log.Infow("synced discovered containers", "added", added, "total", len(m.units))
	}
}

// ─── Wait Helpers ───────────────────────────────────────────────────────────

func (m *Manager) waitForHealthy(ctx context.Context, unit *ContainerUnit, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for %s to become healthy", unit.Name)
		case <-tick.C:
			if m.performLegacyHealthCheck(ctx, unit) {
				return nil
			}
		}
	}
}

// GetUnitStatus returns the status of all managed units.
func (m *Manager) GetUnitStatus() map[string]UnitStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]UnitStatus, len(m.units))
	for name, unit := range m.units {
		result[name] = UnitStatus{
			Name:            name,
			Status:          unit.Status,
			Healthy:         unit.Healthy,
			Ready:           unit.Ready,
			StartupComplete: unit.StartupComplete,
			RestartCount:    unit.RestartCount,
			LastRestart:     unit.LastRestartAt,
			LastHealthCheck: unit.LastHealthCheck,
		}
	}
	return result
}

// UnitStatus is an exported snapshot of a unit's runtime state.
type UnitStatus struct {
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	Healthy         bool      `json:"healthy"`
	Ready           bool      `json:"ready"`
	StartupComplete bool      `json:"startupComplete"`
	RestartCount    int       `json:"restartCount"`
	LastRestart     time.Time `json:"lastRestart,omitempty"`
	LastHealthCheck time.Time `json:"lastHealthCheck,omitempty"`
}

// ─── Topological Sort ───────────────────────────────────────────────────────

func topoSort(units []*ContainerUnit, unitMap map[string]*ContainerUnit) []*ContainerUnit {
	sort.Slice(units, func(i, j int) bool {
		return units[i].Priority < units[j].Priority
	})

	visited := make(map[string]bool)
	var result []*ContainerUnit

	var visit func(u *ContainerUnit)
	visit = func(u *ContainerUnit) {
		if visited[u.Name] {
			return
		}
		visited[u.Name] = true

		for _, dep := range u.DependsOn {
			if depUnit, ok := unitMap[dep]; ok {
				visit(depUnit)
			}
		}

		result = append(result, u)
	}

	for _, u := range units {
		visit(u)
	}

	return result
}
