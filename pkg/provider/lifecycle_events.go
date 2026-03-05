package provider

import (
	"sync"
	"time"
)

// Pod lifecycle phase constants.
const (
	PhaseCleanup        = "cleanup"
	PhaseImageResolve   = "image-resolve"
	PhaseNetworkAlloc   = "network-alloc"
	PhaseVolumeMount    = "volume-mount"
	PhaseContainerCreate = "container-create"
	PhaseTarballExtract = "tarball-extract"
	PhaseContainerStart = "container-start"
	PhaseLifecycleReg   = "lifecycle-reg"
	PhaseDNSRegister    = "dns-register"
	PhasePodReady       = "pod-ready"
)

// PhaseTimings records min/max/avg durations per lifecycle phase.
type PhaseTimings struct {
	Count int64   `json:"count"`
	MinMs int64   `json:"min_ms"`
	MaxMs int64   `json:"max_ms"`
	AvgMs float64 `json:"avg_ms"`
	total int64   // internal accumulator
}

// RecoveryStats tracks container recovery events.
type RecoveryStats struct {
	RestartAttempts  int64 `json:"restart_attempts"`
	RestartSuccesses int64 `json:"restart_successes"`
	RecreateAttempts int64 `json:"recreate_attempts"`
	LastRecovery     string `json:"last_recovery,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

// LifecycleStats holds per-phase timing stats and recovery stats.
type LifecycleStats struct {
	mu       sync.Mutex
	Phases   map[string]*PhaseTimings `json:"phases"`
	Recovery RecoveryStats            `json:"recovery"`
}

var globalStats = &LifecycleStats{
	Phases: make(map[string]*PhaseTimings),
}

// GetLifecycleStats returns the global lifecycle stats.
func GetLifecycleStats() *LifecycleStats {
	return globalStats
}

// Record adds a duration measurement for the given phase.
func (ls *LifecycleStats) Record(phase string, d time.Duration) {
	ms := d.Milliseconds()
	ls.mu.Lock()
	defer ls.mu.Unlock()

	pt, ok := ls.Phases[phase]
	if !ok {
		pt = &PhaseTimings{MinMs: ms, MaxMs: ms}
		ls.Phases[phase] = pt
	}
	pt.Count++
	pt.total += ms
	if ms < pt.MinMs {
		pt.MinMs = ms
	}
	if ms > pt.MaxMs {
		pt.MaxMs = ms
	}
	pt.AvgMs = float64(pt.total) / float64(pt.Count)
}

// RecordRestart records a restart attempt and its outcome.
func (ls *LifecycleStats) RecordRestart(success bool, container, comment string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.Recovery.RestartAttempts++
	if success {
		ls.Recovery.RestartSuccesses++
		ls.Recovery.LastRecovery = time.Now().UTC().Format(time.RFC3339) + " " + container
	} else {
		ls.Recovery.LastError = time.Now().UTC().Format(time.RFC3339) + " " + container + ": " + comment
	}
}

// RecordRecreate records a full recreation attempt.
func (ls *LifecycleStats) RecordRecreate(container string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.Recovery.RecreateAttempts++
	ls.Recovery.LastRecovery = time.Now().UTC().Format(time.RFC3339) + " recreated " + container
}

// Snapshot returns a copy of the stats safe for JSON serialization.
func (ls *LifecycleStats) Snapshot() map[string]interface{} {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	phases := make(map[string]*PhaseTimings, len(ls.Phases))
	for k, v := range ls.Phases {
		cp := *v
		phases[k] = &cp
	}
	return map[string]interface{}{
		"phases":   phases,
		"recovery": ls.Recovery,
	}
}

// phaseTracker instruments a single CreatePod invocation.
type phaseTracker struct {
	stats     *LifecycleStats
	phase     string
	startTime time.Time
}

func newPhaseTracker() *phaseTracker {
	return &phaseTracker{stats: globalStats}
}

func (pt *phaseTracker) start(phase string) {
	pt.phase = phase
	pt.startTime = time.Now()
}

func (pt *phaseTracker) done() time.Duration {
	d := time.Since(pt.startTime)
	if pt.phase != "" {
		pt.stats.Record(pt.phase, d)
	}
	return d
}
