package gitbackup

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// RegisterRoutes registers the git backup API endpoints.
func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/gitbackup/status", m.handleStatus)
	mux.HandleFunc("/api/v1/gitbackup/trigger", m.handleTrigger)
	mux.HandleFunc("/api/v1/gitbackup/diag", m.handleDiag)
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m.Status())
}

func (m *Manager) handleTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.TriggerNow()
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "triggered"})
}

func (m *Manager) handleDiag(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result := map[string]interface{}{
		"storeNil": m.store == nil,
	}

	if m.store == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
		return
	}

	buckets := make(map[string]interface{})
	for _, be := range bucketExports {
		if be.skip {
			buckets[be.bucketName] = "skipped"
			continue
		}
		bucket := m.store.BucketByName(be.bucketName)
		if bucket == nil {
			buckets[be.bucketName] = "nil"
			continue
		}
		keys, err := bucket.Keys(ctx, "")
		if err != nil {
			buckets[be.bucketName] = map[string]interface{}{"error": err.Error()}
			continue
		}
		buckets[be.bucketName] = map[string]interface{}{"keys": len(keys)}
	}
	result["buckets"] = buckets

	files, manifest, err := exportAll(ctx, m.store)
	result["exportFiles"] = len(files)
	result["exportManifest"] = len(manifest)
	if err != nil {
		result["exportError"] = err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
