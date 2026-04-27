package routeros

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glennswest/mkube/pkg/config"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := NewClient(config.RouterOSConfig{
		RESTURL: server.URL,
		User:    "admin",
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	return client, server
}

func TestListContainers(t *testing.T) {
	containers := []Container{
		{ID: "*1", Name: "test-container", Running: "true"},
		{ID: "*2", Name: "another", Stopped: "true"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/container" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	result, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(result))
	}
	if result[0].Name != "test-container" {
		t.Errorf("expected name 'test-container', got %q", result[0].Name)
	}
}

func TestGetContainer(t *testing.T) {
	containers := []Container{
		{ID: "*1", Name: "target", Running: "true"},
		{ID: "*2", Name: "other", Stopped: "true"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	ct, err := client.GetContainer(context.Background(), "target")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ct.ID != "*1" {
		t.Errorf("expected ID '*1', got %q", ct.ID)
	}

	_, err = client.GetContainer(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent container")
	}
}

func TestCreateContainer(t *testing.T) {
	var scriptSource, scriptName string
	var scriptCreated, scriptRun, scriptRemoved bool
	containerReady := false

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/script" && r.Method == http.MethodGet:
			// cleanupStaleScripts lists existing scripts
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]scriptEntry{})
		case r.URL.Path == "/system/script/add":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			scriptSource = body["source"]
			scriptName = body["name"]
			scriptCreated = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"ret": "*A1"})
		case r.URL.Path == "/system/script/run":
			scriptRun = true
			containerReady = true // simulate async extraction completing
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/system/script/remove":
			scriptRemoved = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/container":
			w.Header().Set("Content-Type", "application/json")
			if containerReady {
				_ = json.NewEncoder(w).Encode([]Container{
					{ID: "*1", Name: "new-container", Stopped: "true"},
				})
			} else {
				_ = json.NewEncoder(w).Encode([]Container{})
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	spec := ContainerSpec{
		Name:      "new-container",
		File:      "/cache/test.tar",
		Interface: "veth-test-0",
		RootDir:   "/data/test",
		Logging:   "yes",
	}

	err := client.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scriptCreated {
		t.Error("expected script to be created")
	}
	if !scriptRun {
		t.Error("expected script to be run")
	}
	if !scriptRemoved {
		t.Error("expected script to be cleaned up")
	}
	if !strings.HasPrefix(scriptName, "mkube-task-") {
		t.Errorf("expected script name with mkube-task- prefix, got %q", scriptName)
	}
	expected := "/container/add name=new-container file=/cache/test.tar interface=veth-test-0 root-dir=/data/test logging=yes"
	if scriptSource != expected {
		t.Errorf("unexpected script source:\n got: %s\nwant: %s", scriptSource, expected)
	}
}

func TestCreateContainerCleansUpStaleScripts(t *testing.T) {
	var removedIDs []string
	containerReady := false

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/system/script" && r.Method == http.MethodGet:
			// Return stale scripts from prior runs
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]scriptEntry{
				{ID: "*S1", Name: "mkube-task-000001"},
				{ID: "*S2", Name: "mkube-task-000002"},
				{ID: "*S3", Name: "user-script"}, // should NOT be removed
			})
		case r.URL.Path == "/system/script/add":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"ret": "*A1"})
		case r.URL.Path == "/system/script/run":
			containerReady = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/system/script/remove":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			removedIDs = append(removedIDs, body[".id"])
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/container":
			w.Header().Set("Content-Type", "application/json")
			if containerReady {
				_ = json.NewEncoder(w).Encode([]Container{
					{ID: "*1", Name: "test-ct", Stopped: "true"},
				})
			} else {
				_ = json.NewEncoder(w).Encode([]Container{})
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	spec := ContainerSpec{
		Name:      "test-ct",
		File:      "/cache/test.tar",
		Interface: "veth-test-0",
		RootDir:   "/data/test",
	}

	err := client.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have removed the two stale mkube-task-* scripts, plus the new one after completion
	staleRemoved := 0
	for _, id := range removedIDs {
		if id == "*S1" || id == "*S2" {
			staleRemoved++
		}
		if id == "*S3" {
			t.Error("should not remove non-mkube-task scripts")
		}
	}
	if staleRemoved != 2 {
		t.Errorf("expected 2 stale scripts removed, got %d (removed: %v)", staleRemoved, removedIDs)
	}
}

func TestBuildContainerAddCLI(t *testing.T) {
	spec := ContainerSpec{
		Name:        "test",
		RemoteImage: "192.168.200.3:5000/myapp:latest",
		Interface:   "veth-ns-pod-0",
		RootDir:     "/data/test",
		MountLists:  "mounts-test",
		Cmd:         "/app",
		Entrypoint:  "/bin/sh",
		WorkDir:     "/app",
		Hostname:    "test-host",
		DNS:         "192.168.200.199",
		User:        "nobody",
		Envlist:     "envs-test",
		Logging:     "yes",
		StartOnBoot: "yes",
	}

	result := buildContainerAddCLI(spec)
	expected := "/container/add name=test remote-image=192.168.200.3:5000/myapp:latest" +
		" interface=veth-ns-pod-0 root-dir=/data/test mountlists=mounts-test" +
		" cmd=/app entrypoint=/bin/sh workdir=/app hostname=test-host" +
		" dns=192.168.200.199 user=nobody envlist=envs-test logging=yes start-on-boot=yes"
	if result != expected {
		t.Errorf("unexpected CLI:\n got: %s\nwant: %s", result, expected)
	}
}

func TestStartStopContainer(t *testing.T) {
	var lastPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	if err := client.StartContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("start error: %v", err)
	}
	if lastPath != "/container/start" {
		t.Errorf("expected /container/start, got %s", lastPath)
	}

	if err := client.StopContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("stop error: %v", err)
	}
	if lastPath != "/container/stop" {
		t.Errorf("expected /container/stop, got %s", lastPath)
	}
}

func TestRemoveContainer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/container/remove" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	if err := client.RemoveContainer(context.Background(), "*1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateVeth(t *testing.T) {
	var body map[string]string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/interface/veth":
			// ListVeths — return empty list so CreateVeth proceeds to add
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case "/interface/veth/add":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["name"] != "veth0" {
		t.Errorf("expected name 'veth0', got %q", body["name"])
	}
	if body["address"] != "172.20.0.5/16" {
		t.Errorf("expected address '172.20.0.5/16', got %q", body["address"])
	}
}

func TestCreateVethIdempotent(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/interface/veth":
			// Return existing veth with matching config
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]NetworkInterface{
				{ID: "*1", Name: "veth0", Address: "172.20.0.5/16", Gateway: "172.20.0.1"},
			})
		case "/interface/veth/add":
			t.Error("should not call add when veth already exists")
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateVethUpdate(t *testing.T) {
	var setBody map[string]string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/interface/veth":
			// Return existing veth with different address
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]NetworkInterface{
				{ID: "*1", Name: "veth0", Address: "172.20.0.99/16", Gateway: "172.20.0.1"},
			})
		case "/interface/veth/set":
			_ = json.NewDecoder(r.Body).Decode(&setBody)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	if err := client.CreateVeth(context.Background(), "veth0", "172.20.0.5/16", "172.20.0.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if setBody[".id"] != "*1" {
		t.Errorf("expected .id '*1', got %q", setBody[".id"])
	}
	if setBody["address"] != "172.20.0.5/16" {
		t.Errorf("expected address '172.20.0.5/16', got %q", setBody["address"])
	}
}

func TestListVeths(t *testing.T) {
	veths := []NetworkInterface{
		{ID: "*1", Name: "veth0", Address: "172.20.0.5/16"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(veths)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	result, err := client.ListVeths(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 veth, got %d", len(result))
	}
}

func TestHTTPErrorHandling(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	_, err := client.ListContainers(context.Background())
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestClose(t *testing.T) {
	client, err := NewClient(config.RouterOSConfig{
		RESTURL: "https://localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("unexpected close error: %v", err)
	}
}
