package provider

import (
	"testing"

	"go.uber.org/zap"
)

// An empty desired state must never authorise deleting a running fleet.
//
// This is the shape of the 2026-08-19 outage. mkube restarted, NATS was
// connected — so the only guard passed — but the pods had not streamed in
// yet, so every veth and container matched "no desired pod". The sweep
// removed a running pod's container and left the storage engine's veth
// without a bridge port, which surfaced as `no route to host` and stopped
// anything golden-backed from starting.
func TestSafeToReapRefusesAnEmptyDesiredState(t *testing.T) {
	log := zap.NewNop().Sugar()

	if safeToReap(log, "veths", 0, 26) {
		t.Fatal("nothing desired but 26 running: this must refuse, not delete the node")
	}
	if safeToReap(log, "containers", 0, 1) {
		t.Fatal("even one running resource is enough to distrust an empty desired state")
	}
}

// The guard must not seize up a node that genuinely has nothing to run, or
// real orphans would accumulate forever.
func TestSafeToReapAllowsGenuineCleanup(t *testing.T) {
	log := zap.NewNop().Sugar()

	if !safeToReap(log, "veths", 0, 0) {
		t.Error("nothing desired and nothing running is not a conflict")
	}
	if !safeToReap(log, "veths", 20, 26) {
		t.Error("20 desired against 26 actual is exactly the case reaping exists for")
	}
	if !safeToReap(log, "containers", 26, 26) {
		t.Error("a fully converged node must still pass")
	}
	if !safeToReap(log, "veths", 30, 26) {
		t.Error("more desired than actual is a pending create, not a reason to stop")
	}
}
