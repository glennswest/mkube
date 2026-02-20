package provider

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/runtime"
)

// ─── Mock Runtime ────────────────────────────────────────────────────────────

type mockRuntime struct {
	mu         sync.Mutex
	containers map[string]*runtime.Container
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{containers: make(map[string]*runtime.Container)}
}

func (m *mockRuntime) CreateContainer(_ context.Context, spec runtime.ContainerSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers[spec.Name] = &runtime.Container{
		ID:        spec.Name,
		Name:      spec.Name,
		Image:     spec.Image,
		Interface: spec.Interface,
		RootDir:   spec.RootDir,
		Status:    "stopped", // extraction complete
		Hostname:  spec.Hostname,
		DNS:       spec.DNS,
	}
	return nil
}

func (m *mockRuntime) StartContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ct, ok := m.containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	ct.Status = "running"
	return nil
}

func (m *mockRuntime) StopContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ct, ok := m.containers[id]
	if !ok {
		return fmt.Errorf("container %s not found", id)
	}
	ct.Status = "stopped"
	return nil
}

func (m *mockRuntime) RemoveContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.containers, id)
	return nil
}

func (m *mockRuntime) GetContainer(_ context.Context, name string) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ct, ok := m.containers[name]
	if !ok {
		return nil, runtime.ErrNotFound
	}
	cp := *ct
	return &cp, nil
}

func (m *mockRuntime) ListContainers(_ context.Context) ([]runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]runtime.Container, 0, len(m.containers))
	for _, ct := range m.containers {
		out = append(out, *ct)
	}
	return out, nil
}

func (m *mockRuntime) GetLogs(context.Context, string) ([]runtime.LogEntry, error) { return nil, nil }
func (m *mockRuntime) GetSystemResource(context.Context) (*runtime.SystemResource, error) {
	return nil, nil
}
func (m *mockRuntime) UploadFile(context.Context, string, io.Reader) error       { return nil }
func (m *mockRuntime) RemoveFile(context.Context, string) error                  { return nil }
func (m *mockRuntime) CreateMount(context.Context, string, string, string) error { return nil }
func (m *mockRuntime) RemoveMountsByList(context.Context, string) error          { return nil }
func (m *mockRuntime) Backend() string                                           { return "stormbase" }
func (m *mockRuntime) Close() error                                              { return nil }

// ─── Mock Network Driver ─────────────────────────────────────────────────────

type mockNetworkDriver struct{}

func (d *mockNetworkDriver) CreateBridge(context.Context, string, network.BridgeOpts) error {
	return nil
}
func (d *mockNetworkDriver) DeleteBridge(context.Context, string) error { return nil }
func (d *mockNetworkDriver) ListBridges(context.Context) ([]network.BridgeInfo, error) {
	return nil, nil
}
func (d *mockNetworkDriver) CreatePort(context.Context, string, string, string) error { return nil }
func (d *mockNetworkDriver) DeletePort(context.Context, string) error                 { return nil }
func (d *mockNetworkDriver) AttachPort(context.Context, string, string) error         { return nil }
func (d *mockNetworkDriver) DetachPort(context.Context, string, string) error         { return nil }
func (d *mockNetworkDriver) ListPorts(context.Context) ([]network.PortInfo, error)    { return nil, nil }
func (d *mockNetworkDriver) SetPortVLAN(context.Context, string, int, bool) error     { return nil }
func (d *mockNetworkDriver) RemovePortVLAN(context.Context, string, int) error        { return nil }
func (d *mockNetworkDriver) CreateTunnel(context.Context, string, network.TunnelSpec) error {
	return nil
}
func (d *mockNetworkDriver) DeleteTunnel(context.Context, string) error { return nil }
func (d *mockNetworkDriver) NodeName() string                           { return "test-node" }
func (d *mockNetworkDriver) Capabilities() network.DriverCapabilities {
	return network.DriverCapabilities{}
}

// ─── Test Helper ─────────────────────────────────────────────────────────────

func newTestProvider(t *testing.T) (*MicroKubeProvider, *mockRuntime) {
	t.Helper()

	rt := newMockRuntime()
	log := zap.NewNop().Sugar()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		NodeName: "test-node",
		Networks: []config.NetworkDef{
			{
				Name:    "containers",
				Bridge:  "br-test",
				CIDR:    "172.20.0.0/24",
				Gateway: "172.20.0.1",
			},
		},
		Storage: config.StorageConfig{
			BasePath: tmpDir,
		},
	}

	netMgr, err := network.NewManager(cfg.Networks, &mockNetworkDriver{}, nil, log)
	if err != nil {
		t.Fatalf("network.NewManager: %v", err)
	}

	lcMgr := lifecycle.NewManager(config.LifecycleConfig{}, nil, log)

	p, err := NewMicroKubeProvider(Deps{
		Config:       cfg,
		Runtime:      rt,
		NetworkMgr:   netMgr,
		LifecycleMgr: lcMgr,
		Logger:       log,
	})
	if err != nil {
		t.Fatalf("NewMicroKubeProvider: %v", err)
	}

	return p, rt
}

func testPod(name string, containers ...string) *corev1.Pod {
	var cs []corev1.Container
	for _, c := range containers {
		cs = append(cs, corev1.Container{Name: c, Image: c + ":latest"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				"vkube.io/file": "/dev/null",
			},
		},
		Spec: corev1.PodSpec{
			Containers:    cs,
			RestartPolicy: corev1.RestartPolicyAlways,
		},
	}
}

// ─── Integration Tests ───────────────────────────────────────────────────────

func TestProviderPodCreateAndDelete(t *testing.T) {
	p, rt := newTestProvider(t)
	ctx := context.Background()
	pod := testPod("myapp", "web")

	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Pod should be tracked
	if _, ok := p.pods["default/myapp"]; !ok {
		t.Fatal("pod not tracked after create")
	}

	// Container should exist and be running
	ct, err := rt.GetContainer(ctx, sanitizeName(pod, "web"))
	if err != nil {
		t.Fatalf("container not found in runtime: %v", err)
	}
	if !ct.IsRunning() {
		t.Errorf("expected running, got %s", ct.Status)
	}

	// GetPodStatus should return Running
	status, err := p.GetPodStatus(ctx, "default", "myapp")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Errorf("expected PodRunning, got %s", status.Phase)
	}

	// Delete
	if err := p.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}

	// Pod should no longer be tracked
	if _, ok := p.pods["default/myapp"]; ok {
		t.Error("pod still tracked after delete")
	}

	// Container should be gone from runtime
	if _, err := rt.GetContainer(ctx, sanitizeName(pod, "web")); err == nil {
		t.Error("container still in runtime after delete")
	}
}

func TestProviderPodStatusReflectsRuntime(t *testing.T) {
	p, rt := newTestProvider(t)
	ctx := context.Background()
	pod := testPod("statusapp", "server")

	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Running
	status, err := p.GetPodStatus(ctx, "default", "statusapp")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Errorf("expected PodRunning, got %s", status.Phase)
	}

	// Stop the container directly
	name := sanitizeName(pod, "server")
	if err := rt.StopContainer(ctx, name); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	// Status should reflect stopped → Pending with Terminated state
	status, err = p.GetPodStatus(ctx, "default", "statusapp")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if status.Phase != corev1.PodPending {
		t.Errorf("expected PodPending, got %s", status.Phase)
	}
	if len(status.ContainerStatuses) != 1 {
		t.Fatalf("expected 1 container status, got %d", len(status.ContainerStatuses))
	}
	cs := status.ContainerStatuses[0]
	if cs.State.Terminated == nil {
		t.Error("expected Terminated state for stopped container")
	}
}

func TestProviderMultiContainerPod(t *testing.T) {
	p, rt := newTestProvider(t)
	ctx := context.Background()
	pod := testPod("multi", "frontend", "backend")

	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Both containers should exist
	for _, cName := range []string{"frontend", "backend"} {
		rosName := sanitizeName(pod, cName)
		ct, err := rt.GetContainer(ctx, rosName)
		if err != nil {
			t.Fatalf("container %s not found: %v", rosName, err)
		}
		if !ct.IsRunning() {
			t.Errorf("container %s: expected running, got %s", rosName, ct.Status)
		}
	}

	// Status should show 2 container statuses
	status, err := p.GetPodStatus(ctx, "default", "multi")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if len(status.ContainerStatuses) != 2 {
		t.Errorf("expected 2 container statuses, got %d", len(status.ContainerStatuses))
	}

	// Containers should have different interfaces (names are different in runtime)
	fe, _ := rt.GetContainer(ctx, sanitizeName(pod, "frontend"))
	be, _ := rt.GetContainer(ctx, sanitizeName(pod, "backend"))
	if fe.Interface == be.Interface {
		t.Errorf("expected different interfaces, both got %s", fe.Interface)
	}
}

func TestProviderUpdatePod(t *testing.T) {
	p, rt := newTestProvider(t)
	ctx := context.Background()
	pod := testPod("update-me", "app")

	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	// Verify original container
	rosName := sanitizeName(pod, "app")
	ct, err := rt.GetContainer(ctx, rosName)
	if err != nil {
		t.Fatalf("container not found: %v", err)
	}
	if ct.Image != "/dev/null" {
		t.Errorf("expected image /dev/null, got %s", ct.Image)
	}

	// Update with new image
	updatedPod := pod.DeepCopy()
	updatedPod.Annotations["vkube.io/file"] = "/tmp/new-image.tar"

	if err := p.UpdatePod(ctx, updatedPod); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}

	// New container should have updated image
	ct, err = rt.GetContainer(ctx, rosName)
	if err != nil {
		t.Fatalf("container not found after update: %v", err)
	}
	if ct.Image != "/tmp/new-image.tar" {
		t.Errorf("expected image /tmp/new-image.tar, got %s", ct.Image)
	}
	if !ct.IsRunning() {
		t.Errorf("expected running after update, got %s", ct.Status)
	}
}

func TestProviderGetPods(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()

	pod1 := testPod("app-one", "web")
	pod2 := testPod("app-two", "api")

	if err := p.CreatePod(ctx, pod1); err != nil {
		t.Fatalf("CreatePod 1: %v", err)
	}
	if err := p.CreatePod(ctx, pod2); err != nil {
		t.Fatalf("CreatePod 2: %v", err)
	}

	pods, err := p.GetPods(ctx)
	if err != nil {
		t.Fatalf("GetPods: %v", err)
	}
	if len(pods) != 2 {
		t.Errorf("expected 2 pods, got %d", len(pods))
	}

	names := map[string]bool{}
	for _, p := range pods {
		names[p.Name] = true
	}
	if !names["app-one"] || !names["app-two"] {
		t.Errorf("expected both pods, got %v", names)
	}
}

func TestProviderDeleteMissing(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()

	// Fabricate a pod that was never created in the runtime
	pod := testPod("ghost", "phantom")
	p.pods[podKey(pod)] = pod.DeepCopy()

	// Delete should succeed (log warnings but no error)
	if err := p.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod on missing containers should not error: %v", err)
	}

	if _, ok := p.pods[podKey(pod)]; ok {
		t.Error("pod still tracked after delete")
	}
}
