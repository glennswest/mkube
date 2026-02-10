package routeros

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/glenneth/microkube/pkg/config"
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
		json.NewEncoder(w).Encode(containers)
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
		json.NewEncoder(w).Encode(containers)
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
	var received ContainerSpec
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	})

	client, server := newTestClient(t, handler)
	defer server.Close()

	spec := ContainerSpec{
		Name:      "new-container",
		File:      "/cache/test.tar",
		Interface: "veth-test-0",
		RootDir:   "/data/test",
	}

	err := client.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if received.Name != "new-container" {
		t.Errorf("expected name 'new-container', got %q", received.Name)
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
		if r.URL.Path != "/interface/veth/add" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
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

func TestListVeths(t *testing.T) {
	veths := []NetworkInterface{
		{ID: "*1", Name: "veth0", Address: "172.20.0.5/16"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(veths)
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
