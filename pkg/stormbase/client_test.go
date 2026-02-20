package stormbase

import (
	"context"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/glennswest/mkube/pkg/runtime"
	stormdpb "github.com/glennswest/mkube/pkg/stormbase/proto"
)

// mockStormDaemon implements the StormDaemon gRPC server for testing.
type mockStormDaemon struct {
	stormdpb.UnimplementedStormDaemonServer
	workloads []*stormdpb.WorkloadInfo
}

func (m *mockStormDaemon) WorkloadList(_ context.Context, _ *stormdpb.WorkloadListRequest) (*stormdpb.WorkloadListResponse, error) {
	return &stormdpb.WorkloadListResponse{Workloads: m.workloads}, nil
}

func (m *mockStormDaemon) WorkloadCreate(_ context.Context, req *stormdpb.WorkloadCreateRequest) (*stormdpb.WorkloadActionResponse, error) {
	m.workloads = append(m.workloads, &stormdpb.WorkloadInfo{
		Name:          req.Name,
		Kind:          req.Kind,
		Status:        "stopped",
		ContainerId:   "sb-" + req.Name,
		RestartPolicy: req.RestartPolicy,
	})
	return &stormdpb.WorkloadActionResponse{Success: true}, nil
}

func (m *mockStormDaemon) WorkloadStart(_ context.Context, req *stormdpb.WorkloadActionRequest) (*stormdpb.WorkloadActionResponse, error) {
	for _, w := range m.workloads {
		if w.Name == req.Name {
			w.Status = "running"
			return &stormdpb.WorkloadActionResponse{Success: true}, nil
		}
	}
	return &stormdpb.WorkloadActionResponse{Success: false, Message: "not found"}, nil
}

func (m *mockStormDaemon) WorkloadStop(_ context.Context, req *stormdpb.WorkloadActionRequest) (*stormdpb.WorkloadActionResponse, error) {
	for _, w := range m.workloads {
		if w.Name == req.Name {
			w.Status = "stopped"
			return &stormdpb.WorkloadActionResponse{Success: true}, nil
		}
	}
	return &stormdpb.WorkloadActionResponse{Success: false, Message: "not found"}, nil
}

func (m *mockStormDaemon) WorkloadRemove(_ context.Context, req *stormdpb.WorkloadActionRequest) (*stormdpb.WorkloadActionResponse, error) {
	for i, w := range m.workloads {
		if w.Name == req.Name {
			m.workloads = append(m.workloads[:i], m.workloads[i+1:]...)
			return &stormdpb.WorkloadActionResponse{Success: true}, nil
		}
	}
	return &stormdpb.WorkloadActionResponse{Success: false, Message: "not found"}, nil
}

func (m *mockStormDaemon) NodeStatus(_ context.Context, _ *stormdpb.NodeStatusRequest) (*stormdpb.NodeStatusResponse, error) {
	return &stormdpb.NodeStatusResponse{
		Hostname:        "storm-node-1",
		UptimeSecs:      3600,
		WorkloadCount:   uint32(len(m.workloads)),
		RunningCount:    uint32(countRunning(m.workloads)),
		MemoryTotal:     34359738368,
		MemoryAvailable: 17179869184,
		DiskTotal:       500000000000,
		DiskAvailable:   250000000000,
		NodeId:          "node-abc123",
		PodCidr:         "10.42.0.0/24",
	}, nil
}

func (m *mockStormDaemon) ImageEnsure(_ context.Context, req *stormdpb.ImageEnsureRequest) (*stormdpb.ImageEnsureResponse, error) {
	return &stormdpb.ImageEnsureResponse{
		Available:      true,
		ManifestDigest: "sha256:abc123",
	}, nil
}

func countRunning(workloads []*stormdpb.WorkloadInfo) int {
	n := 0
	for _, w := range workloads {
		if w.Status == "running" {
			n++
		}
	}
	return n
}

// startMockServer starts a gRPC server with the mock daemon and returns the client.
func startMockServer(t *testing.T) (*Client, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	srv := grpc.NewServer()
	stormdpb.RegisterStormDaemonServer(srv, &mockStormDaemon{})

	go func() {
		_ = srv.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		t.Fatalf("failed to dial: %v", err)
	}

	client := &Client{
		conn:   conn,
		daemon: stormdpb.NewStormDaemonClient(conn),
	}

	cleanup := func() {
		client.Close()
		srv.Stop()
	}

	return client, cleanup
}

func TestStormBaseContainerLifecycle(t *testing.T) {
	client, cleanup := startMockServer(t)
	defer cleanup()

	ctx := context.Background()

	// Create
	err := client.CreateContainer(ctx, runtime.ContainerSpec{
		Name:          "dns-appliance",
		Image:         "ghcr.io/example/dns:latest",
		RestartPolicy: "always",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	// Get
	ct, err := client.GetContainer(ctx, "dns-appliance")
	if err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	if ct.Name != "dns-appliance" {
		t.Errorf("expected name 'dns-appliance', got %q", ct.Name)
	}
	if ct.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", ct.Status)
	}

	// Start
	err = client.StartContainer(ctx, "dns-appliance")
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	ct, _ = client.GetContainer(ctx, "dns-appliance")
	if ct.Status != "running" {
		t.Errorf("expected running after start, got %q", ct.Status)
	}

	// List
	all, err := client.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 container, got %d", len(all))
	}

	// Stop
	err = client.StopContainer(ctx, "dns-appliance")
	if err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	ct, _ = client.GetContainer(ctx, "dns-appliance")
	if ct.Status != "stopped" {
		t.Errorf("expected stopped after stop, got %q", ct.Status)
	}

	// Remove
	err = client.RemoveContainer(ctx, "dns-appliance")
	if err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	_, err = client.GetContainer(ctx, "dns-appliance")
	if err == nil {
		t.Error("expected error after remove")
	}
}

func TestStormBaseNodeStatus(t *testing.T) {
	client, cleanup := startMockServer(t)
	defer cleanup()

	sr, err := client.GetSystemResource(context.Background())
	if err != nil {
		t.Fatalf("GetSystemResource: %v", err)
	}
	if sr.Hostname != "storm-node-1" {
		t.Errorf("expected hostname 'storm-node-1', got %q", sr.Hostname)
	}
	if sr.Platform != "stormbase" {
		t.Errorf("expected platform 'stormbase', got %q", sr.Platform)
	}
	if sr.TotalMemory != fmt.Sprint(34359738368) {
		t.Errorf("unexpected total memory: %s", sr.TotalMemory)
	}
}

func TestStormBaseImageEnsure(t *testing.T) {
	client, cleanup := startMockServer(t)
	defer cleanup()

	err := client.UploadFile(context.Background(), "ghcr.io/example/dns:latest", nil)
	if err != nil {
		t.Fatalf("UploadFile (ImageEnsure): %v", err)
	}
}

func TestStormBaseBackendIdentifier(t *testing.T) {
	client, cleanup := startMockServer(t)
	defer cleanup()

	if client.Backend() != "stormbase" {
		t.Errorf("expected 'stormbase', got %q", client.Backend())
	}
}

// Verify interface compliance
var _ runtime.ContainerRuntime = (*Client)(nil)
