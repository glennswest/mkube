package dzo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RunAPI starts the DZO REST API server.
func (o *Operator) RunAPI(ctx context.Context, listenAddr string) {
	mux := http.NewServeMux()

	// Zones
	mux.HandleFunc("GET /api/v1/zones", o.handleListZones)
	mux.HandleFunc("GET /api/v1/zones/{name}", o.handleGetZone)
	mux.HandleFunc("POST /api/v1/zones", o.handleCreateZone)
	mux.HandleFunc("DELETE /api/v1/zones/{name}", o.handleDeleteZone)

	// Namespaces
	mux.HandleFunc("GET /api/v1/namespaces", o.handleListNamespaces)
	mux.HandleFunc("GET /api/v1/namespaces/{name}", o.handleGetNamespace)
	mux.HandleFunc("POST /api/v1/namespaces", o.handleCreateNamespace)
	mux.HandleFunc("DELETE /api/v1/namespaces/{name}", o.handleDeleteNamespace)

	// Instances (read-only)
	mux.HandleFunc("GET /api/v1/instances", o.handleListInstances)
	mux.HandleFunc("GET /api/v1/instances/{name}", o.handleGetInstance)

	// Health
	mux.HandleFunc("GET /healthz", o.handleHealthz)

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	o.log.Infow("DZO API listening", "addr", listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		o.log.Errorw("DZO API error", "error", err)
	}
}

// ─── Zone Handlers ──────────────────────────────────────────────────────────

func (o *Operator) handleListZones(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.ListZones())
}

func (o *Operator) handleGetZone(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Support dotted zone names passed as path suffix
	name = decodeDottedName(name, r.URL.Path, "/api/v1/zones/")

	zone, err := o.GetZone(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, zone)
}

func (o *Operator) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	var req CreateZoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Network == "" {
		http.Error(w, `"name" and "network" are required`, http.StatusBadRequest)
		return
	}

	zone, err := o.CreateZone(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusCreated, zone)
}

func (o *Operator) handleDeleteZone(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	name = decodeDottedName(name, r.URL.Path, "/api/v1/zones/")

	if err := o.DeleteZone(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "in use") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Namespace Handlers ─────────────────────────────────────────────────────

func (o *Operator) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.ListNamespaces())
}

func (o *Operator) handleGetNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns, err := o.GetNamespace(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (o *Operator) handleCreateNamespace(w http.ResponseWriter, r *http.Request) {
	var req CreateNamespaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Domain == "" || req.Network == "" {
		http.Error(w, `"name", "domain", and "network" are required`, http.StatusBadRequest)
		return
	}

	ns, err := o.CreateNamespace(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusCreated, ns)
}

func (o *Operator) handleDeleteNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := o.DeleteNamespace(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "still has") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── Instance Handlers ──────────────────────────────────────────────────────

func (o *Operator) handleListInstances(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.ListInstances())
}

func (o *Operator) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	inst, err := o.GetInstance(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

// ─── Health ─────────────────────────────────────────────────────────────────

func (o *Operator) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeDottedName handles zone names with dots (e.g., "kube.gt.lo")
// which Go's ServeMux may split across path segments.
// Falls back to extracting the full suffix from the raw URL path.
func decodeDottedName(pathValue, rawPath, prefix string) string {
	// If the path value already looks correct, use it
	if pathValue != "" && !strings.HasSuffix(rawPath, "/") {
		// Extract everything after the prefix from the raw path
		if idx := strings.Index(rawPath, prefix); idx >= 0 {
			return rawPath[idx+len(prefix):]
		}
	}
	return fmt.Sprintf("%s", pathValue)
}
