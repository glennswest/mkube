// mkube-update: Watches a local OCI registry for image digest changes and
// replaces RouterOS containers when new images are available.
//
// For most containers, mkube-update talks directly to the RouterOS REST API.
// For its own container (self-update), it calls the mkube update API
// which performs the swap.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	Tag          string        `yaml:"tag"`           // single tag (backward compat)
	Tags         []string      `yaml:"tags,omitempty"` // ordered preference list; first found wins
	Container    string        `yaml:"container,omitempty"`  // single container
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
			for _, name := range targets {
				if err := u.requestSelfUpdate(ctx, name, imageRef); err != nil {
					u.log.Errorw("self-update request failed", "name", name, "error", err)
					// Revert digest so we retry next poll
					u.digests[key] = prev
				}
			}
		} else if w.Rolling && len(targets) > 1 {
			delay := w.RollingDelay
			if delay == 0 {
				delay = 5 * time.Second
			}
			for i, name := range targets {
				if err := u.replaceContainer(ctx, name, imageRef); err != nil {
					u.log.Errorw("rolling update failed", "name", name, "error", err)
					// Revert digest so we retry next poll
					u.digests[key] = prev
					break
				}
				if i < len(targets)-1 {
					time.Sleep(delay)
				}
			}
		} else {
			for _, name := range targets {
				if err := u.replaceContainer(ctx, name, imageRef); err != nil {
					u.log.Errorw("container replacement failed", "name", name, "error", err)
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

// replaceContainer stops, removes, and recreates a container via RouterOS REST API.
func (u *Updater) replaceContainer(ctx context.Context, name, imageRef string) error {
	log := u.log.With("container", name, "image", imageRef)

	// Get current container
	ct, err := u.rosGetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("getting container: %w", err)
	}

	// Stop if running
	if ct.isRunning() {
		log.Info("stopping container")
		if err := u.rosPost(ctx, "/container/stop", map[string]string{".id": ct.ID}); err != nil {
			return fmt.Errorf("stopping: %w", err)
		}
		if err := u.waitForStopped(ctx, name); err != nil {
			return err
		}
	}

	// Remove
	log.Info("removing container")
	if err := u.rosPost(ctx, "/container/remove", map[string]string{".id": ct.ID}); err != nil {
		return fmt.Errorf("removing: %w", err)
	}

	// Wait for removal
	time.Sleep(2 * time.Second)

	// Recreate — we need the original container's full config
	// Re-read to get all fields (the list endpoint returns everything)
	// Since we just removed it, we build the spec from what we had.
	// Use remote-image (not tag) for OCI pulls, and skip RouterOS cert
	// check since it doesn't have our custom CA.
	spec := map[string]string{
		"name":              name,
		"remote-image":      imageRef,
		"check-certificate": "no",
		"interface":         ct.iface,
		"root-dir":          ct.rootDir,
		"logging":           ct.logging,
		"start-on-boot":     ct.startOnBoot,
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

	log.Info("creating container with new image")
	if err := u.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating: %w", err)
	}

	// Wait for extraction + start
	if err := u.waitForExists(ctx, name); err != nil {
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

	// Verify running
	if err := u.waitForRunning(ctx, name); err != nil {
		return err
	}

	log.Info("container replaced successfully")
	return nil
}

// requestSelfUpdate calls the mkube update API to replace our own container.
func (u *Updater) requestSelfUpdate(ctx context.Context, name, imageRef string) error {
	u.log.Infow("requesting self-update via mkube API", "name", name, "tag", imageRef)

	body, _ := json.Marshal(map[string]string{
		"name": name,
		"tag":  imageRef,
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
// pulls the image from GHCR, creates a docker-save tarball, and creates the
// container via the RouterOS REST API.
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

	// Container doesn't exist — create it via remote-image from local registry.
	// The installer seeds all images into the local registry, so we pull from
	// there. Scratch containers have no system root CAs so GHCR TLS would fail.
	imageRef := bc.Image
	if strings.HasPrefix(imageRef, "ghcr.io") {
		repo := imageRef[strings.LastIndex(imageRef, "/")+1:] // "mkube:edge"
		imageRef = trimScheme(u.cfg.RegistryURL) + "/" + repo
		log.Infow("rewrote GHCR ref to local registry", "original", bc.Image, "local", imageRef)
	}

	log.Infow("mkube container not found, bootstrapping", "image", imageRef)

	// Create container via remote-image (RouterOS pulls from registry directly)
	spec := map[string]string{
		"name":              bc.Container.Name,
		"remote-image":      imageRef,
		"check-certificate": "no",
		"interface":         bc.Container.Interface,
		"root-dir":          bc.Container.RootDir,
		"hostname":          bc.Container.Hostname,
		"dns":               bc.Container.DNS,
		"logging":           bc.Container.Logging,
		"start-on-boot":     bc.Container.StartOnBoot,
	}
	if bc.Container.MountLists != "" {
		spec["mountlists"] = bc.Container.MountLists
	}

	log.Infow("creating mkube container", "spec", spec)
	if err := u.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	// Wait for extraction (RouterOS downloads + extracts the image)
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
