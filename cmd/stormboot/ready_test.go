package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The reason readiness is a separate check from "the container is running":
// stormblockmk answers 503 with its blockers until every export is wired, and
// starting sbregistry against a not-yet-ready engine just moves the failure.
func TestWaitReadyWaitsForTheServiceNotTheContainer(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"ready":false,"blockers":["exports still pending wiring"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer srv.Close()

	s := &Stage{ReadyURL: srv.URL, ReadyStatus: 200, Timeout: 30 * time.Second}
	if err := waitReady(context.Background(), s); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Errorf("gave up after %d probes; it should have kept waiting", got)
	}
}

// mkube's /healthz answers 200 and then reports its commit, so for that
// endpoint the body is the only thing that distinguishes states.
func TestWaitReadyChecksTheBodyWhenAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\nnode: rose1\ncommit: deadbee\n"))
	}))
	defer srv.Close()

	ok := &Stage{ReadyURL: srv.URL, ReadyStatus: 200, ReadyContains: "commit: deadbee", Timeout: 5 * time.Second}
	if err := waitReady(context.Background(), ok); err != nil {
		t.Fatalf("matching body should be ready: %v", err)
	}

	wrong := &Stage{ReadyURL: srv.URL, ReadyStatus: 200, ReadyContains: "commit: 1234567", Timeout: time.Second}
	if err := waitReady(context.Background(), wrong); err == nil {
		t.Fatal("a body that does not match must not count as ready")
	}
}

func TestWaitReadyTimesOutWithTheReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"blockers":["exports still pending wiring"]}`))
	}))
	defer srv.Close()

	s := &Stage{ReadyURL: srv.URL, ReadyStatus: 200, Timeout: 100 * time.Millisecond}
	err := waitReady(context.Background(), s)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	// The last thing the endpoint said is the whole diagnostic value here.
	if got := err.Error(); !contains(got, "exports still pending wiring") {
		t.Errorf("timeout error lost the reason: %s", got)
	}
}

func TestWaitReadyStopsOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Stage{ReadyURL: srv.URL, ReadyStatus: 200, Timeout: time.Minute}
	if err := waitReady(ctx, s); err == nil {
		t.Fatal("a cancelled context must stop the wait")
	}
}

func contains(h, n string) bool {
	return len(h) >= len(n) && (func() bool {
		for i := 0; i+len(n) <= len(h); i++ {
			if h[i:i+len(n)] == n {
				return true
			}
		}
		return false
	})()
}
