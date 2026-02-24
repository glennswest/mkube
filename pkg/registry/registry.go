package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

// PushEvent is emitted when a manifest is pushed to the registry.
type PushEvent struct {
	Repo      string
	Reference string
	Digest    string
	Time      time.Time
}

// Registry provides an OCI Distribution Spec v2 compatible registry with
// on-disk blob/manifest storage, Docker push (chunked upload) support,
// and optional pull-through caching from upstream registries.
type Registry struct {
	cfg        config.RegistryConfig
	log        *zap.SugaredLogger
	server     *http.Server
	store      *BlobStore
	client     *http.Client
	PushEvents chan PushEvent
	SyncEvents chan PushEvent // external manifest PUTs only (for upstream sync, avoids loop)
}

// Start launches the embedded registry server.
func Start(ctx context.Context, cfg config.RegistryConfig, log *zap.SugaredLogger) (*Registry, error) {
	store, err := NewBlobStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("initializing blob store: %w", err)
	}

	r := &Registry{
		cfg:   cfg,
		log:   log,
		store: store,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			},
		},
		PushEvents: make(chan PushEvent, 64),
		SyncEvents: make(chan PushEvent, 64),
	}

	mux := http.NewServeMux()

	// OCI Distribution Spec v2 endpoints
	mux.HandleFunc("/v2/", r.handleV2)
	mux.HandleFunc("/v2/_catalog", r.handleCatalog)
	mux.HandleFunc("POST /v2/_pull", r.handlePull)

	r.server = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	// Try loading TLS cert from disk (installed by mkube-installer).
	// If certs exist, serve HTTPS. Otherwise fall back to HTTP.
	certFile := cfg.TLSCertFile
	keyFile := cfg.TLSKeyFile
	if certFile == "" {
		certFile = "/etc/registry/tls.crt"
	}
	if keyFile == "" {
		keyFile = "/etc/registry/tls.key"
	}

	if _, err := os.Stat(certFile); err == nil {
		tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS cert: %w", err)
		}
		r.server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		}
		go func() {
			log.Infow("registry listening (HTTPS)", "addr", cfg.ListenAddr)
			if err := r.server.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
				log.Errorw("registry server error", "error", err)
			}
		}()
	} else {
		log.Warn("TLS cert not found, falling back to HTTP (insecure)")
		go func() {
			log.Infow("registry listening (HTTP)", "addr", cfg.ListenAddr)
			if err := r.server.ListenAndServe(); err != http.ErrServerClosed {
				log.Errorw("registry server error", "error", err)
			}
		}()
	}

	// Periodic cleanup of stale upload sessions
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.store.CleanupStaleUploads(1 * time.Hour)
			}
		}
	}()

	return r, nil
}

// Store returns the underlying blob store (used by the image watcher).
func (r *Registry) Store() *BlobStore {
	return r.store
}

// Shutdown gracefully stops the registry server.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.log.Info("shutting down registry")
	return r.server.Shutdown(ctx)
}

// handleV2 routes OCI distribution spec requests.
func (r *Registry) handleV2(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Base endpoint: GET /v2/
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	trimmed := strings.TrimPrefix(path, "/v2/")

	// Handle chunked blob uploads: /v2/<name>/blobs/uploads/ or /v2/<name>/blobs/uploads/<uuid>
	if idx := strings.Index(trimmed, "/blobs/uploads"); idx >= 0 {
		repoName := trimmed[:idx]
		rest := trimmed[idx+len("/blobs/uploads"):]
		rest = strings.TrimPrefix(rest, "/")
		r.handleBlobUpload(w, req, repoName, rest)
		return
	}

	// Parse: /v2/<name>/manifests/<reference> or /v2/<name>/blobs/<digest>
	var repoName, resourceType, reference string
	if idx := strings.LastIndex(trimmed, "/manifests/"); idx >= 0 {
		repoName = trimmed[:idx]
		resourceType = "manifests"
		reference = trimmed[idx+len("/manifests/"):]
	} else if idx := strings.LastIndex(trimmed, "/blobs/"); idx >= 0 {
		repoName = trimmed[:idx]
		resourceType = "blobs"
		reference = trimmed[idx+len("/blobs/"):]
	}

	if repoName == "" || reference == "" {
		http.NotFound(w, req)
		return
	}

	switch resourceType {
	case "manifests":
		r.handleManifest(w, req, repoName, reference)
	case "blobs":
		r.handleBlob(w, req, repoName, reference)
	default:
		http.NotFound(w, req)
	}
}

// handleManifest serves or stores manifests.
func (r *Registry) handleManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	switch req.Method {
	case http.MethodGet:
		data, contentType, err := r.store.GetManifest(repo, ref)
		if err == nil {
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = w.Write(data)
			return
		}
		// Pull-through: fetch from upstream, cache locally
		if r.cfg.PullThrough {
			r.pullThroughManifest(w, req, repo, ref)
			return
		}
		http.NotFound(w, req)

	case http.MethodHead:
		data, contentType, err := r.store.GetManifest(repo, ref)
		if err == nil {
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, req)

	case http.MethodPut:
		contentType := req.Header.Get("Content-Type")
		if err := r.store.PutManifest(repo, ref, contentType, req.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Docker-Content-Digest", ref)
		w.WriteHeader(http.StatusCreated)

		// Emit push event for CI/CD and upstream sync
		evt := PushEvent{
			Repo:      repo,
			Reference: ref,
			Digest:    req.Header.Get("Docker-Content-Digest"),
			Time:      time.Now(),
		}
		select {
		case r.PushEvents <- evt:
		default:
			r.log.Warnw("push event channel full, dropping event", "repo", repo, "ref", ref)
		}
		select {
		case r.SyncEvents <- evt:
		default:
			r.log.Warnw("sync event channel full, dropping event", "repo", repo, "ref", ref)
		}

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleBlob serves or stores blobs.
func (r *Registry) handleBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	switch req.Method {
	case http.MethodGet:
		data, err := r.store.GetBlob(digest)
		if err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = w.Write(data)
			return
		}
		// Pull-through: fetch from upstream
		if r.cfg.PullThrough {
			r.pullThroughBlob(w, req, repo, digest)
			return
		}
		http.NotFound(w, req)

	case http.MethodHead:
		exists, size := r.store.HasBlob(digest)
		if exists {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, req)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleBlobUpload implements the Docker chunked blob upload protocol:
//   - POST   /v2/<name>/blobs/uploads/       → initiate upload, return UUID
//   - PATCH  /v2/<name>/blobs/uploads/<uuid>  → append chunk data
//   - PUT    /v2/<name>/blobs/uploads/<uuid>?digest=... → finalize upload
//   - GET    /v2/<name>/blobs/uploads/<uuid>  → check upload status
func (r *Registry) handleBlobUpload(w http.ResponseWriter, req *http.Request, repo, uploadUUID string) {
	switch req.Method {
	case http.MethodPost:
		// Initiate a new upload
		id, err := r.store.InitiateUpload(repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, id))
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)
		r.log.Debugw("blob upload initiated", "repo", repo, "uuid", id)

	case http.MethodPatch:
		// Append chunk
		if uploadUUID == "" {
			http.Error(w, "missing upload UUID", http.StatusBadRequest)
			return
		}
		size, err := r.store.AppendUpload(uploadUUID, req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, uploadUUID))
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		w.Header().Set("Range", fmt.Sprintf("0-%d", size-1))
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPut:
		// Finalize upload
		if uploadUUID == "" {
			http.Error(w, "missing upload UUID", http.StatusBadRequest)
			return
		}
		digest := req.URL.Query().Get("digest")
		if digest == "" {
			http.Error(w, "missing digest parameter", http.StatusBadRequest)
			return
		}

		// If the PUT includes a body (monolithic upload or final chunk), append it first
		if req.ContentLength > 0 {
			if _, err := r.store.AppendUpload(uploadUUID, req.Body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		if err := r.store.FinalizeUpload(uploadUUID, digest); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, digest))
		w.WriteHeader(http.StatusCreated)
		r.log.Debugw("blob upload finalized", "repo", repo, "digest", digest)

	case http.MethodGet:
		// Check upload status
		if uploadUUID == "" {
			http.Error(w, "missing upload UUID", http.StatusBadRequest)
			return
		}
		size, ok := r.store.GetUploadSize(uploadUUID)
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		w.Header().Set("Range", fmt.Sprintf("0-%d", size))
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleCatalog returns the list of locally stored repositories.
func (r *Registry) handleCatalog(w http.ResponseWriter, req *http.Request) {
	repos := r.store.ListRepositories()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{
		"repositories": repos,
	})
}

// pullRequest is the JSON body for POST /v2/_pull.
type pullRequest struct {
	Image string `json:"image"` // e.g. "ghcr.io/glennswest/nats:edge"
}

// handlePull triggers a pull-through fetch from an upstream registry.
// POST /v2/_pull  {"image": "ghcr.io/user/repo:tag"}
// This resolves the image reference, fetches the manifest and all blobs,
// and caches them locally so subsequent OCI pulls succeed immediately.
func (r *Registry) handlePull(w http.ResponseWriter, req *http.Request) {
	var pr pullRequest
	if err := json.NewDecoder(req.Body).Decode(&pr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if pr.Image == "" {
		http.Error(w, `"image" is required`, http.StatusBadRequest)
		return
	}

	// Parse image ref: "ghcr.io/user/repo:tag" → host="ghcr.io", repo="user/repo", ref="tag"
	host, repo, ref := parseImageRef(pr.Image)
	if repo == "" || ref == "" {
		http.Error(w, fmt.Sprintf("cannot parse image reference %q", pr.Image), http.StatusBadRequest)
		return
	}

	r.log.Infow("pull request received", "image", pr.Image, "host", host, "repo", repo, "ref", ref)

	// Fetch manifest from upstream
	manifest, contentType, err := r.fetchUpstreamManifest(req.Context(), host, repo, ref)
	if err != nil {
		r.log.Errorw("pull failed: manifest fetch", "image", pr.Image, "error", err)
		http.Error(w, fmt.Sprintf("manifest fetch failed: %v", err), http.StatusBadGateway)
		return
	}

	// Determine local repo name (strip host prefix)
	localRepo := repo

	// Cache manifest
	if err := r.store.PutManifest(localRepo, ref, contentType, io.NopCloser(strings.NewReader(string(manifest)))); err != nil {
		http.Error(w, fmt.Sprintf("caching manifest: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse manifest to find blob digests
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		// Manifest cached but blobs not fetched — still useful
		r.log.Warnw("pull: manifest cached but cannot parse for blobs", "error", err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"partial","manifest":true,"blobs":0}`)
		return
	}

	// Collect all digests to fetch
	var digests []string
	if m.Config.Digest != "" {
		digests = append(digests, m.Config.Digest)
	}
	for _, l := range m.Layers {
		digests = append(digests, l.Digest)
	}

	// Fetch blobs
	fetched := 0
	for _, digest := range digests {
		if exists, _ := r.store.HasBlob(digest); exists {
			fetched++
			continue
		}
		if err := r.fetchUpstreamBlob(req.Context(), host, repo, digest); err != nil {
			r.log.Warnw("pull: blob fetch failed", "digest", digest, "error", err)
			continue
		}
		fetched++
	}

	r.log.Infow("pull complete", "image", pr.Image, "localRepo", localRepo, "blobs", fetched, "total", len(digests))

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"ok","manifest":true,"blobs":%d,"total":%d}`, fetched, len(digests))
}

// parseImageRef splits "ghcr.io/user/repo:tag" into host, repo, tag.
// For "repo:tag" without host, returns empty host.
func parseImageRef(ref string) (host, repo, tag string) {
	tag = "latest"
	if idx := strings.LastIndex(ref, ":"); idx > 0 {
		// Ensure it's a tag separator, not part of a port
		afterColon := ref[idx+1:]
		if !strings.Contains(afterColon, "/") {
			tag = afterColon
			ref = ref[:idx]
		}
	}

	// Split host from repo: first component with a dot or colon is the host
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		host = parts[0]
		repo = parts[1]
	} else {
		repo = ref
	}
	return
}

// fetchUpstreamManifest fetches a manifest from an upstream registry host.
func (r *Registry) fetchUpstreamManifest(ctx context.Context, host, repo, ref string) ([]byte, string, error) {
	if host == "" {
		// Try configured upstreams
		for _, upstream := range r.cfg.UpstreamRegistries {
			data, ct, err := r.fetchManifestFromHost(ctx, resolveRegistryHost(upstream), repo, ref)
			if err == nil {
				return data, ct, nil
			}
		}
		return nil, "", fmt.Errorf("manifest not found on any upstream")
	}
	return r.fetchManifestFromHost(ctx, host, repo, ref)
}

func (r *Registry) fetchManifestFromHost(ctx context.Context, host, repo, ref string) ([]byte, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, ref)
	mReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	mReq.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")

	resp, err := r.client.Do(mReq)
	if err != nil {
		return nil, "", err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		token, err := r.getUpstreamToken(ctx, resp, host, repo)
		if err != nil {
			return nil, "", fmt.Errorf("auth failed: %w", err)
		}
		mReq, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		mReq.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")
		mReq.Header.Set("Authorization", "Bearer "+token)
		resp, err = r.client.Do(mReq)
		if err != nil {
			return nil, "", err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// fetchUpstreamBlob fetches a single blob from upstream and caches it.
func (r *Registry) fetchUpstreamBlob(ctx context.Context, host, repo, digest string) error {
	fetchFromHost := func(h string) error {
		url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", h, repo, digest)
		bReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := r.client.Do(bReq)
		if err != nil {
			return err
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			token, err := r.getUpstreamToken(ctx, resp, h, repo)
			if err != nil {
				return fmt.Errorf("auth failed: %w", err)
			}
			bReq, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			bReq.Header.Set("Authorization", "Bearer "+token)
			resp, err = r.client.Do(bReq)
			if err != nil {
				return err
			}
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			return fmt.Errorf("upstream returned %d", resp.StatusCode)
		}

		return r.store.PutBlob(digest, resp.Body)
	}

	if host != "" {
		return fetchFromHost(host)
	}
	for _, upstream := range r.cfg.UpstreamRegistries {
		if err := fetchFromHost(resolveRegistryHost(upstream)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("blob %s not found on any upstream", digest)
}

// resolveUpstreamRepo looks up the watchImages config to find the correct
// upstream host/repo/ref for a local repo name. Returns empty strings if
// no mapping is found.
func (r *Registry) resolveUpstreamRepo(localRepo string) (host, repo, ref string) {
	for _, wi := range r.cfg.WatchImages {
		if wi.LocalRepo == localRepo {
			return parseUpstreamRef(wi.Upstream)
		}
	}
	return "", "", ""
}

// pullThroughManifest fetches a manifest from upstream, caches it, and serves it.
func (r *Registry) pullThroughManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	// Try watchImages mapping first — this has the correct upstream path
	// (e.g. localRepo "microdns" → "ghcr.io/glennswest/microdns")
	if upHost, upRepo, _ := r.resolveUpstreamRepo(repo); upHost != "" && upRepo != "" {
		if data, contentType, digest, err := r.tryPullThroughManifest(req, upHost, upRepo, ref); err == nil {
			if err := r.store.PutManifest(repo, ref, contentType, io.NopCloser(strings.NewReader(string(data)))); err != nil {
				r.log.Warnw("failed to cache manifest", "repo", repo, "ref", ref, "error", err)
				http.Error(w, "cache error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", contentType)
			if digest != "" {
				w.Header().Set("Docker-Content-Digest", digest)
			}
			_, _ = w.Write(data)
			r.log.Infow("pull-through manifest via watchImages", "repo", repo, "ref", ref, "upstream", upHost+"/"+upRepo)
			return
		} else {
			r.log.Debugw("pull-through via watchImages failed, trying upstreamRegistries",
				"repo", repo, "upstream", upHost+"/"+upRepo, "error", err)
		}
	}

	for _, upstream := range r.cfg.UpstreamRegistries {
		host := resolveRegistryHost(upstream)
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, ref)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}
		proxyReq.Header.Set("Accept", req.Header.Get("Accept"))

		resp, err := r.client.Do(proxyReq)
		if err != nil {
			continue
		}

		// Handle 401 with token exchange
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			token, err := r.getUpstreamToken(req.Context(), resp, upstream, repo)
			if err != nil {
				r.log.Debugw("pull-through auth failed", "upstream", upstream, "error", err)
				continue
			}
			proxyReq, _ = http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
			proxyReq.Header.Set("Accept", req.Header.Get("Accept"))
			proxyReq.Header.Set("Authorization", "Bearer "+token)
			resp, err = r.client.Do(proxyReq)
			if err != nil {
				continue
			}
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		digest := resp.Header.Get("Docker-Content-Digest")

		// Validate: reject HTML responses (e.g., Docker Hub website)
		if !isJSONContentType(contentType) {
			resp.Body.Close()
			r.log.Debugw("pull-through manifest rejected: not JSON",
				"upstream", upstream, "contentType", contentType)
			continue
		}

		// Read body and validate it's actual JSON
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}

		if len(data) > 0 && data[0] != '{' && data[0] != '[' {
			r.log.Debugw("pull-through manifest rejected: body not JSON",
				"upstream", upstream, "prefix", string(data[:min(50, len(data))]))
			continue
		}

		// Cache it locally
		if err := r.store.PutManifest(repo, ref, contentType, io.NopCloser(strings.NewReader(string(data)))); err != nil {
			r.log.Warnw("failed to cache manifest", "repo", repo, "ref", ref, "error", err)
			http.Error(w, "cache error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", contentType)
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		_, _ = w.Write(data)
		r.log.Debugw("pull-through manifest cached", "repo", repo, "ref", ref, "upstream", upstream)
		return
	}

	http.Error(w, "manifest not found on any upstream", http.StatusNotFound)
}

// tryPullThroughManifest attempts to fetch a manifest from a specific upstream host/repo.
// Returns the manifest data, content type, digest, and any error.
func (r *Registry) tryPullThroughManifest(req *http.Request, host, repo, ref string) ([]byte, string, string, error) {
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", resolveRegistryHost(host), repo, ref)
	proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	proxyReq.Header.Set("Accept", req.Header.Get("Accept"))

	resp, err := r.client.Do(proxyReq)
	if err != nil {
		return nil, "", "", err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		token, err := r.getUpstreamToken(req.Context(), resp, host, repo)
		if err != nil {
			return nil, "", "", fmt.Errorf("auth failed: %w", err)
		}
		proxyReq, _ = http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		proxyReq.Header.Set("Accept", req.Header.Get("Accept"))
		proxyReq.Header.Set("Authorization", "Bearer "+token)
		resp, err = r.client.Do(proxyReq)
		if err != nil {
			return nil, "", "", err
		}
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !isJSONContentType(contentType) {
		return nil, "", "", fmt.Errorf("not JSON content-type: %s", contentType)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	if len(data) > 0 && data[0] != '{' && data[0] != '[' {
		return nil, "", "", fmt.Errorf("body not JSON")
	}

	return data, contentType, resp.Header.Get("Docker-Content-Digest"), nil
}

// tryPullThroughBlob attempts to fetch a blob from a specific upstream host/repo,
// caches it locally, and returns any error.
func (r *Registry) tryPullThroughBlob(req *http.Request, host, repo, digest string) error {
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", resolveRegistryHost(host), repo, digest)
	proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return err
	}

	resp, err := r.client.Do(proxyReq)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		token, err := r.getUpstreamToken(req.Context(), resp, host, repo)
		if err != nil {
			return fmt.Errorf("auth failed: %w", err)
		}
		proxyReq, _ = http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		proxyReq.Header.Set("Authorization", "Bearer "+token)
		resp, err = r.client.Do(proxyReq)
		if err != nil {
			return err
		}
	}

	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		return fmt.Errorf("HTML response")
	}

	return r.store.PutBlob(digest, resp.Body)
}

// pullThroughBlob fetches a blob from upstream, caches it, and serves it.
func (r *Registry) pullThroughBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	// Try watchImages mapping first for correct upstream path
	if upHost, upRepo, _ := r.resolveUpstreamRepo(repo); upHost != "" && upRepo != "" {
		if err := r.tryPullThroughBlob(req, upHost, upRepo, digest); err == nil {
			data, err := r.store.GetBlob(digest)
			if err != nil {
				http.Error(w, "cache read error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = w.Write(data)
			r.log.Debugw("pull-through blob via watchImages", "digest", digest, "upstream", upHost+"/"+upRepo)
			return
		} else {
			r.log.Debugw("pull-through blob via watchImages failed, trying upstreamRegistries",
				"repo", repo, "digest", digest, "error", err)
		}
	}

	for _, upstream := range r.cfg.UpstreamRegistries {
		host := resolveRegistryHost(upstream)
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", host, repo, digest)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}

		resp, err := r.client.Do(proxyReq)
		if err != nil {
			continue
		}

		// Handle 401 with token exchange
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			token, err := r.getUpstreamToken(req.Context(), resp, upstream, repo)
			if err != nil {
				r.log.Debugw("pull-through blob auth failed", "upstream", upstream, "error", err)
				continue
			}
			proxyReq, _ = http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
			proxyReq.Header.Set("Authorization", "Bearer "+token)
			resp, err = r.client.Do(proxyReq)
			if err != nil {
				continue
			}
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			continue
		}

		// Validate: reject HTML responses
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") {
			resp.Body.Close()
			r.log.Debugw("pull-through blob rejected: HTML response",
				"upstream", upstream, "digest", digest)
			continue
		}

		// Cache blob locally
		if err := r.store.PutBlob(digest, resp.Body); err != nil {
			resp.Body.Close()
			r.log.Warnw("failed to cache blob", "digest", digest, "error", err)
			http.Error(w, "cache error", http.StatusInternalServerError)
			return
		}
		resp.Body.Close()

		// Serve from cache
		data, err := r.store.GetBlob(digest)
		if err != nil {
			http.Error(w, "cache read error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = w.Write(data)
		r.log.Debugw("pull-through blob cached", "digest", digest, "upstream", upstream)
		return
	}

	http.Error(w, "blob not found on any upstream", http.StatusNotFound)
}

// getUpstreamToken handles the OCI/Docker token exchange for pull-through.
// Reuses parseWWWAuthenticate and extractJSONString from the same package.
func (r *Registry) getUpstreamToken(ctx context.Context, resp *http.Response, host, repo string) (string, error) {
	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		return "", fmt.Errorf("no WWW-Authenticate header")
	}

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

	tokenResp, err := r.client.Do(tokenReq)
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

	token := extractJSONString(body, "token")
	if token == "" {
		token = extractJSONString(body, "access_token")
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	return token, nil
}
