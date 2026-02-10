package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
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
	PushEvents chan PushEvent
}

// Start launches the embedded registry server.
func Start(ctx context.Context, cfg config.RegistryConfig, log *zap.SugaredLogger) (*Registry, error) {
	store, err := NewBlobStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("initializing blob store: %w", err)
	}

	r := &Registry{
		cfg:        cfg,
		log:        log,
		store:      store,
		PushEvents: make(chan PushEvent, 64),
	}

	mux := http.NewServeMux()

	// OCI Distribution Spec v2 endpoints
	mux.HandleFunc("/v2/", r.handleV2)
	mux.HandleFunc("/v2/_catalog", r.handleCatalog)

	r.server = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Infow("registry listening", "addr", cfg.ListenAddr)
		if err := r.server.ListenAndServe(); err != http.ErrServerClosed {
			log.Errorw("registry server error", "error", err)
		}
	}()

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
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Docker-Content-Digest", ref)
			w.Write(data)
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
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", ref)
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

		// Emit push event for CI/CD
		select {
		case r.PushEvents <- PushEvent{
			Repo:      repo,
			Reference: ref,
			Digest:    req.Header.Get("Docker-Content-Digest"),
			Time:      time.Now(),
		}:
		default:
			r.log.Warnw("push event channel full, dropping event", "repo", repo, "ref", ref)
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
			w.Write(data)
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
	json.NewEncoder(w).Encode(map[string][]string{
		"repositories": repos,
	})
}

// pullThroughManifest fetches a manifest from upstream, caches it, and serves it.
func (r *Registry) pullThroughManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream, repo, ref)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}
		proxyReq.Header.Set("Accept", req.Header.Get("Accept"))

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil || resp.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		digest := resp.Header.Get("Docker-Content-Digest")

		// Cache it locally
		if err := r.store.PutManifest(repo, ref, contentType, resp.Body); err != nil {
			resp.Body.Close()
			r.log.Warnw("failed to cache manifest", "repo", repo, "ref", ref, "error", err)
			http.Error(w, "cache error", http.StatusInternalServerError)
			return
		}
		resp.Body.Close()

		// Serve from cache
		data, ct, err := r.store.GetManifest(repo, ref)
		if err != nil {
			http.Error(w, "cache read error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", ct)
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.Write(data)
		r.log.Debugw("pull-through manifest cached", "repo", repo, "ref", ref, "upstream", upstream)
		return
	}

	http.Error(w, "manifest not found on any upstream", http.StatusNotFound)
}

// pullThroughBlob fetches a blob from upstream, caches it, and serves it.
func (r *Registry) pullThroughBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", upstream, repo, digest)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil || resp.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close()
			}
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
		w.Write(data)
		r.log.Debugw("pull-through blob cached", "digest", digest, "upstream", upstream)
		return
	}

	http.Error(w, "blob not found on any upstream", http.StatusNotFound)
}

// proxyToUpstream attempts to proxy the request to configured upstream registries.
func (r *Registry) proxyToUpstream(w http.ResponseWriter, req *http.Request) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		target, err := url.Parse(fmt.Sprintf("https://%s", upstream))
		if err != nil {
			continue
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			// Silently try next upstream
		}

		r.log.Debugw("proxying to upstream", "upstream", upstream, "path", req.URL.Path)
		proxy.ServeHTTP(w, req)
		return
	}

	http.Error(w, "no upstream registry available", http.StatusBadGateway)
}
