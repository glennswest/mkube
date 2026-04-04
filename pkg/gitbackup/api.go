package gitbackup

import (
	"encoding/json"
	"net/http"
)

// RegisterRoutes registers the git backup API endpoints.
func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/gitbackup/status", m.handleStatus)
	mux.HandleFunc("/api/v1/gitbackup/trigger", m.handleTrigger)
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
