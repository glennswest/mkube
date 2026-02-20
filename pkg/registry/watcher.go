package registry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

// ociManifest is a minimal representation for parsing manifest JSON.
type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Config    ociDescriptor   `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
	Manifests []ociDescriptor `json:"manifests"` // index/list only
}

type ociDescriptor struct {
	MediaType string       `json:"mediaType"`
	Digest    string       `json:"digest"`
	Size      int64        `json:"size"`
	Platform  *ociPlatform `json:"platform,omitempty"`
}

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// ImageWatcher periodically polls upstream registries for digest changes
// on configured images. When a new digest is detected, it pulls the
// manifest and blobs into the local registry store and emits a PushEvent.
type ImageWatcher struct {
	cfg    config.RegistryConfig
	store  *BlobStore
	events chan<- PushEvent
	log    *zap.SugaredLogger
	client *http.Client

	pollTrigger chan struct{}

	mu          sync.Mutex
	lastDigests map[string]string // upstream ref → last seen digest
}

// NewImageWatcher creates a watcher that checks upstream registries for
// new image versions and mirrors them into the local store.
func NewImageWatcher(cfg config.RegistryConfig, store *BlobStore, events chan<- PushEvent, log *zap.SugaredLogger) *ImageWatcher {
	return &ImageWatcher{
		cfg:    cfg,
		store:  store,
		events: events,
		log:    log.Named("image-watcher"),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			},
		},
		pollTrigger: make(chan struct{}, 1),
		lastDigests: make(map[string]string),
	}
}

// Run starts the polling loop. It blocks until ctx is cancelled.
func (w *ImageWatcher) Run(ctx context.Context) {
	if len(w.cfg.WatchImages) == 0 {
		w.log.Info("no images configured for watching")
		return
	}

	interval := time.Duration(w.cfg.WatchPollSeconds) * time.Second
	if interval < 30*time.Second {
		interval = 2 * time.Minute
	}

	// Validate stored manifests on startup — remove any HTML or corrupted entries
	// that may have been cached by a broken pull-through or upstream misconfiguration.
	if removed := w.store.ValidateManifests(); removed > 0 {
		w.log.Warnw("removed corrupted manifest entries on startup", "count", removed)
	}

	w.log.Infow("image watcher started",
		"images", len(w.cfg.WatchImages),
		"interval", interval,
	)

	// Initial check immediately
	w.checkAll(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("image watcher shutting down")
			return
		case <-ticker.C:
			w.checkAll(ctx)
		case <-w.pollTrigger:
			w.log.Info("manual poll triggered")
			w.checkAll(ctx)
		}
	}
}

// TriggerPoll requests an immediate poll cycle. Non-blocking; if a
// trigger is already pending it is a no-op.
func (w *ImageWatcher) TriggerPoll() {
	select {
	case w.pollTrigger <- struct{}{}:
	default:
	}
}

func (w *ImageWatcher) checkAll(ctx context.Context) {
	for _, img := range w.cfg.WatchImages {
		if ctx.Err() != nil {
			return
		}
		w.checkImage(ctx, img)
	}
}

func (w *ImageWatcher) checkImage(ctx context.Context, img config.WatchImage) {
	host, repo, ref := parseUpstreamRef(img.Upstream)
	if host == "" || repo == "" {
		w.log.Warnw("invalid upstream image ref", "upstream", img.Upstream)
		return
	}

	// Get the current digest from upstream via HEAD on the manifest
	digest, err := w.headManifest(ctx, host, repo, ref)
	if err != nil {
		w.log.Debugw("failed to check upstream digest",
			"upstream", img.Upstream, "error", err)
		return
	}

	w.mu.Lock()
	lastDigest := w.lastDigests[img.Upstream]
	w.mu.Unlock()

	if digest == lastDigest {
		return // no change
	}

	if lastDigest == "" {
		w.log.Infow("initial digest recorded",
			"upstream", img.Upstream, "digest", truncDigest(digest))
	} else {
		w.log.Infow("new image digest detected",
			"upstream", img.Upstream,
			"old", truncDigest(lastDigest),
			"new", truncDigest(digest))
	}

	// Pull manifest + blobs into local store
	if err := w.mirrorImage(ctx, host, repo, ref, img.LocalRepo); err != nil {
		w.log.Errorw("failed to mirror image",
			"upstream", img.Upstream, "error", err)
		return
	}

	w.mu.Lock()
	w.lastDigests[img.Upstream] = digest
	w.mu.Unlock()

	// Only emit push event if this isn't the first check (we don't want
	// to trigger a rolling update on startup just because we learned the digest)
	if lastDigest != "" {
		select {
		case w.events <- PushEvent{
			Repo:      img.LocalRepo,
			Reference: ref,
			Digest:    digest,
			Time:      time.Now(),
		}:
			w.log.Infow("push event emitted for upstream change",
				"repo", img.LocalRepo, "ref", ref)
		default:
			w.log.Warnw("push event channel full", "repo", img.LocalRepo)
		}
	}
}

// headManifest does a HEAD request to the upstream registry to get the
// current digest for a manifest reference.
func (w *ImageWatcher) headManifest(ctx context.Context, host, repo, ref string) (string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", resolveRegistryHost(host), repo, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}

	// Accept common manifest media types
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))

	// Try with anonymous auth first; if 401, try to get a token
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HEAD %s: %w", url, err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Try bearer token auth
		token, err := w.getToken(ctx, resp, host, repo)
		if err != nil {
			return "", fmt.Errorf("auth for %s/%s: %w", host, repo, err)
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
		}, ", "))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = w.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("HEAD %s (authed): %w", url, err)
		}
		resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD %s returned %d", url, resp.StatusCode)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no Docker-Content-Digest in response")
	}

	return digest, nil
}

// mirrorImage pulls a manifest and all referenced blobs from upstream
// into the local blob store.
func (w *ImageWatcher) mirrorImage(ctx context.Context, host, repo, ref, localRepo string) error {
	// Fetch the manifest
	manifestData, contentType, err := w.fetchManifest(ctx, host, repo, ref)
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	// Validate manifest is actually JSON before storing
	if !isJSONContentType(contentType) && !json.Valid(manifestData) {
		return fmt.Errorf("manifest from %s/%s:%s is not JSON (content-type: %s), skipping", host, repo, ref, contentType)
	}

	// Parse manifest to determine if it's an index or a single manifest
	var manifest ociManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	// If this is a manifest index/list, fetch the arm64/linux child manifest
	if len(manifest.Manifests) > 0 {
		if err := w.mirrorIndex(ctx, host, repo, ref, localRepo, manifestData, contentType, manifest); err != nil {
			return err
		}
		return nil
	}

	// Single manifest — fetch config + layer blobs
	if err := w.mirrorBlobs(ctx, host, repo, manifest); err != nil {
		return err
	}

	// Store manifest under local repo name
	if err := w.store.PutManifest(localRepo, ref, contentType, bytes.NewReader(manifestData)); err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}

	w.log.Infow("mirrored manifest + blobs",
		"upstream", fmt.Sprintf("%s/%s:%s", host, repo, ref),
		"local", fmt.Sprintf("%s:%s", localRepo, ref),
		"blobs", 1+len(manifest.Layers),
		"size", len(manifestData))

	return nil
}

// mirrorIndex handles a manifest index/list by selecting the arm64/linux
// child, fetching it and its blobs, then storing both the child and the index.
func (w *ImageWatcher) mirrorIndex(ctx context.Context, host, repo, ref, localRepo string, indexData []byte, indexContentType string, index ociManifest) error {
	// Find the arm64/linux manifest (or fall back to first entry)
	var target *ociDescriptor
	for i := range index.Manifests {
		m := &index.Manifests[i]
		if m.Platform != nil && m.Platform.Architecture == "arm64" && m.Platform.OS == "linux" {
			target = m
			break
		}
	}
	if target == nil && len(index.Manifests) > 0 {
		target = &index.Manifests[0]
		w.log.Warnw("no arm64/linux manifest in index, using first entry",
			"repo", repo, "digest", target.Digest)
	}
	if target == nil {
		return fmt.Errorf("empty manifest index for %s/%s:%s", host, repo, ref)
	}

	// Fetch the child manifest by digest
	childData, childCT, err := w.fetchManifest(ctx, host, repo, target.Digest)
	if err != nil {
		return fmt.Errorf("fetching child manifest %s: %w", truncDigest(target.Digest), err)
	}

	var child ociManifest
	if err := json.Unmarshal(childData, &child); err != nil {
		return fmt.Errorf("parsing child manifest: %w", err)
	}

	// Fetch blobs for the child manifest
	if err := w.mirrorBlobs(ctx, host, repo, child); err != nil {
		return err
	}

	// Store child manifest by digest
	if err := w.store.PutManifest(localRepo, target.Digest, childCT, bytes.NewReader(childData)); err != nil {
		return fmt.Errorf("storing child manifest: %w", err)
	}

	// Store the index manifest under the tag
	if err := w.store.PutManifest(localRepo, ref, indexContentType, bytes.NewReader(indexData)); err != nil {
		return fmt.Errorf("storing index manifest: %w", err)
	}

	w.log.Infow("mirrored index + child manifest + blobs",
		"upstream", fmt.Sprintf("%s/%s:%s", host, repo, ref),
		"local", fmt.Sprintf("%s:%s", localRepo, ref),
		"childDigest", truncDigest(target.Digest),
		"blobs", 1+len(child.Layers))

	return nil
}

// mirrorBlobs fetches config + layer blobs for a single manifest, skipping
// blobs that already exist in the store.
func (w *ImageWatcher) mirrorBlobs(ctx context.Context, host, repo string, manifest ociManifest) error {
	// Collect all blob digests: config + layers
	var digests []string
	if manifest.Config.Digest != "" {
		digests = append(digests, manifest.Config.Digest)
	}
	for _, layer := range manifest.Layers {
		digests = append(digests, layer.Digest)
	}

	for _, digest := range digests {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip if already in store
		if exists, _ := w.store.HasBlob(digest); exists {
			continue
		}

		data, err := w.fetchBlob(ctx, host, repo, digest)
		if err != nil {
			return fmt.Errorf("fetching blob %s: %w", truncDigest(digest), err)
		}

		if err := w.store.PutBlob(digest, bytes.NewReader(data)); err != nil {
			return fmt.Errorf("storing blob %s: %w", truncDigest(digest), err)
		}

		w.log.Debugw("mirrored blob",
			"repo", repo, "digest", truncDigest(digest), "size", len(data))
	}

	return nil
}

// fetchBlob gets a blob from upstream by digest.
func (w *ImageWatcher) fetchBlob(ctx context.Context, host, repo, digest string) ([]byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", resolveRegistryHost(host), repo, digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		token, err := w.getToken(ctx, resp, host, repo)
		if err != nil {
			return nil, err
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = w.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	// Validate we didn't get HTML back
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("blob response is HTML, not a blob (upstream %s may require auth)", host)
	}

	return io.ReadAll(resp.Body)
}

// fetchManifest gets the full manifest from upstream.
func (w *ImageWatcher) fetchManifest(ctx context.Context, host, repo, ref string) ([]byte, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", resolveRegistryHost(host), repo, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Retry with token
		token, err := w.getToken(ctx, resp, host, repo)
		if err != nil {
			return nil, "", err
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
		}, ", "))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = w.client.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	return data, resp.Header.Get("Content-Type"), nil
}

// getToken parses WWW-Authenticate header and fetches a bearer token.
// Supports the standard Docker/OCI token flow:
//
//	WWW-Authenticate: Bearer realm="https://...",service="...",scope="repository:...:pull"
func (w *ImageWatcher) getToken(ctx context.Context, resp *http.Response, host, repo string) (string, error) {
	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		return "", fmt.Errorf("no WWW-Authenticate header")
	}

	// Parse Bearer realm="...",service="...",scope="..."
	params := parseWWWAuthenticate(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in WWW-Authenticate")
	}

	service := params["service"]
	scope := params["scope"]
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", repo)
	}

	tokenURL := fmt.Sprintf("%s?service=%s&scope=%s", realm, service, scope)
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}

	tokenResp, err := w.client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", tokenResp.StatusCode)
	}

	body, err := io.ReadAll(tokenResp.Body)
	if err != nil {
		return "", err
	}

	// Parse {"token":"..."} or {"access_token":"..."}
	token := extractJSONString(body, "token")
	if token == "" {
		token = extractJSONString(body, "access_token")
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	return token, nil
}

// parseUpstreamRef splits "ghcr.io/owner/repo:tag" into (host, repo, tag).
func parseUpstreamRef(ref string) (host, repo, tag string) {
	tag = "latest"

	// Split off tag
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		tag = ref[lastColon+1:]
		ref = ref[:lastColon]
	}

	// Split host from repo
	firstSlash := strings.Index(ref, "/")
	if firstSlash < 0 {
		return "", ref, tag
	}

	host = ref[:firstSlash]
	repo = ref[firstSlash+1:]
	return host, repo, tag
}

// parseWWWAuthenticate parses Bearer realm="...",service="...",scope="..."
func parseWWWAuthenticate(header string) map[string]string {
	params := make(map[string]string)
	header = strings.TrimPrefix(header, "Bearer ")
	header = strings.TrimPrefix(header, "bearer ")

	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		val := strings.TrimSpace(part[eq+1:])
		val = strings.Trim(val, "\"")
		params[key] = val
	}
	return params
}

// extractJSONString extracts a string value from a JSON object by key.
// Simple parser — avoids importing encoding/json for a single field.
func extractJSONString(data []byte, key string) string {
	needle := fmt.Sprintf(`"%s":"`, key)
	idx := strings.Index(string(data), needle)
	if idx < 0 {
		// Try with space after colon
		needle = fmt.Sprintf(`"%s": "`, key)
		idx = strings.Index(string(data), needle)
		if idx < 0 {
			return ""
		}
	}
	start := idx + len(needle)
	end := strings.IndexByte(string(data[start:]), '"')
	if end < 0 {
		return ""
	}
	return string(data[start : start+end])
}

// resolveRegistryHost maps well-known registry hostnames to their actual
// API endpoints. "docker.io" in particular doesn't serve the v2 API —
// it must be rewritten to "registry-1.docker.io".
func resolveRegistryHost(host string) string {
	if host == "docker.io" || host == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return host
}

// isJSONContentType returns true if the content type looks like JSON
// (either application/json or any +json media type).
func isJSONContentType(ct string) bool {
	ct = strings.TrimSpace(ct)
	if strings.HasPrefix(ct, "application/json") {
		return true
	}
	if strings.Contains(ct, "+json") {
		return true
	}
	return false
}

func truncDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "..."
	}
	return d
}
