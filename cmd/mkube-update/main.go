// mkube-update: Watches a local OCI registry for image digest changes and
// replaces RouterOS containers when new images are available.
//
// For most containers, mkube-update talks directly to the RouterOS REST API.
// For its own container (self-update), it calls the microkube update API
// which performs the swap.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var version = "dev"

// Config is the top-level configuration loaded from YAML.
type Config struct {
	RegistryURL     string        `yaml:"registryURL"`
	RouterOSURL     string        `yaml:"routerosURL"`
	RouterOSUser    string        `yaml:"routerosUser"`
	RouterOSPassword string       `yaml:"routerosPassword"`
	MkubeAPI        string        `yaml:"mkubeAPI"`
	PollSeconds     int           `yaml:"pollSeconds"`
	Watches         []WatchEntry  `yaml:"watches"`
}

// WatchEntry defines a single image to watch in the local registry.
type WatchEntry struct {
	Repo         string        `yaml:"repo"`
	Tag          string        `yaml:"tag"`
	Container    string        `yaml:"container,omitempty"`    // single container
	Containers   []string      `yaml:"containers,omitempty"`   // multiple containers
	SelfUpdate   bool          `yaml:"selfUpdate,omitempty"`
	Rolling      bool          `yaml:"rolling,omitempty"`
	RollingDelay time.Duration `yaml:"rollingDelay,omitempty"`
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

// rosContainer is a minimal RouterOS container representation.
type rosContainer struct {
	ID      string `json:".id"`
	Name    string `json:"name"`
	Running string `json:"running,omitempty"`
	Stopped string `json:"stopped,omitempty"`
}

func (c rosContainer) isRunning() bool { return c.Running == "true" }
func (c rosContainer) isStopped() bool { return c.Stopped == "true" }

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
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
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 30 * time.Second,
		},
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
			srv.Shutdown(context.Background())
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
		digest, err := u.getDigest(ctx, w.Repo, w.Tag)
		if err != nil {
			u.log.Warnw("failed to get digest", "repo", w.Repo, "tag", w.Tag, "error", err)
			continue
		}

		key := w.Repo + ":" + w.Tag
		prev, seen := u.digests[key]
		u.digests[key] = digest

		if !seen {
			u.log.Infow("initial digest recorded", "repo", w.Repo, "tag", w.Tag, "digest", digest)
			continue
		}

		if digest == prev {
			continue
		}

		u.log.Infow("digest changed", "repo", w.Repo, "tag", w.Tag,
			"old", prev, "new", digest)

		targets := w.Targets()
		if len(targets) == 0 {
			u.log.Warnw("no target containers for watch", "repo", w.Repo)
			continue
		}

		imageRef := fmt.Sprintf("%s/%s:%s",
			trimScheme(u.cfg.RegistryURL), w.Repo, w.Tag)

		if w.SelfUpdate {
			// Self-update: ask microkube to replace us
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
	// Since we just removed it, we build the spec from what we had
	spec := map[string]string{
		"name":         name,
		"tag":          imageRef,
		"interface":    ct.iface,
		"root-dir":     ct.rootDir,
		"logging":      ct.logging,
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

// requestSelfUpdate calls the microkube update API to replace our own container.
func (u *Updater) requestSelfUpdate(ctx context.Context, name, imageRef string) error {
	u.log.Infow("requesting self-update via microkube API", "name", name, "tag", imageRef)

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

// trimScheme removes http:// or https:// from a URL to get a registry host.
func trimScheme(url string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
