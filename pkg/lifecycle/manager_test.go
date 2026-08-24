package lifecycle

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

func testLogger() *zap.SugaredLogger {
	log, _ := zap.NewDevelopment()
	return log.Sugar()
}

func TestRegisterAndUnregister(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	mgr.Register("test", ContainerUnit{
		Name:        "test",
		ContainerID: "*1",
		StartOnBoot: true,
	})

	if len(mgr.units) != 1 {
		t.Errorf("expected 1 unit, got %d", len(mgr.units))
	}

	u := mgr.units["test"]
	if !u.Healthy {
		t.Error("expected healthy=true after registration")
	}
	if !u.StartupComplete {
		t.Error("expected startupComplete=true when no startup probe")
	}
	if !u.Ready {
		t.Error("expected ready=true by default")
	}

	mgr.Unregister("test")
	if len(mgr.units) != 0 {
		t.Errorf("expected 0 units after unregister, got %d", len(mgr.units))
	}
}

func TestRegisterWithStartupProbe(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	mgr.Register("test", ContainerUnit{
		Name:        "test",
		ContainerID: "*1",
		Probes: &ProbeSet{
			Startup: &ProbeConfig{
				Type: "http",
				Path: "/healthz",
				Port: 8080,
			},
		},
	})

	u := mgr.units["test"]
	if u.StartupComplete {
		t.Error("expected startupComplete=false when startup probe is set")
	}
}

func TestGetUnitReady(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	// Unknown unit returns false
	if mgr.GetUnitReady("unknown") {
		t.Error("expected false for unknown unit")
	}

	mgr.Register("test", ContainerUnit{
		Name: "test",
	})

	if !mgr.GetUnitReady("test") {
		t.Error("expected ready=true for registered unit")
	}

	mgr.units["test"].Ready = false
	if mgr.GetUnitReady("test") {
		t.Error("expected ready=false after setting false")
	}
}

func TestBootSequenceOrdering(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	mgr.Register("high", ContainerUnit{Name: "high", Priority: 100})
	mgr.Register("low", ContainerUnit{Name: "low", Priority: 1})
	mgr.Register("mid", ContainerUnit{Name: "mid", Priority: 50})

	seq := mgr.BootSequence()
	if len(seq) != 3 {
		t.Fatalf("expected 3, got %d", len(seq))
	}
	if seq[0].Name != "low" {
		t.Errorf("expected first=low, got %s", seq[0].Name)
	}
	if seq[1].Name != "mid" {
		t.Errorf("expected second=mid, got %s", seq[1].Name)
	}
	if seq[2].Name != "high" {
		t.Errorf("expected third=high, got %s", seq[2].Name)
	}
}

func TestSyncDiscoveredContainers(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	// Pre-register one unit
	mgr.Register("existing", ContainerUnit{Name: "existing"})

	units := []ContainerUnit{
		{Name: "existing", ContainerIP: "1.2.3.4"},
		{Name: "new1", ContainerIP: "1.2.3.5", StartOnBoot: true},
		{Name: "new2", ContainerIP: "1.2.3.6", StartOnBoot: true},
	}

	mgr.SyncDiscoveredContainers(units)

	if len(mgr.units) != 3 {
		t.Errorf("expected 3 units, got %d", len(mgr.units))
	}
	// Existing should not be overwritten
	if mgr.units["existing"].ContainerIP != "" {
		t.Error("existing unit should not be overwritten")
	}
	if mgr.units["new1"].ContainerIP != "1.2.3.5" {
		t.Error("new1 IP wrong")
	}
}

func TestGetUnitStatus(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())
	mgr.Register("a", ContainerUnit{Name: "a", StartOnBoot: true})

	statuses := mgr.GetUnitStatus()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	s := statuses["a"]
	if s.Status != "running" {
		t.Errorf("expected running, got %s", s.Status)
	}
	if !s.Ready {
		t.Error("expected ready=true")
	}
}

func TestShouldRunProbe(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{}, nil, testLogger())

	cfg := &ProbeConfig{PeriodSeconds: 10}
	state := &ProbeState{}

	// First probe should always run
	if !mgr.shouldRunProbe(cfg, state, time.Now()) {
		t.Error("first probe should run")
	}
}

// TestRecreateLoopIsBounded pins the g9 failure mode: a container whose
// liveness probe never passes exhausted its restart budget, fired OnFailed,
// and the resulting pod recreate registered a fresh unit with RestartCount 0 —
// resetting the budget forever. The container was destroyed and its image
// re-extracted every ~26s indefinitely. Recreates must be capped.
func TestRecreateLoopIsBounded(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{
		MaxRestarts:       2,
		RestartCooldown:   0, // no wall-clock wait in tests
		RestartBackoffMax: 1,
		MaxRecreates:      3,
	}, nil, testLogger())

	// Simulate the loop: each pass exhausts restarts and asks to recreate.
	granted := 0
	for i := 0; i < 50; i++ {
		if mgr.shouldRecreate("g9_dns_microdns") {
			granted++
			// A recreate registers a brand-new unit with a zeroed counter.
			mgr.recreates["g9_dns_microdns"].LastAt = time.Time{}
		}
	}

	if granted != 3 {
		t.Fatalf("recreates not capped: granted %d, want 3", granted)
	}
	if mgr.shouldRecreate("g9_dns_microdns") {
		t.Fatal("shouldRecreate still true after cap reached")
	}
}

// TestRecreateBudgetResetsOnRecovery ensures a container that genuinely
// recovers gets a full recovery budget for any later, unrelated failure.
func TestRecreateBudgetResetsOnRecovery(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{
		MaxRecreates: 2, RestartCooldown: 0, RestartBackoffMax: 1,
	}, nil, testLogger())

	if !mgr.shouldRecreate("c") {
		t.Fatal("first recreate should be granted")
	}
	mgr.clearRecreates("c")
	if !mgr.shouldRecreate("c") {
		t.Fatal("budget should reset after recovery")
	}
}

// TestRestartBackoffIsExponentialAndCapped guards the flat-cooldown bug:
// attempts previously waited a constant RestartCooldown regardless of how
// many times the container had already failed.
func TestRestartBackoffIsExponentialAndCapped(t *testing.T) {
	mgr := NewManager(config.LifecycleConfig{
		RestartCooldown: 10, RestartBackoffMax: 300,
	}, nil, testLogger())

	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 300 * time.Second,
		300 * time.Second, // capped
	}
	for i, w := range want {
		if got := mgr.restartBackoff(i); got != w {
			t.Errorf("restartBackoff(%d) = %v, want %v", i, got, w)
		}
	}
}
