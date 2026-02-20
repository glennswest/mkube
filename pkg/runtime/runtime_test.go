package runtime_test

import (
	"context"
	"io"
	"testing"

	"github.com/glennswest/mkube/pkg/runtime"
)

// mockRuntime is a minimal ContainerRuntime for interface compliance testing.
type mockRuntime struct {
	containers map[string]*runtime.Container
	backend    string
}

func newMockRuntime(backend string) *mockRuntime {
	return &mockRuntime{
		containers: make(map[string]*runtime.Container),
		backend:    backend,
	}
}

func (m *mockRuntime) CreateContainer(_ context.Context, spec runtime.ContainerSpec) error {
	m.containers[spec.Name] = &runtime.Container{
		ID:     "mock-" + spec.Name,
		Name:   spec.Name,
		Image:  spec.Image,
		Status: "stopped",
	}
	return nil
}

func (m *mockRuntime) StartContainer(_ context.Context, id string) error {
	for _, c := range m.containers {
		if c.ID == id {
			c.Status = "running"
			return nil
		}
	}
	return runtime.ErrNotFound
}

func (m *mockRuntime) StopContainer(_ context.Context, id string) error {
	for _, c := range m.containers {
		if c.ID == id {
			c.Status = "stopped"
			return nil
		}
	}
	return runtime.ErrNotFound
}

func (m *mockRuntime) RemoveContainer(_ context.Context, id string) error {
	for name, c := range m.containers {
		if c.ID == id {
			delete(m.containers, name)
			return nil
		}
	}
	return runtime.ErrNotFound
}

func (m *mockRuntime) GetContainer(_ context.Context, name string) (*runtime.Container, error) {
	if c, ok := m.containers[name]; ok {
		return c, nil
	}
	return nil, runtime.ErrNotFound
}

func (m *mockRuntime) ListContainers(_ context.Context) ([]runtime.Container, error) {
	var out []runtime.Container
	for _, c := range m.containers {
		out = append(out, *c)
	}
	return out, nil
}

func (m *mockRuntime) GetLogs(_ context.Context, _ string) ([]runtime.LogEntry, error) {
	return nil, nil
}

func (m *mockRuntime) GetSystemResource(_ context.Context) (*runtime.SystemResource, error) {
	return &runtime.SystemResource{Platform: m.backend}, nil
}

func (m *mockRuntime) UploadFile(_ context.Context, _ string, _ io.Reader) error { return nil }
func (m *mockRuntime) RemoveFile(_ context.Context, _ string) error              { return nil }
func (m *mockRuntime) CreateMount(_ context.Context, _, _, _ string) error       { return nil }
func (m *mockRuntime) RemoveMountsByList(_ context.Context, _ string) error      { return nil }
func (m *mockRuntime) Backend() string                                           { return m.backend }
func (m *mockRuntime) Close() error                                              { return nil }

// Compile-time check that mockRuntime implements ContainerRuntime.
var _ runtime.ContainerRuntime = (*mockRuntime)(nil)

func TestContainerLifecycle(t *testing.T) {
	ctx := context.Background()
	rt := newMockRuntime("test")

	// Create
	err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:  "web",
		Image: "nginx:latest",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	// Get
	ct, err := rt.GetContainer(ctx, "web")
	if err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	if ct.Status != "stopped" {
		t.Errorf("expected stopped, got %s", ct.Status)
	}

	// Start
	if err := rt.StartContainer(ctx, ct.ID); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	ct, _ = rt.GetContainer(ctx, "web")
	if !ct.IsRunning() {
		t.Error("expected running after start")
	}

	// Stop
	if err := rt.StopContainer(ctx, ct.ID); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	ct, _ = rt.GetContainer(ctx, "web")
	if !ct.IsStopped() {
		t.Error("expected stopped after stop")
	}

	// List
	all, err := rt.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 container, got %d", len(all))
	}

	// Remove
	if err := rt.RemoveContainer(ctx, ct.ID); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	_, err = rt.GetContainer(ctx, "web")
	if err == nil {
		t.Error("expected error after remove")
	}
}

func TestContainerSpec_StormBaseFields(t *testing.T) {
	spec := runtime.ContainerSpec{
		Name:          "dns-appliance",
		Image:         "ghcr.io/example/dns:latest",
		RestartPolicy: "always",
		CPULimit:      "2",
		MemoryLimit:   "512Mi",
		Command:       []string{"/usr/bin/dns-server", "--config=/etc/dns.toml"},
		Env:           []string{"LOG_LEVEL=debug"},
		Ports: []runtime.PortMapping{
			{HostPort: 53, ContainerPort: 53, Protocol: "udp"},
		},
		Volumes: []runtime.VolumeMount{
			{Source: "/data/dns", Target: "/var/lib/dns", ReadOnly: false},
		},
	}

	if spec.Name != "dns-appliance" {
		t.Errorf("unexpected name: %s", spec.Name)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].Protocol != "udp" {
		t.Error("port mapping not preserved")
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Source != "/data/dns" {
		t.Error("volume mount not preserved")
	}
}

func TestBackendIdentification(t *testing.T) {
	ros := newMockRuntime("routeros")
	sb := newMockRuntime("stormbase")

	if ros.Backend() != "routeros" {
		t.Errorf("expected routeros, got %s", ros.Backend())
	}
	if sb.Backend() != "stormbase" {
		t.Errorf("expected stormbase, got %s", sb.Backend())
	}
}
