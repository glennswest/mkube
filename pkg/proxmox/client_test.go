package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pveResponse wraps data in a Proxmox-style {"data": ...} envelope.
func pveResponse(data interface{}) []byte {
	b, _ := json.Marshal(map[string]interface{}{"data": data})
	return b
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	// Strip /api2/json suffix that NewClient appends, since the test server
	// URL already includes the path prefix.
	client, err := NewClient(ClientConfig{
		URL:            srv.URL,
		TokenID:        "test@pve!test-token",
		TokenSecret:    "00000000-0000-0000-0000-000000000000",
		Node:           "testnode",
		InsecureVerify: true,
		VMIDRange:      "200-299",
		Storage:        "local",
		RootFSStorage:  "local-lvm",
		RootFSSize:     "8",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Override httpClient to use the test server's TLS client
	client.httpClient = srv.Client()
	// Override baseURL to use the test server
	client.baseURL = srv.URL

	return client
}

func TestListContainers(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/testnode/lxc") {
			http.Error(w, "not found", 404)
			return
		}

		// Verify auth header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "PVEAPIToken=") {
			http.Error(w, "unauthorized", 401)
			return
		}

		containers := []LXCContainer{
			{VMID: 200, Name: "web", Status: "running"},
			{VMID: 201, Name: "db", Status: "stopped"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(containers))
	})

	ctx := context.Background()
	containers, err := client.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	if containers[0].Name != "web" || containers[0].Status != "running" {
		t.Errorf("container[0] = %+v, want web/running", containers[0])
	}
	if containers[1].Name != "db" || containers[1].Status != "stopped" {
		t.Errorf("container[1] = %+v, want db/stopped", containers[1])
	}
}

func TestGetContainerStatus(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/testnode/lxc/200/status/current") {
			http.Error(w, "not found", 404)
			return
		}

		ct := LXCContainer{VMID: 200, Name: "web", Status: "running", Uptime: 3600}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(ct))
	})

	ctx := context.Background()
	ct, err := client.GetContainerStatus(ctx, 200)
	if err != nil {
		t.Fatalf("GetContainerStatus: %v", err)
	}
	if ct.Name != "web" {
		t.Errorf("name = %q, want web", ct.Name)
	}
	if ct.Status != "running" {
		t.Errorf("status = %q, want running", ct.Status)
	}
	if ct.Uptime != 3600 {
		t.Errorf("uptime = %d, want 3600", ct.Uptime)
	}
}

func TestGetContainerConfig(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/testnode/lxc/200/config") {
			http.Error(w, "not found", 404)
			return
		}

		cfg := LXCConfig{
			Hostname:   "web",
			Net0:       "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1",
			Memory:     512,
			Cores:      1,
			Nameserver: "192.168.1.199",
			OnBoot:     1,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(cfg))
	})

	ctx := context.Background()
	cfg, err := client.GetContainerConfig(ctx, 200)
	if err != nil {
		t.Fatalf("GetContainerConfig: %v", err)
	}
	if cfg.Hostname != "web" {
		t.Errorf("hostname = %q, want web", cfg.Hostname)
	}
	if !strings.Contains(cfg.Net0, "ip=192.168.1.10/24") {
		t.Errorf("net0 = %q, want ip=192.168.1.10/24", cfg.Net0)
	}
	if cfg.OnBoot != 1 {
		t.Errorf("onboot = %d, want 1", cfg.OnBoot)
	}
}

func TestGetNodeStatus(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/testnode/status") {
			http.Error(w, "not found", 404)
			return
		}

		status := NodeStatus{
			Uptime:     86400,
			CPU:        0.15,
			PVEVersion: "8.3.2",
		}
		status.CPUInfo.Cores = 8
		status.CPUInfo.Model = "AMD EPYC 7543"
		status.Memory.Total = 68719476736
		status.Memory.Free = 34359738368
		status.RootFS.Total = 500107862016
		status.RootFS.Avail = 250053931008

		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(status))
	})

	ctx := context.Background()
	status, err := client.GetNodeStatus(ctx)
	if err != nil {
		t.Fatalf("GetNodeStatus: %v", err)
	}
	if status.PVEVersion != "8.3.2" {
		t.Errorf("version = %q, want 8.3.2", status.PVEVersion)
	}
	if status.CPUInfo.Cores != 8 {
		t.Errorf("cores = %d, want 8", status.CPUInfo.Cores)
	}
}

func TestGetVersion(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/version") {
			http.Error(w, "not found", 404)
			return
		}

		version := VersionInfo{Release: "8.3", Version: "8.3.2"}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(version))
	})

	ctx := context.Background()
	version, err := client.GetVersion(ctx)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if version.Version != "8.3.2" {
		t.Errorf("version = %q, want 8.3.2", version.Version)
	}
}

func TestListNetworkInterfaces(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes/testnode/network") {
			http.Error(w, "not found", 404)
			return
		}

		ifaces := []NetworkInterface{
			{Name: "vmbr0", Type: "bridge", Active: 1, CIDR: "192.168.1.0/24", Address: "192.168.1.160"},
			{Name: "vmbr1", Type: "bridge", Active: 1, CIDR: "192.168.10.0/24", Address: "192.168.10.1"},
			{Name: "eno1", Type: "eth", Active: 1},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(ifaces))
	})

	ctx := context.Background()
	ifaces, err := client.ListNetworkInterfaces(ctx)
	if err != nil {
		t.Fatalf("ListNetworkInterfaces: %v", err)
	}
	if len(ifaces) != 3 {
		t.Fatalf("expected 3 interfaces, got %d", len(ifaces))
	}

	bridges := 0
	for _, iface := range ifaces {
		if iface.Type == "bridge" {
			bridges++
		}
	}
	if bridges != 2 {
		t.Errorf("expected 2 bridges, got %d", bridges)
	}
}

func TestStartStopContainer(t *testing.T) {
	callCount := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		// Start/stop requests
		if strings.Contains(r.URL.Path, "/status/start") || strings.Contains(r.URL.Path, "/status/stop") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			// Return a UPID
			w.Header().Set("Content-Type", "application/json")
			w.Write(pveResponse("UPID:testnode:00000001:00000002:00000003:vz"))
			return
		}

		// Task status polling
		if strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status") {
			w.Header().Set("Content-Type", "application/json")
			w.Write(pveResponse(map[string]string{
				"status":     "stopped",
				"exitstatus": "OK",
			}))
			return
		}

		http.Error(w, "not found", 404)
	})

	ctx := context.Background()

	if err := client.StartContainer(ctx, 200); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	if err := client.StopContainer(ctx, 200); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
}

func TestAuthHeader(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		expected := "PVEAPIToken=test@pve!test-token=00000000-0000-0000-0000-000000000000"
		if auth != expected {
			t.Errorf("auth header = %q, want %q", auth, expected)
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse([]interface{}{}))
	})

	ctx := context.Background()
	_, err := client.ListContainers(ctx)
	if err != nil {
		t.Fatalf("request with auth: %v", err)
	}
}

func TestHTTPError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "permission denied", 403)
	})

	ctx := context.Background()
	_, err := client.ListContainers(ctx)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestSyncVMIDs(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		containers := []LXCContainer{
			{VMID: 200, Name: "web", Status: "running"},
			{VMID: 205, Name: "dns", Status: "running"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pveResponse(containers))
	})

	ctx := context.Background()
	if err := client.SyncVMIDs(ctx); err != nil {
		t.Fatalf("SyncVMIDs: %v", err)
	}

	// Verify VMIDs are marked as used
	if vmid, ok := client.VMIDs().Lookup("web"); !ok || vmid != 200 {
		t.Errorf("web VMID = %d, ok=%v, want 200/true", vmid, ok)
	}
	if vmid, ok := client.VMIDs().Lookup("dns"); !ok || vmid != 205 {
		t.Errorf("dns VMID = %d, ok=%v, want 205/true", vmid, ok)
	}

	// Next allocation should skip used VMIDs
	vmid, err := client.VMIDs().Allocate("newct")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if vmid != 201 {
		t.Errorf("allocated VMID = %d, want 201 (first free after 200)", vmid)
	}
}

func TestCreateContainer(t *testing.T) {
	var receivedParams map[string]string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Container creation
		if strings.HasSuffix(r.URL.Path, "/lxc") && r.Method == http.MethodPost {
			r.ParseForm()
			receivedParams = make(map[string]string)
			for k, v := range r.Form {
				receivedParams[k] = v[0]
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(pveResponse("UPID:testnode:00000001:00000002:00000003:vzcreate"))
			return
		}

		// Task status polling
		if strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status") {
			w.Header().Set("Content-Type", "application/json")
			w.Write(pveResponse(map[string]string{
				"status":     "stopped",
				"exitstatus": "OK",
			}))
			return
		}

		http.Error(w, fmt.Sprintf("unexpected: %s %s", r.Method, r.URL.Path), 404)
	})

	ctx := context.Background()
	spec := LXCCreateSpec{
		VMID:         200,
		Hostname:     "testct",
		OSTemplate:   "local:vztmpl/test.tar.gz",
		Net0:         "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1",
		Memory:       512,
		Swap:         0,
		Cores:        2,
		RootFS:       "local-lvm:8",
		Unprivileged: true,
		Features:     "nesting=1",
		Nameserver:   "192.168.1.199",
		OnBoot:       true,
		Start:        false,
		MountPoints:  map[int]string{0: "local-lvm:4,mp=/data"},
	}

	if err := client.CreateContainer(ctx, spec); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	if receivedParams["vmid"] != "200" {
		t.Errorf("vmid = %q, want 200", receivedParams["vmid"])
	}
	if receivedParams["hostname"] != "testct" {
		t.Errorf("hostname = %q, want testct", receivedParams["hostname"])
	}
	if receivedParams["unprivileged"] != "1" {
		t.Errorf("unprivileged = %q, want 1", receivedParams["unprivileged"])
	}
	if receivedParams["mp0"] != "local-lvm:4,mp=/data" {
		t.Errorf("mp0 = %q, want local-lvm:4,mp=/data", receivedParams["mp0"])
	}
}
