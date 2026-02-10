package registry

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	tmpDir := t.TempDir()
	store, err := NewBlobStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := zap.NewDevelopment()
	return &Registry{
		cfg: config.RegistryConfig{
			ListenAddr: ":0",
			StorePath:  tmpDir,
		},
		log:   log.Sugar(),
		store: store,
	}
}

func TestV2Base(t *testing.T) {
	r := newTestRegistry(t)
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	w := httptest.NewRecorder()

	r.handleV2(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if v := w.Header().Get("Docker-Distribution-API-Version"); v != "registry/2.0" {
		t.Errorf("expected registry/2.0, got %q", v)
	}
}

func TestCatalogEmpty(t *testing.T) {
	r := newTestRegistry(t)
	req := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	w := httptest.NewRecorder()

	r.handleCatalog(w, req)

	var result map[string][]string
	json.NewDecoder(w.Body).Decode(&result)
	if len(result["repositories"]) != 0 {
		t.Errorf("expected empty repositories, got %v", result["repositories"])
	}
}

func TestManifestPutAndGet(t *testing.T) {
	r := newTestRegistry(t)
	manifestData := `{"schemaVersion": 2, "mediaType": "application/vnd.docker.distribution.manifest.v2+json"}`

	// PUT manifest
	putReq := httptest.NewRequest(http.MethodPut, "/v2/myrepo/manifests/latest", bytes.NewBufferString(manifestData))
	putReq.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	putW := httptest.NewRecorder()
	r.handleV2(putW, putReq)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT expected 201, got %d: %s", putW.Code, putW.Body.String())
	}

	// GET manifest
	getReq := httptest.NewRequest(http.MethodGet, "/v2/myrepo/manifests/latest", nil)
	getW := httptest.NewRecorder()
	r.handleV2(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", getW.Code)
	}
	if getW.Body.String() != manifestData {
		t.Errorf("manifest data mismatch: got %q", getW.Body.String())
	}
}

func TestManifestHead(t *testing.T) {
	r := newTestRegistry(t)

	// Store a manifest first
	r.store.PutManifest("testrepo", "v1", "application/vnd.docker.distribution.manifest.v2+json",
		bytes.NewBufferString("test-manifest-data"))

	req := httptest.NewRequest(http.MethodHead, "/v2/testrepo/manifests/v1", nil)
	w := httptest.NewRecorder()
	r.handleV2(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Errorf("unexpected content type: %s", w.Header().Get("Content-Type"))
	}
}

func TestBlobHeadAndGet(t *testing.T) {
	r := newTestRegistry(t)
	blobData := []byte("hello world blob data")
	digest := "sha256:abc123"

	r.store.PutBlob(digest, bytes.NewReader(blobData))

	// HEAD
	headReq := httptest.NewRequest(http.MethodHead, "/v2/myrepo/blobs/"+digest, nil)
	headW := httptest.NewRecorder()
	r.handleV2(headW, headReq)

	if headW.Code != http.StatusOK {
		t.Errorf("HEAD expected 200, got %d", headW.Code)
	}

	// GET
	getReq := httptest.NewRequest(http.MethodGet, "/v2/myrepo/blobs/"+digest, nil)
	getW := httptest.NewRecorder()
	r.handleV2(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("GET expected 200, got %d", getW.Code)
	}
	body, _ := io.ReadAll(getW.Body)
	if string(body) != string(blobData) {
		t.Errorf("blob data mismatch")
	}
}

func TestBlobNotFound(t *testing.T) {
	r := newTestRegistry(t)

	req := httptest.NewRequest(http.MethodGet, "/v2/myrepo/blobs/sha256:nonexistent", nil)
	w := httptest.NewRecorder()
	r.handleV2(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCatalogWithManifests(t *testing.T) {
	r := newTestRegistry(t)

	r.store.PutManifest("library/nginx", "latest", "application/json",
		bytes.NewBufferString("{}"))
	r.store.PutManifest("myapp/backend", "v1", "application/json",
		bytes.NewBufferString("{}"))

	req := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	w := httptest.NewRecorder()
	r.handleCatalog(w, req)

	var result map[string][]string
	json.NewDecoder(w.Body).Decode(&result)

	repos := result["repositories"]
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d: %v", len(repos), repos)
	}
}

func TestManifestMethodNotAllowed(t *testing.T) {
	r := newTestRegistry(t)

	req := httptest.NewRequest(http.MethodDelete, "/v2/myrepo/manifests/latest", nil)
	w := httptest.NewRecorder()
	r.handleV2(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestNestedRepoPath(t *testing.T) {
	r := newTestRegistry(t)

	r.store.PutManifest("library/nginx", "latest", "application/json",
		bytes.NewBufferString(`{"test": true}`))

	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
	w := httptest.NewRecorder()
	r.handleV2(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBlobStoreOperations(t *testing.T) {
	store, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// PutBlob + GetBlob
	data := []byte("test blob content")
	if err := store.PutBlob("sha256:test123", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetBlob("sha256:test123")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("blob mismatch")
	}

	// HasBlob
	exists, size := store.HasBlob("sha256:test123")
	if !exists {
		t.Error("expected blob to exist")
	}
	if size != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), size)
	}

	// Missing blob
	exists, _ = store.HasBlob("sha256:missing")
	if exists {
		t.Error("expected blob to not exist")
	}
}

func TestChunkedBlobUpload(t *testing.T) {
	r := newTestRegistry(t)

	// 1. POST to initiate upload
	postReq := httptest.NewRequest(http.MethodPost, "/v2/myrepo/blobs/uploads/", nil)
	postW := httptest.NewRecorder()
	r.handleV2(postW, postReq)

	if postW.Code != http.StatusAccepted {
		t.Fatalf("POST expected 202, got %d: %s", postW.Code, postW.Body.String())
	}

	uploadUUID := postW.Header().Get("Docker-Upload-UUID")
	if uploadUUID == "" {
		t.Fatal("expected Docker-Upload-UUID header")
	}
	location := postW.Header().Get("Location")
	if location == "" {
		t.Fatal("expected Location header")
	}

	// 2. PATCH to send chunk data
	chunkData := []byte("hello world chunk data")
	patchReq := httptest.NewRequest(http.MethodPatch, location, bytes.NewReader(chunkData))
	patchW := httptest.NewRecorder()
	r.handleV2(patchW, patchReq)

	if patchW.Code != http.StatusAccepted {
		t.Fatalf("PATCH expected 202, got %d: %s", patchW.Code, patchW.Body.String())
	}

	// 3. PUT to finalize with digest
	digest := computeTestDigest(chunkData)
	putURL := location + "?digest=" + digest
	putReq := httptest.NewRequest(http.MethodPut, putURL, nil)
	putReq.ContentLength = 0
	putW := httptest.NewRecorder()
	r.handleV2(putW, putReq)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT expected 201, got %d: %s", putW.Code, putW.Body.String())
	}

	// 4. GET the blob by digest
	getReq := httptest.NewRequest(http.MethodGet, "/v2/myrepo/blobs/"+digest, nil)
	getW := httptest.NewRecorder()
	r.handleV2(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", getW.Code)
	}
	body, _ := io.ReadAll(getW.Body)
	if string(body) != string(chunkData) {
		t.Errorf("blob data mismatch: got %q", string(body))
	}
}

func TestMonolithicBlobUpload(t *testing.T) {
	r := newTestRegistry(t)

	// POST to initiate
	postReq := httptest.NewRequest(http.MethodPost, "/v2/test/blobs/uploads/", nil)
	postW := httptest.NewRecorder()
	r.handleV2(postW, postReq)

	location := postW.Header().Get("Location")

	// PUT with body + digest (monolithic upload)
	data := []byte("monolithic blob data")
	digest := computeTestDigest(data)
	putReq := httptest.NewRequest(http.MethodPut, location+"?digest="+digest, bytes.NewReader(data))
	putReq.ContentLength = int64(len(data))
	putW := httptest.NewRecorder()
	r.handleV2(putW, putReq)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT expected 201, got %d: %s", putW.Code, putW.Body.String())
	}

	// Verify blob exists
	exists, size := r.store.HasBlob(digest)
	if !exists {
		t.Fatal("expected blob to exist after monolithic upload")
	}
	if size != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), size)
	}
}

func computeTestDigest(data []byte) string {
	return computeDigest(data)
}
