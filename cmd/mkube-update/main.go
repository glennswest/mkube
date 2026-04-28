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
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	nurl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/routeros"
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
	RouterOSURL      string          `yaml:"routerosURL"`      // legacy REST URL — host is reused for native API when routerosAddress is empty
	RouterOSAddress  string          `yaml:"routerosAddress"`  // native API endpoint, e.g. "192.168.200.1:8728"
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
		cfg.PollSeconds = 15
	}
	if cfg.TarballDir == "" {
		cfg.TarballDir = "/data/staging"
	}
	if cfg.TarballROSPath == "" {
		cfg.TarballROSPath = "raid1/volumes/mkube-update-updater/data/staging"
	}

	rosAddr := cfg.RouterOSAddress
	if rosAddr == "" {
		rosAddr = deriveNativeAddress(cfg.RouterOSURL)
	}
	if rosAddr == "" {
		log.Fatalw("RouterOS address not configured", "routerosURL", cfg.RouterOSURL, "routerosAddress", cfg.RouterOSAddress)
	}

	rosClient, err := routeros.NewClient(config.RouterOSConfig{
		Address:        rosAddr,
		User:           cfg.RouterOSUser,
		Password:       cfg.RouterOSPassword,
		InsecureVerify: true,
	})
	if err != nil {
		log.Fatalw("creating RouterOS client", "address", rosAddr, "error", err)
	}
	log.Infow("RouterOS native API client ready", "address", rosAddr)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	updater := &Updater{
		cfg:      cfg,
		log:      log,
		digests:  make(map[string]string),
		kickPoll: make(chan struct{}, 1),
		ros:      rosClient,
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
	cfg      Config
	log      *zap.SugaredLogger
	digests  map[string]string // "repo:tag" → last seen digest
	http     *http.Client      // for registry / mkube API calls
	ros      *routeros.Client  // native RouterOS API (port 8728), single TCP connection
	kickPoll chan struct{}     // SSE-triggered immediate poll
}

func (u *Updater) run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(u.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Start SSE watcher for instant push notifications
	go u.watchSSE(ctx)

	// Start half-open socket watcher for instant mkube crash detection.
	// Maintains a persistent TCP connection to mkube — when mkube dies,
	// the socket breaks immediately and we restart it.
	go u.watchMkubeSocket(ctx)

	// Initial poll immediately
	u.poll(ctx)
	u.checkContainerHealth(ctx)

	for {
		select {
		case <-ctx.Done():
			u.log.Info("shutting down")
			return
		case <-ticker.C:
			u.poll(ctx)
			u.checkContainerHealth(ctx)
		case <-u.kickPoll:
			u.log.Info("SSE push event, immediate poll")
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

// checkContainerHealth checks all watched containers and restarts any that
// have stopped (crashed). Always restarts immediately — mkube is critical
// infrastructure that must be up.
func (u *Updater) checkContainerHealth(ctx context.Context) {
	seen := make(map[string]bool)
	for _, w := range u.cfg.Watches {
		if w.SelfUpdate {
			continue // don't restart ourselves
		}
		for _, tgt := range w.Targets() {
			if seen[tgt] {
				continue
			}
			seen[tgt] = true
			u.checkAndRestart(ctx, tgt)
		}
	}
}

func (u *Updater) checkAndRestart(ctx context.Context, name string) {
	ct, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		// Container doesn't exist — if it's the bootstrap container (mkube),
		// re-bootstrap it instead of silently skipping.
		if u.cfg.Bootstrap.Enabled && name == u.cfg.Bootstrap.Container.Name {
			u.log.Warnw("watchdog: bootstrap container missing, re-bootstrapping", "container", name)
			if err := u.bootstrap(ctx); err != nil {
				u.log.Errorw("watchdog: re-bootstrap failed", "container", name, "error", err)
			}
		}
		return
	}

	if ct.IsRunning() || !ct.IsStopped() {
		return
	}

	comment := ct.Comment
	if comment == "" {
		comment = "no error detail"
	}

	u.log.Warnw("watchdog: container stopped, restarting immediately",
		"container", name, "comment", comment)

	if err := u.ros.StartContainer(ctx, ct.ID); err != nil {
		u.log.Errorw("watchdog: restart failed", "container", name, "error", err)
		return
	}

	if err := u.waitForRunning(ctx, name); err != nil {
		u.log.Errorw("watchdog: container did not start", "container", name, "error", err)
		return
	}

	u.log.Infow("watchdog: container restarted successfully", "container", name)
}

// watchMkubeSocket maintains a persistent TCP connection to the mkube API.
// When mkube crashes, the kernel sends RST/FIN and the read returns immediately.
// This gives sub-second crash detection vs. polling every N seconds.
func (u *Updater) watchMkubeSocket(ctx context.Context) {
	if u.cfg.MkubeAPI == "" {
		u.log.Info("watchMkubeSocket: no mkubeAPI configured, skipping")
		return
	}

	parsed, err := nurl.Parse(u.cfg.MkubeAPI)
	if err != nil {
		u.log.Warnw("watchMkubeSocket: bad mkubeAPI URL", "error", err)
		return
	}
	host := parsed.Host
	if !strings.Contains(host, ":") {
		host += ":8082"
	}

	u.log.Infow("half-open socket watcher starting", "target", host)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := net.DialTimeout("tcp", host, 5*time.Second)
		if err != nil {
			// mkube not up yet — check and restart, then wait before retry
			u.log.Debugw("watchMkubeSocket: connect failed", "error", err)
			u.checkContainerHealth(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		u.log.Infow("half-open socket connected to mkube", "target", host)

		// Keep reading — when mkube dies, Read returns error immediately.
		// Set a generous read deadline so we don't time out on idle connections.
		buf := make([]byte, 1)
		for {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			_, err := conn.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Timeout is fine — connection still alive, reset deadline
					continue
				}
				// Connection dropped — mkube likely crashed
				u.log.Warnw("half-open socket: mkube connection lost, restarting",
					"target", host, "error", err)
				conn.Close()
				u.checkContainerHealth(ctx)
				break
			}
		}

		// Brief cooldown before reconnecting
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
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

	// Preserve previous tarball for rollback
	prevPath := strings.TrimSuffix(containerPath, ".tar") + "-prev.tar"
	if _, statErr := os.Stat(containerPath); statErr == nil {
		_ = os.Remove(prevPath) // remove any stale prev
		if renameErr := os.Rename(containerPath, prevPath); renameErr != nil {
			log.Warnw("failed to preserve previous tarball", "error", renameErr)
		} else {
			log.Infow("preserved previous tarball for rollback", "path", prevPath)
		}
	}

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

		tmpFile, err := os.CreateTemp(filepath.Dir(outputPath), "layer-*.tar")
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
	// Tarball cleanup is handled after health verification — not deferred

	// Step 2: Get current container config
	ct, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		// Container missing — if it's the bootstrap container, recreate via bootstrap
		if u.cfg.Bootstrap.Enabled && name == u.cfg.Bootstrap.Container.Name {
			log.Warnw("container missing during replace, re-bootstrapping", "error", err)
			return u.bootstrap(ctx)
		}
		return fmt.Errorf("getting container: %w", err)
	}

	// Step 3: Stop if running
	if ct.IsRunning() {
		log.Info("stopping container")
		if err := u.ros.StopContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		if err := u.waitForStopped(ctx, name); err != nil {
			return err
		}
	}

	// Step 4: Remove old container
	log.Info("removing container")
	if err := u.ros.RemoveContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("removing: %w", err)
	}
	time.Sleep(2 * time.Second)

	// Step 5: Remove old root-dir to force fresh extraction.
	// If root-dir survives, RouterOS skips extraction and reuses the old binary.
	if ct.RootDir != "" {
		log.Infow("cleaning root-dir for fresh extraction", "rootDir", ct.RootDir)
		if err := u.ros.RemoveDirectory(ctx, ct.RootDir); err != nil {
			log.Warnw("failed to remove root-dir, continuing", "rootDir", ct.RootDir, "error", err)
		}
		time.Sleep(time.Second)
	}

	// Step 6: Create new container from pre-staged tarball
	spec := routeros.ContainerSpec{
		Name:        name,
		File:        rosPath,
		Interface:   ct.Interface,
		RootDir:     ct.RootDir,
		MountLists:  ct.MountLists,
		Cmd:         ct.Cmd,
		Entrypoint:  ct.Entrypoint,
		WorkDir:     ct.WorkDir,
		Hostname:    ct.Hostname,
		DNS:         ct.DNS,
		Logging:     ct.Logging,
		StartOnBoot: ct.StartOnBoot,
	}

	log.Infow("creating container from tarball", "rosPath", rosPath)
	if err := u.ros.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("creating: %w", err)
	}

	// Step 7: Wait for extraction + start
	if err := u.waitForExtraction(ctx, name); err != nil {
		return err
	}

	newCt, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting container")
	if err := u.ros.StartContainer(ctx, newCt.ID); err != nil {
		return fmt.Errorf("starting: %w", err)
	}

	if err := u.waitForRunning(ctx, name); err != nil {
		return err
	}

	// Step 8: Post-replace health verification
	healthDuration := 15 * time.Second
	prevContainerPath := strings.TrimSuffix(containerPath, ".tar") + "-prev.tar"
	prevROSPath := strings.TrimSuffix(rosPath, ".tar") + "-prev.tar"

	if err := u.verifyHealth(ctx, name, healthDuration); err != nil {
		log.Errorw("new image failed health check", "error", err)

		// Retry restart up to 3 times
		recovered := false
		for attempt := 1; attempt <= 3; attempt++ {
			log.Infow("restart attempt after health failure", "attempt", attempt, "max", 3)
			if restartErr := u.restartContainer(ctx, name); restartErr != nil {
				log.Warnw("restart failed", "attempt", attempt, "error", restartErr)
				continue
			}
			if u.verifyHealth(ctx, name, healthDuration) == nil {
				log.Infow("container recovered after restart", "attempt", attempt)
				recovered = true
				break
			}
		}

		if !recovered {
			// Check if we have a previous tarball to rollback to
			if _, statErr := os.Stat(prevContainerPath); statErr == nil {
				log.Warn("all restart attempts failed, rolling back to previous image")
				if rollbackErr := u.rollbackContainer(ctx, name, ct, prevROSPath, healthDuration); rollbackErr != nil {
					os.Remove(containerPath)
					os.Remove(prevContainerPath)
					return fmt.Errorf("CRITICAL: new image failed and rollback failed: %w", rollbackErr)
				}
				os.Remove(containerPath)
				os.Remove(prevContainerPath)
				return fmt.Errorf("new image failed health check, rolled back to previous image")
			}
			os.Remove(containerPath)
			return fmt.Errorf("new image failed health check, no previous image available for rollback")
		}
	}

	// Healthy — clean up tarballs
	os.Remove(containerPath)
	os.Remove(prevContainerPath)
	log.Info("container replaced and verified healthy")
	return nil
}

// verifyHealth polls the container status for the given duration. If the
// container stops (crashes) during this window, it returns an error. If it
// stays running for the full duration, it returns nil.
func (u *Updater) verifyHealth(ctx context.Context, name string, duration time.Duration) error {
	log := u.log.With("container", name)
	log.Infow("verifying container health", "duration", duration)

	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}

		ct, err := u.ros.GetContainer(ctx, name)
		if err != nil {
			return fmt.Errorf("health check: container disappeared: %w", err)
		}
		if ct.IsStopped() {
			return fmt.Errorf("health check: container stopped (crashed) during verification")
		}
		if !ct.IsRunning() {
			return fmt.Errorf("health check: container in unexpected state")
		}
	}

	log.Info("health check passed")
	return nil
}

// restartContainer starts a stopped container and waits for it to be running.
func (u *Updater) restartContainer(ctx context.Context, name string) error {
	ct, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting container for restart: %w", err)
	}
	if err := u.ros.StartContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}
	return u.waitForRunning(ctx, name)
}

// rollbackContainer stops the current (failed) container, removes it, cleans
// the root-dir, and recreates from the previous tarball. Verifies health after
// rollback with up to 3 restart retries if the initial health check fails.
func (u *Updater) rollbackContainer(ctx context.Context, name string, origSpec *routeros.Container, prevROSPath string, healthDuration time.Duration) error {
	log := u.log.With("container", name)

	// Stop and remove the failing container
	ct, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		log.Warnw("rollback: could not find container to stop", "error", err)
	} else {
		if ct.IsRunning() {
			_ = u.ros.StopContainer(ctx, ct.ID)
			_ = u.waitForStopped(ctx, name)
		}
		_ = u.ros.RemoveContainer(ctx, ct.ID)
		time.Sleep(2 * time.Second)
	}

	// Clean root-dir
	if origSpec.RootDir != "" {
		_ = u.ros.RemoveDirectory(ctx, origSpec.RootDir)
		time.Sleep(time.Second)
	}

	// Create from previous tarball
	spec := routeros.ContainerSpec{
		Name:        name,
		File:        prevROSPath,
		Interface:   origSpec.Interface,
		RootDir:     origSpec.RootDir,
		MountLists:  origSpec.MountLists,
		Cmd:         origSpec.Cmd,
		Entrypoint:  origSpec.Entrypoint,
		WorkDir:     origSpec.WorkDir,
		Hostname:    origSpec.Hostname,
		DNS:         origSpec.DNS,
		Logging:     origSpec.Logging,
		StartOnBoot: origSpec.StartOnBoot,
	}

	log.Infow("rollback: creating container from previous tarball", "rosPath", prevROSPath)
	if err := u.ros.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("rollback create failed: %w", err)
	}

	if err := u.waitForExtraction(ctx, name); err != nil {
		return fmt.Errorf("rollback extraction failed: %w", err)
	}

	newCt, err := u.ros.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("rollback: getting container: %w", err)
	}

	log.Info("rollback: starting container")
	if err := u.ros.StartContainer(ctx, newCt.ID); err != nil {
		return fmt.Errorf("rollback start failed: %w", err)
	}

	if err := u.waitForRunning(ctx, name); err != nil {
		return fmt.Errorf("rollback: container did not start: %w", err)
	}

	// Verify health
	if u.verifyHealth(ctx, name, healthDuration) == nil {
		log.Info("rollback successful")
		return nil
	}

	// Retry restarts on rollback
	for attempt := 1; attempt <= 3; attempt++ {
		log.Infow("rollback restart attempt", "attempt", attempt, "max", 3)
		if restartErr := u.restartContainer(ctx, name); restartErr != nil {
			continue
		}
		if u.verifyHealth(ctx, name, healthDuration) == nil {
			log.Infow("rollback recovered after restart", "attempt", attempt)
			return nil
		}
	}

	return fmt.Errorf("rollback also failed after 3 restart attempts")
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

	// Check if container already exists — retry a few times since RouterOS
	// may not be reachable immediately after container start.
	var ct *routeros.Container
	for attempt := 0; attempt < 5; attempt++ {
		var err error
		ct, err = u.ros.GetContainer(ctx, bc.Container.Name)
		if err == nil {
			break
		}
		log.Warnw("bootstrap: could not query containers, retrying", "attempt", attempt+1, "error", err)
		time.Sleep(3 * time.Second)
	}
	if ct != nil {
		// Container exists
		if ct.IsRunning() {
			log.Info("mkube already running, skipping bootstrap")
			return nil
		}
		// Exists but stopped — start it
		log.Info("mkube exists but stopped, starting")
		if err := u.ros.StartContainer(ctx, ct.ID); err != nil {
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
	spec := routeros.ContainerSpec{
		Name:        bc.Container.Name,
		File:        rosPath,
		Interface:   bc.Container.Interface,
		RootDir:     bc.Container.RootDir,
		MountLists:  bc.Container.MountLists,
		Hostname:    bc.Container.Hostname,
		DNS:         bc.Container.DNS,
		Logging:     bc.Container.Logging,
		StartOnBoot: bc.Container.StartOnBoot,
	}

	log.Infow("creating mkube container from tarball", "name", spec.Name, "rosPath", rosPath)
	if err := u.ros.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	// Wait for extraction
	log.Info("waiting for container extraction")
	if err := u.waitForExtraction(ctx, bc.Container.Name); err != nil {
		return fmt.Errorf("waiting for extraction: %w", err)
	}

	// Start the container
	newCt, err := u.ros.GetContainer(ctx, bc.Container.Name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting mkube container")
	if err := u.ros.StartContainer(ctx, newCt.ID); err != nil {
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
		ct, err := u.ros.GetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.IsStopped() {
			return nil
		}
		// Extraction failure surfaces in Comment (e.g. "could not acquire interface").
		// Container.Stopped will eventually be true even on failure; we rely on that
		// path plus the timeout. The Comment is logged by checkAndRestart on watchdog.
	}
	return fmt.Errorf("container %s not extracted within 120s", name)
}

// ─── Wait helpers ───────────────────────────────────────────────────────────

func (u *Updater) waitForStopped(ctx context.Context, name string) error {
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		ct, err := u.ros.GetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.IsStopped() {
			return nil
		}
	}
	return fmt.Errorf("container %s did not stop within 30s", name)
}

func (u *Updater) waitForRunning(ctx context.Context, name string) error {
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		ct, err := u.ros.GetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.IsRunning() {
			return nil
		}
	}
	return fmt.Errorf("container %s did not start within 30s", name)
}

// deriveNativeAddress derives a host:8728 native API address from a legacy
// REST URL (e.g. "http://192.168.200.1/rest" → "192.168.200.1:8728"). Returns
// "" if the URL cannot be parsed; the caller should treat that as fatal.
func deriveNativeAddress(restURL string) string {
	if restURL == "" {
		return ""
	}
	parsed, err := nurl.Parse(restURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	return host + ":8728"
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

// watchSSE connects to the registry management SSE endpoint and triggers
// immediate polls when push events arrive. Reconnects with backoff on errors.
// Polling continues as fallback when SSE is unavailable.
func (u *Updater) watchSSE(ctx context.Context) {
	sseURL := u.managementURL() + "/events"
	u.log.Infow("SSE watcher starting", "url", sseURL)

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := u.readSSEStream(ctx, sseURL)
		if err != nil && ctx.Err() == nil {
			u.log.Warnw("SSE connection error, reconnecting", "error", err, "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential backoff capped at maxBackoff
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (u *Updater) readSSEStream(ctx context.Context, sseURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// SSE is a long-lived stream — use a client without a request timeout.
	// The context cancellation handles shutdown; the 30s u.http timeout
	// would kill every SSE connection prematurely.
	sseClient := &http.Client{
		Transport: u.http.Transport,
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE endpoint returned %d", resp.StatusCode)
	}

	u.log.Info("SSE connected")
	// Reset backoff on successful connection (caller will reset)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: push") {
			// Next data line is the push event — trigger poll
			select {
			case u.kickPoll <- struct{}{}:
			default:
			}
		}
	}
	return scanner.Err()
}

// managementURL derives the registry management API URL (:5001) from the
// registry URL. Replaces the port with 5001 and forces HTTP scheme since
// the management API serves plain HTTP only.
func (u *Updater) managementURL() string {
	url := u.cfg.RegistryURL
	// Replace :5000 with :5001 if present
	url = strings.Replace(url, ":5000", ":5001", 1)
	// Management API on :5001 is always plain HTTP
	url = strings.Replace(url, "https://", "http://", 1)
	return url
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
