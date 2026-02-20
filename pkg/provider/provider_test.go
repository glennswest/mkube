package provider

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/glennswest/mkube/pkg/config"
)

func TestPodKey(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pod",
		},
	}
	if got := podKey(pod); got != "default/test-pod" {
		t.Errorf("expected 'default/test-pod', got %q", got)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		namespace     string
		podName       string
		containerName string
		want          string
	}{
		{"default", "myapp", "web", "default_myapp_web"},
		{"default", "MyApp", "Web", "default_myapp_web"},
		{"default", "my_app", "web_server", "default_my_app_web_server"},
		{"", "myapp", "web", "default_myapp_web"},
		{"gw", "dns", "microdns", "gw_dns_microdns"},
		{"g10", "dns", "microdns", "g10_dns_microdns"},
	}

	for _, tt := range tests {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: tt.namespace, Name: tt.podName}}
		got := sanitizeName(pod, tt.containerName)
		if got != tt.want {
			t.Errorf("sanitizeName(%q, %q, %q) = %q, want %q", tt.namespace, tt.podName, tt.containerName, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"", 5, ""},
		{"exactly5", 8, "exactly5"},
	}

	for _, tt := range tests {
		if got := truncate(tt.input, tt.max); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
	}
}

func TestBoolToConditionStatus(t *testing.T) {
	if boolToConditionStatus(true) != corev1.ConditionTrue {
		t.Error("expected ConditionTrue for true")
	}
	if boolToConditionStatus(false) != corev1.ConditionFalse {
		t.Error("expected ConditionFalse for false")
	}
}

func TestExtractPriority(t *testing.T) {
	// With annotation
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"vkube.io/boot-priority": "42",
			},
		},
	}
	if got := extractPriority(pod, 0); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}

	// Invalid annotation — falls back to index*10
	pod.Annotations["vkube.io/boot-priority"] = "invalid"
	if got := extractPriority(pod, 3); got != 30 {
		t.Errorf("expected 30 (fallback), got %d", got)
	}

	// No annotation
	pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
	if got := extractPriority(pod2, 2); got != 20 {
		t.Errorf("expected 20, got %d", got)
	}
}

func TestExtractDependencies(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"vkube.io/depends-on": "dns-pihole,vpn-wg",
			},
		},
	}

	deps := extractDependencies(pod)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0] != "dns-pihole" {
		t.Errorf("expected 'dns-pihole', got %q", deps[0])
	}
	if deps[1] != "vpn-wg" {
		t.Errorf("expected 'vpn-wg', got %q", deps[1])
	}

	// No annotation
	pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
	if deps := extractDependencies(pod2); deps != nil {
		t.Errorf("expected nil deps, got %v", deps)
	}
}

func TestExtractHealthCheck(t *testing.T) {
	// HTTP probe
	c := corev1.Container{
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(8080),
				},
			},
			PeriodSeconds: 30,
		},
	}
	hc := extractHealthCheck(c)
	if hc == nil {
		t.Fatal("expected health check")
	}
	if hc.Type != "http" {
		t.Errorf("expected type 'http', got %q", hc.Type)
	}
	if hc.Path != "/healthz" {
		t.Errorf("expected path '/healthz', got %q", hc.Path)
	}
	if hc.Port != 8080 {
		t.Errorf("expected port 8080, got %d", hc.Port)
	}

	// TCP probe
	c2 := corev1.Container{
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt(5432),
				},
			},
		},
	}
	hc2 := extractHealthCheck(c2)
	if hc2 == nil {
		t.Fatal("expected TCP health check")
	}
	if hc2.Type != "tcp" {
		t.Errorf("expected type 'tcp', got %q", hc2.Type)
	}

	// No probe
	c3 := corev1.Container{}
	if extractHealthCheck(c3) != nil {
		t.Error("expected nil for container without probe")
	}
}

func TestLoadPodManifests(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "pods.yaml")

	content := `apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: default
  annotations:
    vkube.io/boot-priority: "10"
spec:
  restartPolicy: Always
  containers:
    - name: nginx
      image: nginx:latest
---
apiVersion: v1
kind: Pod
metadata:
  name: another-pod
spec:
  containers:
    - name: redis
      image: redis:7
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	pods, _, err := loadManifests(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pods) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(pods))
	}

	if pods[0].Name != "test-pod" {
		t.Errorf("expected 'test-pod', got %q", pods[0].Name)
	}
	if pods[0].Namespace != "default" {
		t.Errorf("expected namespace 'default', got %q", pods[0].Namespace)
	}

	if pods[1].Name != "another-pod" {
		t.Errorf("expected 'another-pod', got %q", pods[1].Name)
	}
	if pods[1].Namespace != "default" {
		t.Errorf("expected default namespace for pod without one, got %q", pods[1].Namespace)
	}
}

func TestLoadPodManifestsNotFound(t *testing.T) {
	_, _, err := loadManifests("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestNewMicroKubeProvider(t *testing.T) {
	p, err := NewMicroKubeProvider(Deps{
		Config: &config.Config{NodeName: "test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.nodeName != "test" {
		t.Errorf("expected nodeName 'test', got %q", p.nodeName)
	}
	if p.pods == nil {
		t.Error("pods map not initialized")
	}
}
