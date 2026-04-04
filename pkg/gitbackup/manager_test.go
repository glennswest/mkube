package gitbackup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

func testLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

func TestBuildCommitMessage(t *testing.T) {
	files := []exportedFile{
		{Path: "pods/default.nats.yaml"},
		{Path: "pods/default.dns.yaml"},
		{Path: "configmaps/default.coredns.yaml"},
	}

	msg := buildCommitMessage(files)
	if !strings.HasPrefix(msg, "backup: ") {
		t.Errorf("expected 'backup: ' prefix, got: %s", msg)
	}
	if !strings.Contains(msg, "pods") {
		t.Errorf("expected 'pods' in message, got: %s", msg)
	}
	if !strings.Contains(msg, "configmaps") {
		t.Errorf("expected 'configmaps' in message, got: %s", msg)
	}
}

func TestHashContent(t *testing.T) {
	h1 := hashContent([]byte("hello"))
	h2 := hashContent([]byte("hello"))
	h3 := hashContent([]byte("world"))

	if h1 != h2 {
		t.Error("same content should produce same hash")
	}
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex, got %d chars", len(h1))
	}
}

func TestSecretBucketSkipped(t *testing.T) {
	for _, be := range bucketExports {
		if be.bucketName == "SECRETS" && !be.skip {
			t.Error("SECRETS bucket should be marked as skip")
		}
		if be.bucketName == "NODE_STATUS" && !be.skip {
			t.Error("NODE_STATUS bucket should be marked as skip")
		}
		if be.bucketName == "JOBLOGS" && !be.skip {
			t.Error("JOBLOGS bucket should be marked as skip")
		}
	}
}

func TestBucketExportCompleteness(t *testing.T) {
	// Verify all non-skip buckets have proper kind mappings
	for _, be := range bucketExports {
		if be.skip {
			continue
		}
		kind := kindForBucket(be.bucketName)
		if kind == be.bucketName {
			t.Errorf("bucket %s has no kind mapping (got raw name back)", be.bucketName)
		}
	}
}

func TestKindForBucket(t *testing.T) {
	tests := []struct {
		bucket string
		kind   string
	}{
		{"DEPLOYMENTS", "Deployment"},
		{"BAREMETALHOSTS", "BareMetalHost"},
		{"NETWORKS", "Network"},
		{"REGISTRIES", "Registry"},
		{"ISCSICDROMS", "ISCSICdrom"},
		{"ISCSIDISKS", "ISCSIDisk"},
		{"BOOTCONFIGS", "BootConfig"},
		{"HOSTRESERVATIONS", "HostReservation"},
		{"JOBRUNNERS", "JobRunner"},
		{"STORAGEPOOLS", "StoragePool"},
		{"JOBS", "Job"},
	}

	for _, tt := range tests {
		got := kindForBucket(tt.bucket)
		if got != tt.kind {
			t.Errorf("kindForBucket(%s) = %s, want %s", tt.bucket, got, tt.kind)
		}
	}
}

func TestApiVersionForBucket(t *testing.T) {
	if v := apiVersionForBucket("DEPLOYMENTS"); v != "apps/v1" {
		t.Errorf("expected apps/v1 for DEPLOYMENTS, got %s", v)
	}
	if v := apiVersionForBucket("PODS"); v != "v1" {
		t.Errorf("expected v1 for PODS, got %s", v)
	}
}

func TestDebounceCoalescing(t *testing.T) {
	// Verify that multiple OnStoreChange calls coalesce into triggerCh
	cfg := config.GitBackupConfig{
		Enabled:         true,
		RepoURL:         "http://localhost",
		RepoName:        "test/repo",
		DebounceSeconds: 1,
	}
	m := &Manager{
		cfg:       cfg,
		log:       testLogger(),
		triggerCh: make(chan struct{}, 1),
		hashes:    make(map[string]string),
	}

	// Fire 100 changes rapidly
	for i := 0; i < 100; i++ {
		m.OnStoreChange("PODS", "key", "put", nil)
	}

	// Channel should have exactly 1 message (coalesced)
	select {
	case <-m.triggerCh:
		// Good — got the coalesced trigger
	default:
		t.Error("expected trigger after OnStoreChange")
	}

	// Channel should be empty now
	select {
	case <-m.triggerCh:
		t.Error("expected only one trigger in channel")
	default:
		// Good
	}

	// Pending changes should be 100
	if m.pendingChanges != 100 {
		t.Errorf("expected 100 pending changes, got %d", m.pendingChanges)
	}
}

func TestStatusOutput(t *testing.T) {
	m := &Manager{
		cfg: config.GitBackupConfig{
			Enabled:  true,
			RepoURL:  "http://git.gt.lo:3000",
			RepoName: "mkube/configstate",
			Branch:   "main",
		},
		triggerCh: make(chan struct{}, 1),
		hashes:    make(map[string]string),
	}

	status := m.Status()

	if status["enabled"] != true {
		t.Error("expected enabled=true")
	}
	if status["repoName"] != "mkube/configstate" {
		t.Errorf("expected repoName=mkube/configstate, got %v", status["repoName"])
	}
	if _, ok := status["lastBackup"]; ok {
		t.Error("lastBackup should not be present before first backup")
	}
}

func TestClientPushFile(t *testing.T) {
	var mu sync.Mutex
	pushed := make(map[string]string)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/state/") {
			body, _ := io.ReadAll(r.Body)
			path := strings.TrimPrefix(r.URL.Path, "/api/repos/test/repo/state/")
			mu.Lock()
			pushed[path] = string(body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"abc123","short_id":"abc123"}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/repos" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := newClient(ts.URL, "test/repo", "main", "mkube", "mkube@test", "", "", false)

	_, err := c.pushFile("pods/test.yaml", "test commit", []byte("content"))
	if err != nil {
		t.Fatalf("pushFile failed: %v", err)
	}

	mu.Lock()
	if pushed["pods/test.yaml"] != "content" {
		t.Errorf("expected pushed content 'content', got %q", pushed["pods/test.yaml"])
	}
	mu.Unlock()
}

func TestClientEnsureRepo(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/repos" {
			called = true
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			json.Unmarshal(body, &req)
			if req["visibility"] != "private" {
				t.Errorf("expected private visibility, got %s", req["visibility"])
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c := newClient(ts.URL, "test/repo", "main", "mkube", "mkube@test", "user", "pass", false)
	err := c.ensureRepo()
	if err != nil {
		t.Fatalf("ensureRepo failed: %v", err)
	}
	if !called {
		t.Error("expected repo creation API to be called")
	}
}

func TestAPIHandlers(t *testing.T) {
	m := &Manager{
		cfg: config.GitBackupConfig{
			Enabled:  true,
			RepoURL:  "http://localhost",
			RepoName: "test/repo",
			Branch:   "main",
		},
		triggerCh: make(chan struct{}, 1),
		hashes:    make(map[string]string),
	}

	// Test status endpoint
	t.Run("status", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/gitbackup/status", nil)
		w := httptest.NewRecorder()
		m.handleStatus(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var status map[string]interface{}
		json.NewDecoder(w.Body).Decode(&status)
		if status["enabled"] != true {
			t.Error("expected enabled=true in status")
		}
	})

	// Test trigger endpoint
	t.Run("trigger", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/gitbackup/trigger", nil)
		w := httptest.NewRecorder()
		m.handleTrigger(w, req)

		if w.Code != http.StatusAccepted {
			t.Errorf("expected 202, got %d", w.Code)
		}

		// Should have a trigger in channel
		select {
		case <-m.triggerCh:
			// Good
		case <-time.After(100 * time.Millisecond):
			t.Error("expected trigger in channel after POST")
		}
	})

	// Test method not allowed
	t.Run("wrong method status", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/gitbackup/status", nil)
		w := httptest.NewRecorder()
		m.handleStatus(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("wrong method trigger", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/gitbackup/trigger", nil)
		w := httptest.NewRecorder()
		m.handleTrigger(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})
}
