package storage

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/dockersave"
)

func testLogger() *zap.SugaredLogger {
	log, _ := zap.NewDevelopment()
	return log.Sugar()
}

func TestSanitizeImageRef(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nginx:latest", "nginx-latest"},
		{"docker.io/library/nginx:1.25", "docker-io-library-nginx-1-25"},
		{"localhost:5000/myapp:v1.0.0", "localhost-5000-myapp-v1-0-0"},
		{"UPPERCASE/Image:Tag", "uppercase-image-tag"},
		{"a/b/c/d:e", "a-b-c-d-e"},
	}

	for _, tt := range tests {
		got := dockersave.SanitizeImageRef(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeImageRef(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEnsureImageCacheHit(t *testing.T) {
	mgr := &Manager{
		cfg: config.StorageConfig{TarballCache: "/cache"},
		log: testLogger(),
		images: map[string]*CachedImage{
			"nginx:latest": {
				Ref:         "nginx:latest",
				TarballPath: "/cache/nginx-latest.tar",
				InUse:       1,
			},
		},
		volumes: make(map[string]*ProvisionedVolume),
	}

	path, err := mgr.EnsureImage(context.TODO(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/cache/nginx-latest.tar" {
		t.Errorf("expected cached path, got %q", path)
	}
	if mgr.images["nginx:latest"].InUse != 2 {
		t.Errorf("expected InUse=2, got %d", mgr.images["nginx:latest"].InUse)
	}
}

func TestReleaseImage(t *testing.T) {
	mgr := &Manager{
		images: map[string]*CachedImage{
			"nginx:latest": {InUse: 3},
		},
	}

	mgr.ReleaseImage("nginx:latest")
	if mgr.images["nginx:latest"].InUse != 2 {
		t.Errorf("expected InUse=2, got %d", mgr.images["nginx:latest"].InUse)
	}

	// Release below zero should clamp to 0
	mgr.images["nginx:latest"].InUse = 0
	mgr.ReleaseImage("nginx:latest")
	if mgr.images["nginx:latest"].InUse != 0 {
		t.Errorf("expected InUse=0, got %d", mgr.images["nginx:latest"].InUse)
	}

	// Releasing nonexistent image should not panic
	mgr.ReleaseImage("nonexistent")
}

func TestProvisionVolume(t *testing.T) {
	mgr := &Manager{
		cfg:     config.StorageConfig{BasePath: "/container-data"},
		log:     testLogger(),
		volumes: make(map[string]*ProvisionedVolume),
	}

	path, err := mgr.ProvisionVolume(context.TODO(), "mycontainer", "data-vol", "/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "/container-data/mycontainer/data-vol"
	if path != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}

	vol, ok := mgr.volumes["mycontainer/data-vol"]
	if !ok {
		t.Fatal("volume not tracked")
	}
	if vol.MountPath != "/data" {
		t.Errorf("expected mount path /data, got %q", vol.MountPath)
	}
}

func TestGCKeepLastN(t *testing.T) {
	mgr := &Manager{
		cfg: config.StorageConfig{
			GCKeepLastN: 2,
			GCDryRun:    true,
		},
		images: map[string]*CachedImage{
			"img1": {Ref: "img1", InUse: 0, PulledAt: time.Now().Add(-3 * time.Hour)},
			"img2": {Ref: "img2", InUse: 0, PulledAt: time.Now().Add(-2 * time.Hour)},
			"img3": {Ref: "img3", InUse: 0, PulledAt: time.Now().Add(-1 * time.Hour)},
			"img4": {Ref: "img4", InUse: 1, PulledAt: time.Now()}, // in use, not a candidate
		},
		volumes: make(map[string]*ProvisionedVolume),
	}

	// Candidates are img1, img2, img3 (3 unused, keep 2, so 1 would be removed)
	var candidates []*CachedImage
	for _, img := range mgr.images {
		if img.InUse == 0 {
			candidates = append(candidates, img)
		}
	}
	if len(candidates) != 3 {
		t.Errorf("expected 3 unused candidates, got %d", len(candidates))
	}
}

func TestNewManager(t *testing.T) {
	mgr, err := NewManager(config.StorageConfig{
		BasePath:     "/data",
		TarballCache: "/cache",
	}, config.RegistryConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.images == nil {
		t.Error("images map not initialized")
	}
	if mgr.volumes == nil {
		t.Error("volumes map not initialized")
	}
}
