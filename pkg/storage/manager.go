package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/dockersave"
	"github.com/glennswest/mkube/pkg/routeros"
)

// Manager handles OCI image → tarball conversion, volume provisioning,
// tarball caching on RouterOS, and garbage collection of unused images
// and orphaned volumes.
type Manager struct {
	cfg         config.StorageConfig
	registryCfg config.RegistryConfig
	ros         *routeros.Client
	log         *zap.SugaredLogger

	mu                sync.Mutex
	images            map[string]*CachedImage // image ref -> cache entry
	volumes           map[string]*ProvisionedVolume
	registryTransport http.RoundTripper // TLS transport for local registry
}

// CachedImage tracks a cached OCI image tarball on the RouterOS filesystem.
type CachedImage struct {
	Ref         string // e.g. "docker.io/library/nginx:latest"
	TarballPath string // path on RouterOS, e.g. "/container-cache/nginx-latest.tar"
	Digest      string // registry manifest digest, e.g. "sha256:abc..."
	PulledAt    time.Time
	Size        int64
	InUse       int // reference count
}

// ProvisionedVolume tracks a volume created for a container.
type ProvisionedVolume struct {
	Name          string
	ContainerName string
	HostPath      string
	MountPath     string
	CreatedAt     time.Time
}

// HostVisiblePath translates a container-internal path to the path visible
// to RouterOS on the host. Paths under persistent mounts use the mount's
// host path; all other paths use the selfRootDir prefix.
func (m *Manager) HostVisiblePath(containerPath string) string {
	for _, mount := range m.cfg.PersistentMounts {
		if strings.HasPrefix(containerPath, mount.ContainerPath) {
			return strings.Replace(containerPath, mount.ContainerPath, mount.HostPath, 1)
		}
	}
	if m.cfg.SelfRootDir != "" {
		return m.cfg.SelfRootDir + "/" + strings.TrimPrefix(containerPath, "/")
	}
	return containerPath
}

// NewManager initializes the storage manager.
func NewManager(cfg config.StorageConfig, registryCfg config.RegistryConfig, ros *routeros.Client, log *zap.SugaredLogger) (*Manager, error) {
	m := &Manager{
		cfg:         cfg,
		registryCfg: registryCfg,
		ros:         ros,
		log:         log,
		images:      make(map[string]*CachedImage),
		volumes:     make(map[string]*ProvisionedVolume),
	}

	// Load registry CA cert for TLS verification if configured
	m.registryTransport = loadRegistryTransport(m.registryCfg.TLSCACertFile, "/etc/mkube/registry-ca.crt", log)

	return m, nil
}

// loadRegistryTransport returns an http.RoundTripper configured to trust
// the registry's CA certificate. Falls back to skip-verify if no CA cert
// is found (backward compatibility).
func loadRegistryTransport(caFile, defaultPath string, log *zap.SugaredLogger) http.RoundTripper {
	if caFile == "" {
		caFile = defaultPath
	}

	// Bounded transport settings prevent goroutine accumulation from
	// crane.Digest/Pull calls on every reconcile cycle. Each unbounded
	// Transport connection spawns 2 goroutines (readLoop + writeLoop)
	// that persist until idle timeout — without limits this causes OOM.
	bounds := func(t *http.Transport) *http.Transport {
		t.MaxIdleConns = 10
		t.MaxIdleConnsPerHost = 2
		t.MaxConnsPerHost = 4
		t.IdleConnTimeout = 30 * time.Second
		return t
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		if log != nil {
			log.Warnw("registry CA cert not found, using insecure TLS", "path", caFile)
		}
		return bounds(&http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		})
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		if log != nil {
			log.Warnw("failed to parse registry CA cert, using insecure TLS", "path", caFile)
		}
		return bounds(&http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		})
	}
	if log != nil {
		log.Infow("loaded registry CA cert", "path", caFile)
	}
	return bounds(&http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
	})
}

// EnsureImage makes sure the given OCI image reference is available as a
// tarball on the RouterOS filesystem. Always pulls fresh from registry.
// Within a single mkube session, the same image won't be pulled twice
// (session dedup). On restart, all images are re-pulled to avoid serving
// corrupted tarballs from disk cache.
func (m *Manager) EnsureImage(ctx context.Context, imageRef string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Normalize image ref to primary registry address before computing tarball name.
	imageRef = m.rewriteLocalhost(imageRef)

	tarballName := dockersave.SanitizeImageRef(imageRef) + ".tar"
	tarballPath := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)

	// Session dedup: if we already pulled this image in this mkube session,
	// verify the tarball still exists on disk and reuse it.
	if cached, ok := m.images[imageRef]; ok {
		if m.tarballExists(tarballPath) {
			cached.InUse++
			m.log.Debugw("image session hit", "ref", imageRef, "path", cached.TarballPath)
			return cached.TarballPath, nil
		}
		m.log.Warnw("tarball file missing, re-pulling", "ref", imageRef, "path", tarballPath)
	}

	// Always pull fresh from registry — no disk cache.
	m.log.Infow("pulling image", "ref", imageRef)

	// Remove any stale tarball before pulling fresh
	_ = os.Remove(tarballPath)
	_ = os.Remove(tarballPath + ".digest")

	if err := m.pullAndUpload(ctx, imageRef, tarballPath); err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Get the registry digest for freshness checking within this session
	digest, _ := m.getRegistryDigest(ctx, imageRef)

	hostPath := m.HostVisiblePath(tarballPath)

	m.images[imageRef] = &CachedImage{
		Ref:         imageRef,
		TarballPath: hostPath,
		Digest:      digest,
		PulledAt:    time.Now(),
		InUse:       1,
	}

	return hostPath, nil
}

// RefreshImage checks whether the registry has a newer version of the
// image than what was last pulled in this session. If the digest has
// changed (or no session record exists) it re-pulls the image, rewrites
// the tarball, and returns changed=true. On first call after restart,
// always re-pulls since no disk cache is used.
func (m *Manager) RefreshImage(ctx context.Context, imageRef string) (tarballPath string, changed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	imageRef = m.rewriteLocalhost(imageRef)

	currentDigest, err := m.getRegistryDigest(ctx, imageRef)
	if err != nil {
		return "", false, fmt.Errorf("getting digest for %s: %w", imageRef, err)
	}

	tarballName := dockersave.SanitizeImageRef(imageRef) + ".tar"
	localTarball := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)

	// Only compare against in-memory digest from this session — no disk cache.
	if cached, ok := m.images[imageRef]; ok && cached.Digest == currentDigest {
		hostPath := m.HostVisiblePath(localTarball)
		return hostPath, false, nil
	}

	// Digest changed or first check this session — re-pull.
	// On first check (no stored digest), always treat as changed since we
	// don't know if the running container matches the current registry image.
	storedDigest := ""
	if cached, ok := m.images[imageRef]; ok {
		storedDigest = cached.Digest
	}
	m.log.Infow("image refresh: pulling fresh",
		"ref", imageRef,
		"old_digest", truncDigest(storedDigest),
		"new_digest", truncDigest(currentDigest))
	_ = os.Remove(localTarball)
	_ = os.Remove(localTarball + ".digest")

	if err := m.pullAndUpload(ctx, imageRef, localTarball); err != nil {
		return "", false, fmt.Errorf("re-pulling image %s: %w", imageRef, err)
	}

	hostPath := m.HostVisiblePath(localTarball)

	wasChanged := storedDigest != "" && storedDigest != currentDigest
	m.images[imageRef] = &CachedImage{
		Ref:         imageRef,
		TarballPath: hostPath,
		Digest:      currentDigest,
		PulledAt:    time.Now(),
		InUse:       1,
	}

	return hostPath, wasChanged, nil
}

// RefreshImageWithHint is like RefreshImage but accepts a deployedDigest hint.
// On first check after restart (no session record), it compares the current
// registry digest against deployedDigest instead of returning changed=false.
// This solves the stale-binary-on-restart bug where the session has no memory
// of what digest the running container was created from.
func (m *Manager) RefreshImageWithHint(ctx context.Context, imageRef, deployedDigest string) (tarballPath string, changed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	imageRef = m.rewriteLocalhost(imageRef)

	currentDigest, err := m.getRegistryDigest(ctx, imageRef)
	if err != nil {
		return "", false, fmt.Errorf("getting digest for %s: %w", imageRef, err)
	}

	tarballName := dockersave.SanitizeImageRef(imageRef) + ".tar"
	localTarball := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)

	// Compare against in-memory digest from this session first.
	if cached, ok := m.images[imageRef]; ok && cached.Digest == currentDigest {
		hostPath := m.HostVisiblePath(localTarball)
		return hostPath, false, nil
	}

	// First check this session: use deployedDigest hint if available.
	compareDigest := ""
	if cached, ok := m.images[imageRef]; ok {
		compareDigest = cached.Digest
	} else if deployedDigest != "" {
		compareDigest = deployedDigest
	}

	m.log.Infow("image refresh: pulling fresh",
		"ref", imageRef,
		"deployed_digest", truncDigest(compareDigest),
		"registry_digest", truncDigest(currentDigest))
	_ = os.Remove(localTarball)
	_ = os.Remove(localTarball + ".digest")

	if err := m.pullAndUpload(ctx, imageRef, localTarball); err != nil {
		return "", false, fmt.Errorf("re-pulling image %s: %w", imageRef, err)
	}

	hostPath := m.HostVisiblePath(localTarball)

	wasChanged := compareDigest != "" && compareDigest != currentDigest
	m.images[imageRef] = &CachedImage{
		Ref:         imageRef,
		TarballPath: hostPath,
		Digest:      currentDigest,
		PulledAt:    time.Now(),
		InUse:       1,
	}

	return hostPath, wasChanged, nil
}

// GetCurrentDigest returns the current registry digest for an image.
// Used to stamp pods with the deployed digest for boot-time comparison.
func (m *Manager) GetCurrentDigest(ctx context.Context, imageRef string) (string, error) {
	imageRef = m.rewriteLocalhost(imageRef)
	return m.getRegistryDigest(ctx, imageRef)
}

// getRegistryDigest queries the registry for the current manifest digest.
func (m *Manager) getRegistryDigest(ctx context.Context, imageRef string) (string, error) {
	imageRef = m.rewriteLocalhost(imageRef)

	// Use a short timeout so a hung registry doesn't block the reconcile loop.
	digestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	opts := []crane.Option{crane.WithContext(digestCtx)}
	opts = append(opts, crane.WithPlatform(&v1.Platform{
		OS:           "linux",
		Architecture: runtime.GOARCH,
	}))

	if m.isLocalRegistry(imageRef) {
		opts = append(opts, crane.WithTransport(m.registryTransport))
	} else {
		opts = append(opts, crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
		))
	}

	return crane.Digest(imageRef, opts...)
}

// CheckImageAvailable verifies that an image is available in the registry
// by performing a HEAD request for the manifest digest. Returns the digest
// string and any error (e.g. 404 if the image is not present).
func (m *Manager) CheckImageAvailable(ctx context.Context, imageRef string) (string, error) {
	return m.getRegistryDigest(ctx, imageRef)
}

// ImageCacheEntry is an exported snapshot of a cached image for the API.
type ImageCacheEntry struct {
	Ref         string    `json:"ref"`
	TarballPath string    `json:"tarballPath"`
	Digest      string    `json:"digest"`
	PulledAt    time.Time `json:"pulledAt"`
	Size        int64     `json:"size"`
	InUse       int       `json:"inUse"`
}

// GetImageCache returns a snapshot of all cached images.
func (m *Manager) GetImageCache() []ImageCacheEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries := make([]ImageCacheEntry, 0, len(m.images))
	for _, img := range m.images {
		entries = append(entries, ImageCacheEntry{
			Ref:         img.Ref,
			TarballPath: img.TarballPath,
			Digest:      img.Digest,
			PulledAt:    img.PulledAt,
			Size:        img.Size,
			InUse:       img.InUse,
		})
	}
	return entries
}

// GetCachedDigest returns the cached registry digest for an image, or empty string.
func (m *Manager) GetCachedDigest(imageRef string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cached, ok := m.images[imageRef]; ok {
		return cached.Digest
	}
	return ""
}

func truncDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "..."
	}
	return d
}

// tarballExists checks whether the tarball file exists on the local filesystem.
// For RouterOS with SelfRootDir, tarballs are on persistent mounts (local disk).
// For Proxmox/StormBase, tarballs are also on local disk.
func (m *Manager) tarballExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pullAndUpload pulls an OCI image from a registry, converts it to a
// RouterOS-compatible flat rootfs tar, and uploads it.
func (m *Manager) pullAndUpload(ctx context.Context, imageRef, tarballPath string) error {
	// Rewrite bare localhost/ refs to the configured local registry address
	imageRef = m.rewriteLocalhost(imageRef)

	// Use a bounded timeout so a hung registry doesn't block the reconcile loop.
	pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Determine crane options — allow insecure for localhost registry
	opts := []crane.Option{crane.WithContext(pullCtx)}

	// Explicit platform: target is always Linux (RouterOS), arch matches build
	opts = append(opts, crane.WithPlatform(&v1.Platform{
		OS:           "linux",
		Architecture: runtime.GOARCH,
	}))

	if m.isLocalRegistry(imageRef) {
		opts = append(opts, crane.WithTransport(m.registryTransport))
	} else {
		// DefaultKeychain reads ~/.docker/config.json if present;
		// Anonymous fallback lets the transport handle OAuth2 bearer
		// token exchange for public registries (GHCR, Docker Hub, etc.)
		opts = append(opts, crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
		))
	}

	m.log.Infow("pulling OCI image", "ref", imageRef)
	img, err := crane.Pull(imageRef, opts...)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Log the pulled image's manifest digest for debugging stale-image issues
	if pulledDigest, err := img.Digest(); err == nil {
		m.log.Infow("pulled image manifest digest", "ref", imageRef, "digest", pulledDigest.String())
	}

	// Flatten OCI layers into a single uncompressed rootfs tarball,
	// then wrap it in docker-save format that RouterOS expects.
	m.log.Infow("flattening OCI layers to rootfs", "ref", imageRef)
	rootfsReader := mutate.Extract(img)
	defer rootfsReader.Close()

	var rootfsBuf bytes.Buffer
	if _, err := io.Copy(&rootfsBuf, rootfsReader); err != nil {
		return fmt.Errorf("extracting rootfs for %s: %w", imageRef, err)
	}

	// Extract image config for entrypoint/cmd/env
	imgCfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("reading image config for %s: %w", imageRef, err)
	}

	// Build docker-save format archive with uncompressed layer
	var dockerSave bytes.Buffer
	if err := dockersave.Write(&dockerSave, rootfsBuf.Bytes(), imageRef, imgCfg); err != nil {
		return fmt.Errorf("building docker-save for %s: %w", imageRef, err)
	}

	if m.cfg.SelfRootDir != "" {
		m.log.Infow("writing docker-save tarball to local disk", "ref", imageRef, "path", tarballPath, "size", dockerSave.Len())
		if err := os.MkdirAll(filepath.Dir(tarballPath), 0o755); err != nil {
			return fmt.Errorf("creating cache dir for %s: %w", tarballPath, err)
		}
		if err := os.WriteFile(tarballPath, dockerSave.Bytes(), 0o644); err != nil {
			return fmt.Errorf("writing tarball %s: %w", tarballPath, err)
		}
	} else if m.ros != nil {
		m.log.Infow("uploading tarball to RouterOS", "ref", imageRef, "path", tarballPath, "size", dockerSave.Len())
		if err := m.ros.UploadFile(ctx, tarballPath, bytes.NewReader(dockerSave.Bytes())); err != nil {
			return fmt.Errorf("uploading %s to RouterOS: %w", tarballPath, err)
		}
	} else {
		// No RouterOS client (stormbase) — write to local disk only
		m.log.Infow("writing tarball to local disk (no RouterOS)", "ref", imageRef, "path", tarballPath, "size", dockerSave.Len())
		if err := os.MkdirAll(filepath.Dir(tarballPath), 0o755); err != nil {
			return fmt.Errorf("creating cache dir for %s: %w", tarballPath, err)
		}
		if err := os.WriteFile(tarballPath, dockerSave.Bytes(), 0o644); err != nil {
			return fmt.Errorf("writing tarball %s: %w", tarballPath, err)
		}
	}

	return nil
}

// rewriteLocalhost rewrites image refs that point to the local registry
// using old or alternative addresses. Handles:
//   - "localhost/foo" → primary local address (go-containerregistry quirk)
//   - old registry addresses (e.g. 192.168.200.2:5000) → primary local address
func (m *Manager) rewriteLocalhost(imageRef string) string {
	if len(m.registryCfg.LocalAddresses) == 0 {
		return imageRef
	}
	primary := m.registryCfg.LocalAddresses[0]

	if strings.HasPrefix(imageRef, "localhost/") {
		rewritten := primary + "/" + strings.TrimPrefix(imageRef, "localhost/")
		m.log.Infow("rewrote bare localhost ref", "original", imageRef, "rewritten", rewritten)
		return rewritten
	}

	// Rewrite any non-primary local address alias to the primary address
	for _, addr := range m.registryCfg.LocalAddresses[1:] {
		if strings.HasPrefix(imageRef, addr+"/") {
			rewritten := primary + "/" + strings.TrimPrefix(imageRef, addr+"/")
			m.log.Debugw("rewrote old registry address", "original", imageRef, "rewritten", rewritten)
			return rewritten
		}
	}
	return imageRef
}

// isLocalRegistry returns true if the image ref points to the embedded registry
// (localhost or any configured local address).
func (m *Manager) isLocalRegistry(imageRef string) bool {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return strings.HasPrefix(imageRef, "localhost:") || strings.HasPrefix(imageRef, "localhost/")
	}
	registry := ref.Context().RegistryStr()
	if registry == "localhost" || registry == "localhost:5000" || strings.HasPrefix(registry, "localhost:") {
		return true
	}
	for _, addr := range m.registryCfg.LocalAddresses {
		if registry == addr {
			return true
		}
	}
	return false
}

// ClearImageDigest removes the session entry for an image so the next
// RefreshImage call will re-pull.
func (m *Manager) ClearImageDigest(imageRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.images, imageRef)

	// Remove stale tarball on disk.
	tarballName := dockersave.SanitizeImageRef(imageRef) + ".tar"
	tarballFile := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)
	_ = os.Remove(tarballFile + ".digest")
	_ = os.Remove(tarballFile)
}

// ClearImageDigestByRepo removes session entries for all images matching
// a given repository name (e.g. "microdns"). This is used when a push
// event is received from the registry to force re-pull on next reconcile.
func (m *Manager) ClearImageDigestByRepo(repo string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ref := range m.images {
		if strings.Contains(ref, "/"+repo+":") || strings.HasPrefix(ref, repo+":") {
			m.log.Infow("clearing session entry for repo push", "ref", ref, "repo", repo)
			delete(m.images, ref)
			tarballName := dockersave.SanitizeImageRef(ref) + ".tar"
			tarballFile := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)
			_ = os.Remove(tarballFile + ".digest")
			_ = os.Remove(tarballFile)
		}
	}
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
// container's volume mount. Volumes are stored under a separate path from
// the container root-dir so that data survives container recreation
// (tarball extraction overwrites root-dir contents).
func (m *Manager) ProvisionVolume(ctx context.Context, containerName, volumeName, mountPath string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Use a dedicated volumes directory parallel to the images directory.
	// e.g. /raid1/images → /raid1/volumes
	volumesBase := strings.TrimSuffix(m.cfg.BasePath, "/images") + "/volumes"
	hostPath := fmt.Sprintf("%s/%s/%s", volumesBase, containerName, volumeName)
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

			if m.ros != nil {
				if err := m.ros.RemoveFile(ctx, img.TarballPath); err != nil {
					m.log.Warnw("GC: failed to remove image", "ref", img.Ref, "error", err)
					continue
				}
			} else {
				_ = os.Remove(img.TarballPath)
			}

			delete(m.images, img.Ref)
			removed++
		}
	}

	// 2. Find orphaned volumes (volumes whose container no longer exists)
	if m.ros == nil {
		// StormBase: stormd manages volume cleanup internally
		if removed > 0 {
			m.log.Infow("GC completed (images only, no routeros)", "imagesRemoved", removed)
		}
		return
	}

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
