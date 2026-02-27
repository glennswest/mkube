// mkube-update: Watches a local OCI registry for image digest changes and
// replaces RouterOS containers when new images are available.
//
// Uses tarball-based updates: pre-pulls images from registry while old
// container is still running, then swaps using local file. This eliminates
// the chicken-and-egg problem where registry can't pull its own update.
//
// For self-updates, it calls the mkube update API which performs the swap.

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var version = "dev"

// Config is the top-level configuration loaded from YAML.
type Config struct {
	RegistryURL      string          `yaml:"registryURL"`
	RouterOSURL      string          `yaml:"routerosURL"`
	RouterOSUser     string          `yaml:"routerosUser"`
	RouterOSPassword string          `yaml:"routerosPassword"`
	MkubeAPI         string          `yaml:"mkubeAPI"`
	PollSeconds      int             `yaml:"pollSeconds"`
	TarballDir       string          `yaml:"tarballDir"`       // container path for staging tarballs (default: /data/staging)
	TarballROSPath   string          `yaml:"tarballROSPath"`   // RouterOS-relative path prefix (default: raid1/volumes/mkube-update-updater/data/staging)
	Watches          []WatchEntry    `yaml:"watches"`
	Bootstrap        BootstrapConfig `yaml:"bootstrap"`
}

// BootstrapConfig defines how mkube-update bootstraps the mkube container.
type BootstrapConfig struct {
	Enabled     bool               `yaml:"enabled"`
	Image       string             `yaml:"image"`
	SelfRootDir string             `yaml:"selfRootDir"`
	TarballDir  string             `yaml:"tarballDir"`
	Container   BootstrapContainer `yaml:"container"`
}

// BootstrapContainer defines the RouterOS container spec for the bootstrapped mkube.
type BootstrapContainer struct {
	Name        string `yaml:"name"`
	Interface   string `yaml:"interface"`
	RootDir     string `yaml:"rootDir"`
	Hostname    string `yaml:"hostname"`
	DNS         string `yaml:"dns"`
	Logging     string `yaml:"logging"`
	StartOnBoot string `yaml:"startOnBoot"`
	MountLists  string `yaml:"mountLists"`
}

// WatchEntry defines a single image to watch in the local registry.
type WatchEntry struct {
	Repo         string        `yaml:"repo"`
	Tag          string        `yaml:"tag"`                   // single tag (backward compat)
	Tags         []string      `yaml:"tags,omitempty"`        // ordered preference list; first found wins
	Container    string        `yaml:"container,omitempty"`   // single container
	Containers   []string      `yaml:"containers,omitempty"` // multiple containers
	SelfUpdate   bool          `yaml:"selfUpdate,omitempty"`
	Rolling      bool          `yaml:"rolling,omitempty"`
	RollingDelay time.Duration `yaml:"rollingDelay,omitempty"`
}

// ResolvedTags returns the tag preference list. If Tags is set, it takes
// priority. Otherwise falls back to the single Tag field.
func (w WatchEntry) ResolvedTags() []string {
	if len(w.Tags) > 0 {
		return w.Tags
	}
	if w.Tag != "" {
		return []string{w.Tag}
	}
	return []string{"latest"}
}

// Targets returns the list of container names for this watch entry.
func (w WatchEntry) Targets() []string {
	if len(w.Containers) > 0 {
		return w.Containers
	}
	if w.Container != "" {
		return []string{w.Container}
	}
	return nil
}

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("starting mkube-update", "version", version)

	configPath := "/etc/mkube-update/config.yaml"
	if v := os.Getenv("MKUBE_UPDATE_CONFIG"); v != "" {
		configPath = v
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalw("reading config", "path", configPath, "error", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalw("parsing config", "error", err)
	}

	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = 60
	}
	if cfg.TarballDir == "" {
		cfg.TarballDir = "/data/staging"
	}
	if cfg.TarballROSPath == "" {
		cfg.TarballROSPath = "raid1/volumes/mkube-update-updater/data/staging"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	updater := &Updater{
		cfg:     cfg,
		log:     log,
		digests: make(map[string]string),
		http: &http.Client{
			Transport: loadRegistryTransport(log),
			Timeout:   30 * time.Second,
		},
	}

	// Ensure tarball staging directory exists
	if err := os.MkdirAll(cfg.TarballDir, 0755); err != nil {
		log.Fatalw("creating tarball staging dir", "path", cfg.TarballDir, "error", err)
	}

	// Bootstrap mkube if configured
	if cfg.Bootstrap.Enabled {
		if err := updater.bootstrap(ctx); err != nil {
			log.Fatalw("bootstrap failed", "error", err)
		}
	}

	// Health endpoint
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "ok")
		})
		srv := &http.Server{Addr: ":8081", Handler: mux}
		go func() {
			<-ctx.Done()
			_ = srv.Shutdown(context.Background())
		}()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorw("healthz server error", "error", err)
		}
	}()

	// Poll loop
	updater.run(ctx)
}

// Updater polls the local registry for digest changes and replaces containers.
type Updater struct {
	cfg     Config
	log     *zap.SugaredLogger
	digests map[string]string // "repo:tag" → last seen digest
	http    *http.Client
}

func (u *Updater) run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(u.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Initial poll immediately
	u.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			u.log.Info("shutting down")
			return
		case <-ticker.C:
			u.poll(ctx)
		}
	}
}

func (u *Updater) poll(ctx context.Context) {
	for _, w := range u.cfg.Watches {
		// Search tags in preference order — first found wins.
		tags := w.ResolvedTags()
		var resolvedTag, digest string
		var err error
		for _, t := range tags {
			digest, err = u.getDigest(ctx, w.Repo, t)
			if err == nil {
				resolvedTag = t
				break
			}
		}
		if resolvedTag == "" {
			u.log.Warnw("no tag found in registry", "repo", w.Repo, "tried", tags, "last_error", err)
			continue
		}

		key := w.Repo // keyed by repo, not repo:tag — tag may change across polls
		prev, seen := u.digests[key]
		u.digests[key] = digest

		if !seen {
			u.log.Infow("initial digest recorded", "repo", w.Repo, "tag", resolvedTag, "digest", digest)
			continue
		}

		if digest == prev {
			continue
		}

		u.log.Infow("digest changed", "repo", w.Repo, "tag", resolvedTag,
			"old", prev, "new", digest)

		targets := w.Targets()
		if len(targets) == 0 {
			u.log.Warnw("no target containers for watch", "repo", w.Repo)
			continue
		}

		imageRef := fmt.Sprintf("%s/%s:%s",
			trimScheme(u.cfg.RegistryURL), w.Repo, resolvedTag)

		if w.SelfUpdate {
			// Self-update: ask mkube to replace us
			for _, tgt := range targets {
				if err := u.requestSelfUpdate(ctx, tgt, imageRef); err != nil {
					u.log.Errorw("self-update request failed", "name", tgt, "error", err)
					// Revert digest so we retry next poll
					u.digests[key] = prev
				}
			}
		} else if w.Rolling && len(targets) > 1 {
			delay := w.RollingDelay
			if delay == 0 {
				delay = 5 * time.Second
			}
			anyFailed := false
			for i, tgt := range targets {
				if err := u.replaceContainer(ctx, tgt, imageRef); err != nil {
					// Skip containers that don't exist (e.g. external DNS)
					if strings.Contains(err.Error(), "not found") {
						u.log.Warnw("container not found, skipping", "name", tgt)
						continue
					}
					u.log.Errorw("rolling update failed", "name", tgt, "error", err)
					anyFailed = true
					break
				}
				if i < len(targets)-1 {
					time.Sleep(delay)
				}
			}
			if anyFailed {
				u.digests[key] = prev
			}
		} else {
			for _, tgt := range targets {
				if err := u.replaceContainer(ctx, tgt, imageRef); err != nil {
					u.log.Errorw("container replacement failed", "name", tgt, "error", err)
					u.digests[key] = prev
				}
			}
		}
	}
}

// getDigest queries the local registry for the current digest of repo:tag.
func (u *Updater) getDigest(ctx context.Context, repo, tag string) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", u.cfg.RegistryURL, repo, tag)

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")

	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("manifest not found: %s:%s", repo, tag)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("registry returned %d for %s:%s", resp.StatusCode, repo, tag)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no Docker-Content-Digest header for %s:%s", repo, tag)
	}
	return digest, nil
}

// prePullTarball pulls an image from the registry and saves it as a
// RouterOS-compatible docker-save v1 tarball. RouterOS requires a very specific
// format: manifest.json, repositories, {hash}.json config, and {hash}/layer.tar
// (uncompressed) with VERSION and json metadata files.
//
// Tries local registry first, falls back to GHCR if local pull fails.
func (u *Updater) prePullTarball(ctx context.Context, imageRef string) (containerPath, rosPath string, err error) {
	log := u.log.With("image", imageRef)
	log.Info("pre-pulling image as tarball")

	// Parse the image reference
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", "", fmt.Errorf("parsing image ref %q: %w", imageRef, err)
	}

	// Try local registry first
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithTransport(u.http.Transport))
	if err != nil {
		log.Warnw("local registry pull failed, trying GHCR fallback", "error", err)

		// Extract repo:tag from the ref and build GHCR ref
		ghcrRef := u.ghcrFallbackRef(imageRef)
		ghcrParsed, ghcrErr := name.ParseReference(ghcrRef)
		if ghcrErr != nil {
			return "", "", fmt.Errorf("local pull failed: %w; parsing GHCR ref %q: %w", err, ghcrRef, ghcrErr)
		}

		// GHCR uses default transport (system CAs if available)
		img, ghcrErr = remote.Image(ghcrParsed, remote.WithContext(ctx))
		if ghcrErr != nil {
			return "", "", fmt.Errorf("local pull failed: %w; GHCR pull failed: %w", err, ghcrErr)
		}

		log.Infow("pulled from GHCR fallback", "ghcrRef", ghcrRef)
	}

	// Sanitize filename: "192.168.200.3:5000/mkube:edge" → "mkube-edge.tar"
	parts := strings.Split(imageRef, "/")
	repoTag := parts[len(parts)-1] // "mkube:edge"
	safeName := strings.ReplaceAll(repoTag, ":", "-") + ".tar"

	containerPath = filepath.Join(u.cfg.TarballDir, safeName)
	rosPath = u.cfg.TarballROSPath + "/" + safeName

	log.Infow("saving RouterOS-compatible tarball", "path", containerPath, "rosPath", rosPath)
	if err := saveRouterOSTarball(img, repoTag, containerPath); err != nil {
		os.Remove(containerPath)
		return "", "", fmt.Errorf("saving tarball: %w", err)
	}

	info, err := os.Stat(containerPath)
	if err != nil {
		return "", "", fmt.Errorf("verifying tarball: %w", err)
	}

	log.Infow("tarball staged", "size", info.Size(), "rosPath", rosPath)
	return containerPath, rosPath, nil
}

// ghcrFallbackRef converts a local registry ref to a GHCR ref.
// "192.168.200.3:5000/mkube:edge" → "ghcr.io/glennswest/mkube:edge"
func (u *Updater) ghcrFallbackRef(imageRef string) string {
	parts := strings.Split(imageRef, "/")
	repoTag := parts[len(parts)-1]
	return "ghcr.io/glennswest/" + repoTag
}

// saveRouterOSTarball creates a docker-save v1 format tarball that RouterOS can
// extract. This matches the exact format produced by hack/make-tarball.sh:
//
//	manifest.json
//	repositories
//	{configHash}.json          — image config
//	{layerHash}/layer.tar      — uncompressed layer
//	{layerHash}/VERSION        — "1.0"
//	{layerHash}/json           — legacy layer metadata
func saveRouterOSTarball(img v1.Image, repoTag, outputPath string) error {
	// Get raw config
	configBytes, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("getting config: %w", err)
	}
	configHash := sha256sum(configBytes)

	// Get layers
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("getting layers: %w", err)
	}

	// Process layers — write uncompressed to temp files, collect diffIDs
	type layerInfo struct {
		diffID  string // hex hash (no "sha256:" prefix)
		tmpFile string
		size    int64
	}

	var layerInfos []layerInfo
	var tmpFiles []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
	}()

	for i, layer := range layers {
		diffID, err := layer.DiffID()
		if err != nil {
			return fmt.Errorf("getting diffID for layer %d: %w", i, err)
		}

		rc, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("getting uncompressed layer %d: %w", i, err)
		}

		tmpFile, err := os.CreateTemp("", "layer-*.tar")
		if err != nil {
			rc.Close()
			return fmt.Errorf("creating temp file for layer %d: %w", i, err)
		}
		tmpFiles = append(tmpFiles, tmpFile.Name())

		n, err := io.Copy(tmpFile, rc)
		rc.Close()
		tmpFile.Close()
		if err != nil {
			return fmt.Errorf("writing layer %d: %w", i, err)
		}

		layerInfos = append(layerInfos, layerInfo{
			diffID:  diffID.Hex,
			tmpFile: tmpFile.Name(),
			size:    n,
		})
	}

	// Build the docker-save tarball
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// 1. Config: {configHash}.json
	if err := tarAddBytes(tw, configHash+".json", configBytes); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	// 2. Each layer: {diffID}/layer.tar, {diffID}/VERSION, {diffID}/json
	for i, li := range layerInfos {
		if err := tarAddFile(tw, li.diffID+"/layer.tar", li.tmpFile, li.size); err != nil {
			return fmt.Errorf("writing layer %d tar: %w", i, err)
		}
		if err := tarAddBytes(tw, li.diffID+"/VERSION", []byte("1.0")); err != nil {
			return fmt.Errorf("writing layer %d VERSION: %w", i, err)
		}
		parentID := ""
		if i > 0 {
			parentID = layerInfos[i-1].diffID
		}
		layerJSON := buildLayerJSON(li.diffID, parentID, i == len(layerInfos)-1, configBytes)
		if err := tarAddBytes(tw, li.diffID+"/json", layerJSON); err != nil {
			return fmt.Errorf("writing layer %d json: %w", i, err)
		}
	}

	// 3. manifest.json
	layerPaths := make([]string, len(layerInfos))
	for i, li := range layerInfos {
		layerPaths[i] = li.diffID + "/layer.tar"
	}
	manifest := []map[string]interface{}{
		{
			"Config":   configHash + ".json",
			"RepoTags": []string{repoTag},
			"Layers":   layerPaths,
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	if err := tarAddBytes(tw, "manifest.json", manifestBytes); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// 4. repositories
	repoParts := strings.SplitN(repoTag, ":", 2)
	repoName := repoParts[0]
	tagName := "latest"
	if len(repoParts) == 2 {
		tagName = repoParts[1]
	}
	topLayerID := layerInfos[len(layerInfos)-1].diffID
	repos := map[string]map[string]string{
		repoName: {tagName: topLayerID},
	}
	reposBytes, _ := json.Marshal(repos)
	if err := tarAddBytes(tw, "repositories", reposBytes); err != nil {
		return fmt.Errorf("writing repositories: %w", err)
	}

	return nil
}

// buildLayerJSON creates the legacy docker layer metadata JSON.
func buildLayerJSON(id, parentID string, isLast bool, configBytes []byte) []byte {
	m := map[string]interface{}{
		"id":      id,
		"created": "1970-01-01T00:00:00Z",
	}
	if parentID != "" {
		m["parent"] = parentID
	}
	if isLast {
		var imgConfig map[string]interface{}
		if json.Unmarshal(configBytes, &imgConfig) == nil {
			if cfg, ok := imgConfig["config"]; ok {
				m["config"] = cfg
			}
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// sha256sum returns the hex-encoded SHA-256 hash of data.
func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// tarAddBytes writes a file entry to a tar writer from a byte slice.
func tarAddBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(data)),
		Mode: 0644,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// tarAddFile writes a file entry to a tar writer by streaming from a file on disk.
func tarAddFile(tw *tar.Writer, name, srcPath string, size int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: size,
		Mode: 0644,
	}); err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

// replaceContainer pre-pulls the new image as a tarball, then stops, removes,
// and recreates the container using the local tarball file.
func (u *Updater) replaceContainer(ctx context.Context, name, imageRef string) error {
	log := u.log.With("container", name, "image", imageRef)

	// Step 1: Pre-pull tarball WHILE old container is still running.
	// This is the key improvement — the registry is still up during this step.
	containerPath, rosPath, err := u.prePullTarball(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("pre-pulling tarball: %w", err)
	}
	defer os.Remove(containerPath) // clean up after use

	// Step 2: Get current container config
	ct, err := u.rosGetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting container: %w", err)
	}

	// Step 3: Stop if running
	if ct.isRunning() {
		log.Info("stopping container")
		if err := u.rosPost(ctx, "/container/stop", map[string]string{".id": ct.ID}); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		if err := u.waitForStopped(ctx, name); err != nil {
			return err
		}
	}

	// Step 4: Remove old container
	log.Info("removing container")
	if err := u.rosPost(ctx, "/container/remove", map[string]string{".id": ct.ID}); err != nil {
		return fmt.Errorf("removing: %w", err)
	}
	time.Sleep(2 * time.Second)

	// Step 5: Remove old root-dir to force fresh extraction
	if ct.rootDir != "" {
		log.Infow("cleaning root-dir for fresh extraction", "rootDir", ct.rootDir)
		// RouterOS file remove via REST API
		_ = u.rosPost(ctx, "/file/remove", map[string]string{".id": ct.rootDir})
		time.Sleep(time.Second)
	}

	// Step 6: Create new container from pre-staged tarball
	spec := map[string]string{
		"name":          name,
		"file":          rosPath,
		"interface":     ct.iface,
		"root-dir":      ct.rootDir,
		"logging":       ct.logging,
		"start-on-boot": ct.startOnBoot,
	}
	if ct.mountLists != "" {
		spec["mountlists"] = ct.mountLists
	}
	if ct.cmd != "" {
		spec["cmd"] = ct.cmd
	}
	if ct.entrypoint != "" {
		spec["entrypoint"] = ct.entrypoint
	}
	if ct.hostname != "" {
		spec["hostname"] = ct.hostname
	}
	if ct.dns != "" {
		spec["dns"] = ct.dns
	}
	if ct.workDir != "" {
		spec["workdir"] = ct.workDir
	}

	log.Infow("creating container from tarball", "rosPath", rosPath)
	if err := u.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating: %w", err)
	}

	// Step 7: Wait for extraction + start
	if err := u.waitForExtraction(ctx, name); err != nil {
		return err
	}

	newCt, err := u.rosGetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting container")
	if err := u.rosPost(ctx, "/container/start", map[string]string{".id": newCt.ID}); err != nil {
		return fmt.Errorf("starting: %w", err)
	}

	if err := u.waitForRunning(ctx, name); err != nil {
		return err
	}

	log.Info("container replaced successfully via tarball")
	return nil
}

// requestSelfUpdate calls the mkube update API to replace our own container.
func (u *Updater) requestSelfUpdate(ctx context.Context, name, imageRef string) error {
	u.log.Infow("requesting self-update via mkube API", "name", name, "tag", imageRef)

	// Pre-pull tarball first, then tell mkube to use it
	containerPath, rosPath, err := u.prePullTarball(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("pre-pulling tarball for self-update: %w", err)
	}
	// Don't remove tarball here — mkube needs it to recreate us
	_ = containerPath

	body, _ := json.Marshal(map[string]string{
		"name":    name,
		"tag":     imageRef,
		"tarball": rosPath,
	})

	url := fmt.Sprintf("%s/api/v1/update-container", u.cfg.MkubeAPI)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling mkube API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mkube API returned %d: %s", resp.StatusCode, string(b))
	}

	u.log.Info("self-update request accepted")
	return nil
}

// ─── Bootstrap ──────────────────────────────────────────────────────────────

// bootstrap ensures the mkube container exists on RouterOS. If missing, it
// pre-pulls the image as a tarball from the local registry and creates the
// container using the file parameter.
func (u *Updater) bootstrap(ctx context.Context) error {
	bc := u.cfg.Bootstrap
	log := u.log.With("bootstrap", bc.Container.Name)

	// Check if container already exists
	ct, err := u.rosGetContainer(ctx, bc.Container.Name)
	if err == nil {
		// Container exists
		if ct.isRunning() {
			log.Info("mkube already running, skipping bootstrap")
			return nil
		}
		// Exists but stopped — start it
		log.Info("mkube exists but stopped, starting")
		if err := u.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
			return fmt.Errorf("starting existing mkube: %w", err)
		}
		if err := u.waitForRunning(ctx, bc.Container.Name); err != nil {
			return fmt.Errorf("waiting for mkube to start: %w", err)
		}
		log.Info("mkube started")
		return nil
	}

	// Container doesn't exist — pre-pull tarball from local registry, then create.
	imageRef := bc.Image
	if strings.HasPrefix(imageRef, "ghcr.io") {
		repo := imageRef[strings.LastIndex(imageRef, "/")+1:] // "mkube:edge"
		imageRef = trimScheme(u.cfg.RegistryURL) + "/" + repo
		log.Infow("rewrote GHCR ref to local registry", "original", bc.Image, "local", imageRef)
	}

	log.Infow("mkube container not found, bootstrapping", "image", imageRef)

	// Pre-pull tarball
	_, rosPath, err := u.prePullTarball(ctx, imageRef)
	if err != nil {
		return fmt.Errorf("pre-pulling tarball for bootstrap: %w", err)
	}

	// Create container from tarball
	spec := map[string]string{
		"name":          bc.Container.Name,
		"file":          rosPath,
		"interface":     bc.Container.Interface,
		"root-dir":      bc.Container.RootDir,
		"hostname":      bc.Container.Hostname,
		"dns":           bc.Container.DNS,
		"logging":       bc.Container.Logging,
		"start-on-boot": bc.Container.StartOnBoot,
	}
	if bc.Container.MountLists != "" {
		spec["mountlists"] = bc.Container.MountLists
	}

	log.Infow("creating mkube container from tarball", "spec", spec)
	if err := u.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	// Wait for extraction
	log.Info("waiting for container extraction")
	if err := u.waitForExtraction(ctx, bc.Container.Name); err != nil {
		return fmt.Errorf("waiting for extraction: %w", err)
	}

	// Start the container
	newCt, err := u.rosGetContainer(ctx, bc.Container.Name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting mkube container")
	if err := u.rosPost(ctx, "/container/start", map[string]string{".id": newCt.ID}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	if err := u.waitForRunning(ctx, bc.Container.Name); err != nil {
		return fmt.Errorf("waiting for mkube to start: %w", err)
	}

	log.Info("mkube bootstrapped successfully")
	return nil
}

// waitForExtraction polls until the container exists and is stopped (extracted),
// with a longer timeout since image extraction can take a while.
func (u *Updater) waitForExtraction(ctx context.Context, name string) error {
	for i := 0; i < 120; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(time.Second)
		ct, err := u.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isStopped() {
			return nil
		}
		// Check for extraction failure
		if ct.status == "error" {
			return fmt.Errorf("container %s extraction failed: %s", name, ct.comment)
		}
	}
	return fmt.Errorf("container %s not extracted within 120s", name)
}

// ─── RouterOS REST helpers ──────────────────────────────────────────────────

// rosContainerFull has all fields we need to preserve during replacement.
type rosContainerFull struct {
	ID          string `json:".id"`
	Name        string `json:"name"`
	Tag         string `json:"tag"`
	Running     string `json:"running,omitempty"`
	Stopped     string `json:"stopped,omitempty"`
	iface       string
	rootDir     string
	mountLists  string
	cmd         string
	entrypoint  string
	workDir     string
	hostname    string
	dns         string
	logging     string
	startOnBoot string
	status      string
	comment     string
}

func (c rosContainerFull) isRunning() bool { return c.Running == "true" }
func (c rosContainerFull) isStopped() bool { return c.Stopped == "true" }

func (u *Updater) rosGetContainer(ctx context.Context, name string) (*rosContainerFull, error) {
	var containers []map[string]interface{}
	if err := u.rosGET(ctx, "/container", &containers); err != nil {
		return nil, err
	}

	for _, c := range containers {
		n, _ := c["name"].(string)
		if n != name {
			continue
		}
		ct := &rosContainerFull{
			Name:    n,
			Running: strVal(c, "running"),
			Stopped: strVal(c, "stopped"),
		}
		ct.ID = strVal(c, ".id")
		ct.Tag = strVal(c, "tag")
		ct.iface = strVal(c, "interface")
		ct.rootDir = strVal(c, "root-dir")
		ct.mountLists = strVal(c, "mountlists")
		ct.cmd = strVal(c, "cmd")
		ct.entrypoint = strVal(c, "entrypoint")
		ct.workDir = strVal(c, "workdir")
		ct.hostname = strVal(c, "hostname")
		ct.dns = strVal(c, "dns")
		ct.logging = strVal(c, "logging")
		ct.startOnBoot = strVal(c, "start-on-boot")
		ct.status = strVal(c, "status")
		ct.comment = strVal(c, "comment")
		return ct, nil
	}
	return nil, fmt.Errorf("container %q not found", name)
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func (u *Updater) rosGET(ctx context.Context, path string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u.cfg.RouterOSURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(u.cfg.RouterOSUser, u.cfg.RouterOSPassword)
	req.Header.Set("Accept", "application/json")

	resp, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

func (u *Updater) rosPost(ctx context.Context, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", u.cfg.RouterOSURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(u.cfg.RouterOSUser, u.cfg.RouterOSPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

// ─── Wait helpers ───────────────────────────────────────────────────────────

func (u *Updater) waitForStopped(ctx context.Context, name string) error {
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		ct, err := u.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isStopped() {
			return nil
		}
	}
	return fmt.Errorf("container %s did not stop within 30s", name)
}

func (u *Updater) waitForExists(ctx context.Context, name string) error {
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		_, err := u.rosGetContainer(ctx, name)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("container %s not found after 60s", name)
}

func (u *Updater) waitForRunning(ctx context.Context, name string) error {
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		ct, err := u.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isRunning() {
			return nil
		}
	}
	return fmt.Errorf("container %s did not start within 30s", name)
}

/// loadRegistryTransport returns an HTTP transport that trusts the registry CA.
// Falls back to skip-verify if the CA cert is not found.
func loadRegistryTransport(log *zap.SugaredLogger) http.RoundTripper {
	caFile := "/etc/mkube-update/registry-ca.crt"
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		log.Warnw("registry CA cert not found, using insecure TLS", "path", caFile)
		return &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Warnw("failed to parse registry CA cert, using insecure TLS", "path", caFile)
		return &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	log.Infow("loaded registry CA cert", "path", caFile)
	return &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
}

// trimScheme removes http:// or https:// from a URL to get a registry host.
func trimScheme(url string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
