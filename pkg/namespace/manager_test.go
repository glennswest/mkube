package namespace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glenneth/microkube/pkg/config"
)

// mockZoneResolver implements ZoneResolver for tests.
type mockZoneResolver struct {
	zones map[string]struct {
		endpoint string
		zoneID   string
	}
}

func newMockZoneResolver() *mockZoneResolver {
	return &mockZoneResolver{
		zones: make(map[string]struct {
			endpoint string
			zoneID   string
		}),
	}
}

func (m *mockZoneResolver) GetZoneEndpoint(zoneName string) (endpoint, zoneID string, err error) {
	z, ok := m.zones[zoneName]
	if !ok {
		return "", "", fmt.Errorf("zone %q not found", zoneName)
	}
	return z.endpoint, z.zoneID, nil
}

func (m *mockZoneResolver) EnsureZone(ctx context.Context, zoneName, network string, dedicated bool) error {
	if _, ok := m.zones[zoneName]; !ok {
		m.zones[zoneName] = struct {
			endpoint string
			zoneID   string
		}{
			endpoint: "http://mock:8080",
			zoneID:   "zone-" + zoneName,
		}
	}
	return nil
}

func testManager(t *testing.T, statePath string) (*Manager, *mockZoneResolver) {
	t.Helper()

	logger, _ := zap.NewDevelopment()
	log := logger.Sugar()

	resolver := newMockZoneResolver()
	// Prepopulate zones for the default networks
	resolver.zones["gt.lo"] = struct {
		endpoint string
		zoneID   string
	}{"http://192.168.200.199:8080", "zone-gt-lo"}
	resolver.zones["g10.lo"] = struct {
		endpoint string
		zoneID   string
	}{"http://192.168.10.199:8080", "zone-g10-lo"}

	networks := []config.NetworkDef{
		{
			Name:    "gt",
			Bridge:  "bridge-gt",
			CIDR:    "192.168.200.0/24",
			Gateway: "192.168.200.1",
			DNS: config.DNSConfig{
				Endpoint: "http://192.168.200.199:8080",
				Zone:     "gt.lo",
				Server:   "192.168.200.199",
			},
		},
		{
			Name:    "g10",
			Bridge:  "bridge",
			CIDR:    "192.168.10.0/24",
			Gateway: "192.168.10.1",
			DNS: config.DNSConfig{
				Endpoint: "http://192.168.10.199:8080",
				Zone:     "g10.lo",
				Server:   "192.168.10.199",
			},
		},
	}

	nsCfg := config.NamespaceConfig{
		StatePath:   statePath,
		DefaultMode: "nested",
	}

	dzoCfg := config.DZOConfig{
		DefaultMode: "nested",
	}

	mgr := NewManager(nsCfg, dzoCfg, networks, resolver, log)
	return mgr, resolver
}

func TestBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}

	// Should have 2 default namespaces
	nss := mgr.ListNamespaces()
	if len(nss) != 2 {
		t.Fatalf("expected 2 namespaces, got %d", len(nss))
	}

	// Verify default namespace properties
	gt, err := mgr.GetNamespace("gt")
	if err != nil {
		t.Fatalf("getting gt namespace: %v", err)
	}
	if gt.Domain != "gt.lo" {
		t.Errorf("expected domain gt.lo, got %s", gt.Domain)
	}
	if gt.Mode != ModeOpen {
		t.Errorf("expected mode open, got %s", gt.Mode)
	}

	// State file should exist
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Error("state file was not created")
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	if len(mgr.ListNamespaces()) != 2 {
		t.Fatalf("expected 2 namespaces after double bootstrap, got %d", len(mgr.ListNamespaces()))
	}
}

func TestCreateNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	ns, err := mgr.CreateNamespace(ctx, "kube", "kube.gt.lo", "gt", ModeNested, false)
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	if ns.Name != "kube" {
		t.Errorf("expected name kube, got %s", ns.Name)
	}
	if ns.Domain != "kube.gt.lo" {
		t.Errorf("expected domain kube.gt.lo, got %s", ns.Domain)
	}
	if ns.Mode != ModeNested {
		t.Errorf("expected mode nested, got %s", ns.Mode)
	}
}

func TestCreateNamespaceDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Create with all defaults (empty domain, network, mode)
	ns, err := mgr.CreateNamespace(ctx, "infra", "", "", "", false)
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Should default to first network
	if ns.Network != "gt" {
		t.Errorf("expected default network gt, got %s", ns.Network)
	}
	// Domain should be auto-derived
	if ns.Domain != "infra.gt.lo" {
		t.Errorf("expected domain infra.gt.lo, got %s", ns.Domain)
	}
	// Mode should be default
	if ns.Mode != ModeNested {
		t.Errorf("expected default mode nested, got %s", ns.Mode)
	}
}

func TestCreateNamespaceDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// "gt" already exists from bootstrap
	_, err := mgr.CreateNamespace(ctx, "gt", "gt.lo", "gt", ModeOpen, false)
	if err == nil {
		t.Fatal("expected error creating duplicate namespace")
	}
}

func TestDeleteNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, err := mgr.CreateNamespace(ctx, "test", "test.gt.lo", "gt", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := mgr.DeleteNamespace(ctx, "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err = mgr.GetNamespace("test")
	if err == nil {
		t.Fatal("expected error getting deleted namespace")
	}
}

func TestDeleteNamespaceWithContainers(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, err := mgr.CreateNamespace(ctx, "test", "test.gt.lo", "gt", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mgr.AddContainerToNamespace("test", "my-container")

	err = mgr.DeleteNamespace(ctx, "test")
	if err == nil {
		t.Fatal("expected error deleting namespace with containers")
	}

	mgr.RemoveContainerFromNamespace("test", "my-container")
	if err := mgr.DeleteNamespace(ctx, "test"); err != nil {
		t.Fatalf("delete after removing containers: %v", err)
	}
}

func TestResolveNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	endpoint, zoneID, err := mgr.ResolveNamespace("gt")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if endpoint != "http://192.168.200.199:8080" {
		t.Errorf("expected endpoint http://192.168.200.199:8080, got %s", endpoint)
	}
	if zoneID != "zone-gt-lo" {
		t.Errorf("expected zone ID zone-gt-lo, got %s", zoneID)
	}

	_, _, err = mgr.ResolveNamespace("nonexistent")
	if err == nil {
		t.Fatal("expected error resolving non-existent namespace")
	}
}

func TestStatePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr1, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr1.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, err := mgr1.CreateNamespace(ctx, "persist", "persist.gt.lo", "gt", ModeNested, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Create new manager, load state
	mgr2, _ := testManager(t, statePath)
	if err := mgr2.Bootstrap(ctx); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	ns, err := mgr2.GetNamespace("persist")
	if err != nil {
		t.Fatalf("persisted namespace not found: %v", err)
	}
	if ns.Domain != "persist.gt.lo" {
		t.Errorf("expected domain persist.gt.lo, got %s", ns.Domain)
	}
}

// ─── K8s API Tests ──────────────────────────────────────────────────────────

func TestK8sAPIListNamespaces(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/namespaces", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var list corev1.NamespaceList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if list.Kind != "NamespaceList" {
		t.Errorf("expected kind NamespaceList, got %s", list.Kind)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list.Items))
	}
}

func TestK8sAPICreateNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	// Create with K8s Namespace format
	k8sNS := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube",
		},
	}
	body, _ := json.Marshal(k8sNS)

	req := httptest.NewRequest("POST", "/api/v1/namespaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created corev1.Namespace
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if created.Name != "kube" {
		t.Errorf("expected name kube, got %s", created.Name)
	}
	if created.Kind != "Namespace" {
		t.Errorf("expected kind Namespace, got %s", created.Kind)
	}

	// Should have auto-derived domain
	if domain := created.Annotations[annDomain]; domain != "kube.gt.lo" {
		t.Errorf("expected domain kube.gt.lo, got %s", domain)
	}
}

func TestK8sAPICreateNamespaceWithAnnotations(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	k8sNS := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "custom",
			Annotations: map[string]string{
				"vkube.io/domain":  "custom.g10.lo",
				"vkube.io/network": "g10",
				"vkube.io/mode":    "open",
			},
		},
	}
	body, _ := json.Marshal(k8sNS)

	req := httptest.NewRequest("POST", "/api/v1/namespaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created corev1.Namespace
	json.NewDecoder(w.Body).Decode(&created)

	if created.Annotations["vkube.io/domain"] != "custom.g10.lo" {
		t.Errorf("expected domain custom.g10.lo, got %s", created.Annotations["vkube.io/domain"])
	}
	if created.Annotations["vkube.io/network"] != "g10" {
		t.Errorf("expected network g10, got %s", created.Annotations["vkube.io/network"])
	}
	if created.Annotations["vkube.io/mode"] != "open" {
		t.Errorf("expected mode open, got %s", created.Annotations["vkube.io/mode"])
	}
}

func TestK8sAPIGetNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/namespaces/gt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var ns corev1.Namespace
	json.NewDecoder(w.Body).Decode(&ns)

	if ns.Name != "gt" {
		t.Errorf("expected name gt, got %s", ns.Name)
	}
	if ns.Kind != "Namespace" {
		t.Errorf("expected kind Namespace, got %s", ns.Kind)
	}
}

func TestK8sAPIGetNamespaceNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/namespaces/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestK8sAPIDeleteNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	mgr, _ := testManager(t, statePath)
	ctx := context.Background()

	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Create a namespace first
	mgr.CreateNamespace(ctx, "test", "test.gt.lo", "gt", "", false)

	mux := http.NewServeMux()
	mgr.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/api/v1/namespaces/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify deleted
	_, err := mgr.GetNamespace("test")
	if err == nil {
		t.Fatal("expected namespace to be deleted")
	}
}

func TestDZOStateMigration(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "ns-state.yaml")

	// Write a mock DZO state with namespaces
	dzoStatePath := filepath.Join(tmpDir, "dzo-state.yaml")
	dzoState := `zones:
  gt.lo:
    name: gt.lo
    endpoint: "http://192.168.200.199:8080"
    network: gt
instances: {}
namespaces:
  gt:
    name: gt
    domain: gt.lo
    zone: gt.lo
    network: gt
    mode: open
  kube:
    name: kube
    domain: kube.gt.lo
    zone: kube.gt.lo
    network: gt
    mode: nested
    containers:
      - my-app
`
	os.WriteFile(dzoStatePath, []byte(dzoState), 0644)

	logger, _ := zap.NewDevelopment()
	log := logger.Sugar()

	resolver := newMockZoneResolver()
	resolver.zones["gt.lo"] = struct {
		endpoint string
		zoneID   string
	}{"http://192.168.200.199:8080", "zone-gt-lo"}
	resolver.zones["kube.gt.lo"] = struct {
		endpoint string
		zoneID   string
	}{"http://192.168.200.199:8080", "zone-kube-gt-lo"}
	resolver.zones["g10.lo"] = struct {
		endpoint string
		zoneID   string
	}{"http://192.168.10.199:8080", "zone-g10-lo"}

	networks := []config.NetworkDef{
		{
			Name:    "gt",
			Bridge:  "bridge-gt",
			CIDR:    "192.168.200.0/24",
			Gateway: "192.168.200.1",
			DNS: config.DNSConfig{
				Endpoint: "http://192.168.200.199:8080",
				Zone:     "gt.lo",
				Server:   "192.168.200.199",
			},
		},
		{
			Name:    "g10",
			Bridge:  "bridge",
			CIDR:    "192.168.10.0/24",
			Gateway: "192.168.10.1",
			DNS: config.DNSConfig{
				Endpoint: "http://192.168.10.199:8080",
				Zone:     "g10.lo",
				Server:   "192.168.10.199",
			},
		},
	}

	nsCfg := config.NamespaceConfig{
		StatePath: statePath,
	}

	dzoCfg := config.DZOConfig{
		StatePath: dzoStatePath,
	}

	mgr := NewManager(nsCfg, dzoCfg, networks, resolver, log)

	ctx := context.Background()
	if err := mgr.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap with migration: %v", err)
	}

	// Should have migrated "kube" namespace
	kube, err := mgr.GetNamespace("kube")
	if err != nil {
		t.Fatalf("migrated kube namespace not found: %v", err)
	}
	if kube.Domain != "kube.gt.lo" {
		t.Errorf("expected domain kube.gt.lo, got %s", kube.Domain)
	}
	if kube.Mode != ModeNested {
		t.Errorf("expected mode nested, got %s", kube.Mode)
	}
	if len(kube.Containers) != 1 || kube.Containers[0] != "my-app" {
		t.Errorf("expected containers [my-app], got %v", kube.Containers)
	}

	// Should also have "gt" (from migration) and "g10" (default from bootstrap)
	if len(mgr.ListNamespaces()) != 3 {
		t.Fatalf("expected 3 namespaces (gt, kube migrated + g10 default), got %d", len(mgr.ListNamespaces()))
	}
}
