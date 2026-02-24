package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// BlobStore provides on-disk storage for OCI blobs and manifests.
// Directory structure:
//
//	<root>/
//	  blobs/
//	    sha256/
//	      <hex digest>          — raw blob data
//	  manifests/
//	    <repo>/
//	      <tag or digest>.json  — manifest data
//	      <tag or digest>.type  — content-type metadata
//
// uploadSession tracks an in-progress chunked blob upload.
type uploadSession struct {
	UUID      string
	Repo      string
	TempFile  *os.File
	Size      int64
	CreatedAt time.Time
	mu        sync.Mutex // per-session lock for append operations
}

type BlobStore struct {
	root      string
	uploadsMu sync.RWMutex                // protects the uploads map only
	uploads   map[string]*uploadSession    // uuid -> session
	blobLocks sync.Map                     // digest -> *sync.Mutex (per-blob write lock)
	repoLocks sync.Map                     // repo -> *sync.RWMutex (per-repo manifest lock)
}

// NewBlobStore creates a new on-disk blob store at the given root directory.
func NewBlobStore(root string) (*BlobStore, error) {
	for _, dir := range []string{
		filepath.Join(root, "blobs", "sha256"),
		filepath.Join(root, "manifests"),
		filepath.Join(root, "uploads"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
		}
	}
	return &BlobStore{
		root:    root,
		uploads: make(map[string]*uploadSession),
	}, nil
}

// getBlobLock returns a per-digest mutex for blob writes.
func (s *BlobStore) getBlobLock(digest string) *sync.Mutex {
	mu, _ := s.blobLocks.LoadOrStore(digest, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// getRepoLock returns a per-repo RWMutex for manifest operations.
func (s *BlobStore) getRepoLock(repo string) *sync.RWMutex {
	mu, _ := s.repoLocks.LoadOrStore(repo, &sync.RWMutex{})
	return mu.(*sync.RWMutex)
}

// GetBlob returns the raw data for a blob by its digest (e.g. "sha256:abc123").
// Blob reads are lock-free — filesystem reads are safe for immutable content-addressed data.
func (s *BlobStore) GetBlob(digest string) ([]byte, error) {
	path := s.blobPath(digest)
	return os.ReadFile(path)
}

// HasBlob checks whether a blob exists and returns its size.
func (s *BlobStore) HasBlob(digest string) (exists bool, size int64) {
	info, err := os.Stat(s.blobPath(digest))
	if err != nil {
		return false, 0
	}
	return true, info.Size()
}

// PutBlob stores blob data from a reader, keyed by digest.
// Uses per-digest locking so different blobs can be written concurrently.
func (s *BlobStore) PutBlob(digest string, r io.Reader) error {
	mu := s.getBlobLock(digest)
	mu.Lock()
	defer mu.Unlock()

	path := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, r)
	return err
}

// GetManifest returns the manifest data and content type for a repo/reference.
// Uses per-repo read lock so multiple reads can proceed concurrently.
func (s *BlobStore) GetManifest(repo, ref string) (data []byte, contentType string, err error) {
	mu := s.getRepoLock(repo)
	mu.RLock()
	defer mu.RUnlock()

	dataPath := s.manifestPath(repo, ref)
	typePath := dataPath + ".type"

	data, err = os.ReadFile(dataPath)
	if err != nil {
		return nil, "", err
	}

	typeBytes, err := os.ReadFile(typePath)
	if err != nil {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	} else {
		contentType = string(typeBytes)
	}

	return data, contentType, nil
}

// PutManifest stores a manifest for a repo/reference with its content type.
// Uses per-repo write lock so different repos can be written concurrently.
func (s *BlobStore) PutManifest(repo, ref, contentType string, r io.Reader) error {
	mu := s.getRepoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	dataPath := s.manifestPath(repo, ref)
	if err := os.MkdirAll(filepath.Dir(dataPath), 0755); err != nil {
		return err
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	if err := os.WriteFile(dataPath, data, 0644); err != nil {
		return err
	}

	if contentType != "" {
		if err := os.WriteFile(dataPath+".type", []byte(contentType), 0644); err != nil {
			return err
		}
	}

	return nil
}

// ListRepositories returns all repository names that have stored manifests.
func (s *BlobStore) ListRepositories() []string {
	manifestsDir := filepath.Join(s.root, "manifests")
	var repos []string

	_ = filepath.Walk(manifestsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && !strings.HasSuffix(info.Name(), ".type") {
			rel, err := filepath.Rel(manifestsDir, filepath.Dir(path))
			if err == nil && rel != "." {
				repos = appendUnique(repos, rel)
			}
		}
		return nil
	})

	sort.Strings(repos)
	return repos
}

// ─── Chunked Upload ─────────────────────────────────────────────────────────

// InitiateUpload starts a new chunked blob upload session and returns its UUID.
func (s *BlobStore) InitiateUpload(repo string) (string, error) {
	id := uuid.New().String()
	tmpPath := filepath.Join(s.root, "uploads", id)
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("creating upload temp file: %w", err)
	}

	session := &uploadSession{
		UUID:      id,
		Repo:      repo,
		TempFile:  f,
		CreatedAt: time.Now(),
	}

	s.uploadsMu.Lock()
	s.uploads[id] = session
	s.uploadsMu.Unlock()

	return id, nil
}

// AppendUpload appends data to an in-progress upload session.
// Uses per-session locking so different uploads proceed concurrently.
func (s *BlobStore) AppendUpload(id string, r io.Reader) (int64, error) {
	s.uploadsMu.RLock()
	session, ok := s.uploads[id]
	s.uploadsMu.RUnlock()

	if !ok {
		return 0, fmt.Errorf("upload %s not found", id)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	n, err := io.Copy(session.TempFile, r)
	if err != nil {
		return session.Size, fmt.Errorf("appending to upload: %w", err)
	}
	session.Size += n
	return session.Size, nil
}

// FinalizeUpload completes an upload, verifies the digest, and moves the blob
// into permanent storage. The temp file is cleaned up.
func (s *BlobStore) FinalizeUpload(id, digest string) error {
	s.uploadsMu.Lock()
	session, ok := s.uploads[id]
	if ok {
		delete(s.uploads, id)
	}
	s.uploadsMu.Unlock()

	if !ok {
		return fmt.Errorf("upload %s not found", id)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Close the temp file so we can read it
	session.TempFile.Close()
	tmpPath := filepath.Join(s.root, "uploads", id)

	// Verify digest
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("reading upload data: %w", err)
	}

	if digest != "" {
		computed := computeDigest(data)
		if computed != digest {
			os.Remove(tmpPath)
			return fmt.Errorf("digest mismatch: expected %s, got %s", digest, computed)
		}
	}

	// Move to blob storage
	blobPath := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, blobPath); err != nil {
		// Cross-device fallback: copy + remove
		if err := os.WriteFile(blobPath, data, 0644); err != nil {
			os.Remove(tmpPath)
			return err
		}
		os.Remove(tmpPath)
	}

	return nil
}

// GetUploadSize returns the current size of an in-progress upload.
func (s *BlobStore) GetUploadSize(id string) (int64, bool) {
	s.uploadsMu.RLock()
	session, ok := s.uploads[id]
	s.uploadsMu.RUnlock()

	if !ok {
		return 0, false
	}

	session.mu.Lock()
	size := session.Size
	session.mu.Unlock()

	return size, true
}

// CleanupStaleUploads removes upload sessions older than maxAge.
func (s *BlobStore) CleanupStaleUploads(maxAge time.Duration) {
	s.uploadsMu.Lock()
	defer s.uploadsMu.Unlock()

	now := time.Now()
	for id, session := range s.uploads {
		if now.Sub(session.CreatedAt) > maxAge {
			session.TempFile.Close()
			os.Remove(filepath.Join(s.root, "uploads", id))
			delete(s.uploads, id)
		}
	}
}

// PurgeRepo removes all manifests for a given repo from the store.
func (s *BlobStore) PurgeRepo(repo string) error {
	mu := s.getRepoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	dir := filepath.Join(s.root, "manifests", repo)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(dir)
}

// ValidateManifests checks all stored manifests and removes any that are not
// valid JSON (e.g., HTML pages cached by a broken pull-through). Returns the
// number of corrupted entries removed.
func (s *BlobStore) ValidateManifests() int {
	manifestsDir := filepath.Join(s.root, "manifests")
	removed := 0

	_ = filepath.Walk(manifestsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".type") {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// A valid manifest must be a JSON object
		if len(data) == 0 || (data[0] != '{' && data[0] != '[') {
			os.Remove(path)
			os.Remove(path + ".type")
			removed++
		}
		return nil
	})

	return removed
}

func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func (s *BlobStore) blobPath(digest string) string {
	// digest format: "sha256:hexhexhex..."
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return filepath.Join(s.root, "blobs", "sha256", digest)
	}
	return filepath.Join(s.root, "blobs", parts[0], parts[1])
}

func (s *BlobStore) manifestPath(repo, ref string) string {
	// Sanitize ref for filesystem (colons in digests)
	safe := strings.ReplaceAll(ref, ":", "-")
	return filepath.Join(s.root, "manifests", repo, safe+".json")
}

func appendUnique(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}
