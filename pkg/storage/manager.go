package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/routeros"
)

// Manager handles OCI image → tarball conversion, volume provisioning,
// tarball caching on RouterOS, and garbage collection of unused images
// and orphaned volumes.
type Manager struct {
	cfg            config.StorageConfig
	registryCfg    config.RegistryConfig
	ros            *routeros.Client
	log            *zap.SugaredLogger

	mu       sync.Mutex
	images   map[string]*CachedImage   // image ref -> cache entry
	volumes  map[string]*ProvisionedVolume
}

// CachedImage tracks a cached OCI image tarball on the RouterOS filesystem.
type CachedImage struct {
	Ref         string    // e.g. "docker.io/library/nginx:latest"
	TarballPath string    // path on RouterOS, e.g. "/container-cache/nginx-latest.tar"
	PulledAt    time.Time
	Size        int64
	InUse       int       // reference count
}

// ProvisionedVolume tracks a volume created for a container.
type ProvisionedVolume struct {
	Name          string
	ContainerName string
	HostPath      string
	MountPath     string
	CreatedAt     time.Time
}

// NewManager initializes the storage manager.
func NewManager(cfg config.StorageConfig, registryCfg config.RegistryConfig, ros *routeros.Client, log *zap.SugaredLogger) (*Manager, error) {
	return &Manager{
		cfg:         cfg,
		registryCfg: registryCfg,
		ros:         ros,
		log:         log,
		images:      make(map[string]*CachedImage),
		volumes:     make(map[string]*ProvisionedVolume),
	}, nil
}

// EnsureImage makes sure the given OCI image reference is available as a
// tarball on the RouterOS filesystem. Steps:
//  1. Check local cache
//  2. If not cached: pull from registry, convert to tarball, upload to RouterOS
//  3. Return the tarball path on RouterOS
func (m *Manager) EnsureImage(ctx context.Context, imageRef string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check cache
	if cached, ok := m.images[imageRef]; ok {
		cached.InUse++
		m.log.Debugw("image cache hit", "ref", imageRef, "path", cached.TarballPath)
		return cached.TarballPath, nil
	}

	m.log.Infow("pulling image", "ref", imageRef)

	// Determine tarball filename from image ref
	tarballName := sanitizeImageRef(imageRef) + ".tar"
	tarballPath := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)

	// Pull the image and convert to tarball.
	// In practice this would use:
	//   - crane/go-containerregistry to pull OCI images
	//   - Convert to a flat tarball that RouterOS expects
	//   - Upload via RouterOS file API or SFTP
	//
	// For images from the embedded Zot registry (localhost:5000),
	// this is a local operation.
	if err := m.pullAndUpload(ctx, imageRef, tarballPath); err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	m.images[imageRef] = &CachedImage{
		Ref:         imageRef,
		TarballPath: tarballPath,
		PulledAt:    time.Now(),
		InUse:       1,
	}

	return tarballPath, nil
}

// pullAndUpload pulls an OCI image from a registry, converts it to a
// RouterOS-compatible flat rootfs tar, and uploads it.
func (m *Manager) pullAndUpload(ctx context.Context, imageRef, tarballPath string) error {
	// Determine crane options — allow insecure for localhost registry
	opts := []crane.Option{crane.WithContext(ctx)}
	if m.isLocalRegistry(imageRef) {
		opts = append(opts, crane.Insecure)
	} else {
		opts = append(opts, crane.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	m.log.Infow("pulling OCI image", "ref", imageRef)
	img, err := crane.Pull(imageRef, opts...)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Flatten OCI layers into a single rootfs tarball.
	// mutate.Extract merges all layers (applying whiteouts) into one reader.
	m.log.Infow("flattening OCI layers to rootfs", "ref", imageRef)
	rootfsReader := mutate.Extract(img)
	defer rootfsReader.Close()

	// Read the flattened rootfs into a buffer for upload
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rootfsReader); err != nil {
		return fmt.Errorf("extracting rootfs for %s: %w", imageRef, err)
	}

	m.log.Infow("uploading tarball to RouterOS", "ref", imageRef, "path", tarballPath, "size", buf.Len())
	if err := m.ros.UploadFile(ctx, tarballPath, bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("uploading %s to RouterOS: %w", tarballPath, err)
	}

	return nil
}

// imageSize returns the total size of an OCI image across all layers.
func imageSize(img v1.Image) int64 {
	layers, err := img.Layers()
	if err != nil {
		return 0
	}
	var total int64
	for _, l := range layers {
		sz, _ := l.Size()
		total += sz
	}
	return total
}

// isLocalRegistry returns true if the image ref points to the embedded registry
// (localhost or any configured local address).
func (m *Manager) isLocalRegistry(imageRef string) bool {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return strings.HasPrefix(imageRef, "localhost:")
	}
	registry := ref.Context().RegistryStr()
	if registry == "localhost:5000" || strings.HasPrefix(registry, "localhost:") {
		return true
	}
	for _, addr := range m.registryCfg.LocalAddresses {
		if registry == addr {
			return true
		}
	}
	return false
}

// tarballSize reads through a tar archive and sums the file sizes.
func tarballSize(data []byte) int64 {
	tr := tar.NewReader(bytes.NewReader(data))
	var total int64
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		total += hdr.Size
	}
	return total
}

// ReleaseImage decrements the use count of an image.
func (m *Manager) ReleaseImage(imageRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cached, ok := m.images[imageRef]; ok {
		cached.InUse--
		if cached.InUse < 0 {
			cached.InUse = 0
		}
	}
}

// ProvisionVolume creates a directory on the RouterOS filesystem for a
// container's volume mount.
func (m *Manager) ProvisionVolume(ctx context.Context, containerName, volumeName, mountPath string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hostPath := fmt.Sprintf("%s/%s/%s", m.cfg.BasePath, containerName, volumeName)
	key := fmt.Sprintf("%s/%s", containerName, volumeName)

	// Create the directory on RouterOS
	// RouterOS filesystem is accessible via /file/
	// The directory will be created implicitly when the container starts
	// if root-dir is set, but we track it for GC purposes.

	m.volumes[key] = &ProvisionedVolume{
		Name:          volumeName,
		ContainerName: containerName,
		HostPath:      hostPath,
		MountPath:     mountPath,
		CreatedAt:     time.Now(),
	}

	m.log.Infow("volume provisioned", "container", containerName, "volume", volumeName, "path", hostPath)
	return hostPath, nil
}

// ─── Garbage Collection ─────────────────────────────────────────────────────

// RunGarbageCollector periodically cleans up unused images and orphaned volumes.
func (m *Manager) RunGarbageCollector(ctx context.Context) {
	interval := time.Duration(m.cfg.GCIntervalMinutes) * time.Minute
	if interval == 0 {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.log.Infow("storage GC started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			m.log.Info("storage GC shutting down")
			return
		case <-ticker.C:
			m.runGC(ctx)
		}
	}
}

func (m *Manager) runGC(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Debug("running storage garbage collection")

	// 1. Clean up unused images (InUse == 0)
	var candidates []*CachedImage
	for _, img := range m.images {
		if img.InUse == 0 {
			candidates = append(candidates, img)
		}
	}

	// Sort by PulledAt (oldest first), keep last N
	// (simplified: just count and remove excess)
	removed := 0
	keepN := m.cfg.GCKeepLastN
	if keepN == 0 {
		keepN = 5
	}

	if len(candidates) > keepN {
		for _, img := range candidates[:len(candidates)-keepN] {
			if m.cfg.GCDryRun {
				m.log.Infow("GC dry-run: would remove image", "ref", img.Ref, "path", img.TarballPath)
				continue
			}

			if err := m.ros.RemoveFile(ctx, img.TarballPath); err != nil {
				m.log.Warnw("GC: failed to remove image", "ref", img.Ref, "error", err)
				continue
			}

			delete(m.images, img.Ref)
			removed++
		}
	}

	// 2. Find orphaned volumes (volumes whose container no longer exists)
	containers, err := m.ros.ListContainers(ctx)
	if err != nil {
		m.log.Warnw("GC: failed to list containers", "error", err)
		return
	}

	activeContainers := make(map[string]bool)
	for _, c := range containers {
		activeContainers[c.Name] = true
	}

	orphanedVolumes := 0
	for key, vol := range m.volumes {
		if !activeContainers[vol.ContainerName] {
			if m.cfg.GCDryRun {
				m.log.Infow("GC dry-run: would remove volume", "path", vol.HostPath)
				continue
			}

			if err := m.ros.RemoveFile(ctx, vol.HostPath); err != nil {
				m.log.Warnw("GC: failed to remove volume", "path", vol.HostPath, "error", err)
				continue
			}

			delete(m.volumes, key)
			orphanedVolumes++
		}
	}

	if removed > 0 || orphanedVolumes > 0 {
		m.log.Infow("GC completed", "imagesRemoved", removed, "volumesRemoved", orphanedVolumes)
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func sanitizeImageRef(ref string) string {
	result := make([]byte, 0, len(ref))
	for _, c := range ref {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, byte(c))
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, byte(c+32))
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}
