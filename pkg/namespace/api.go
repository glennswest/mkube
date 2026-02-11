package namespace

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	annDomain       = "vkube.io/domain"
	annNetwork      = "vkube.io/network"
	annMode         = "vkube.io/mode"
	annDedicatedDNS = "vkube.io/dedicated-dns"
)

// RegisterRoutes registers namespace API handlers on the provided mux.
func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/namespaces", m.handleList)
	mux.HandleFunc("GET /api/v1/namespaces/{name}", m.handleGet)
	mux.HandleFunc("POST /api/v1/namespaces", m.handleCreate)
	mux.HandleFunc("DELETE /api/v1/namespaces/{name}", m.handleDelete)
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	nss := m.ListNamespaces()

	items := make([]corev1.Namespace, 0, len(nss))
	for _, ns := range nss {
		items = append(items, toK8sNamespace(ns))
	}

	list := corev1.NamespaceList{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "NamespaceList",
		},
		Items: items,
	}

	writeJSON(w, http.StatusOK, list)
}

func (m *Manager) handleGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns, err := m.GetNamespace(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toK8sNamespace(ns))
}

func (m *Manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var k8sNS corev1.Namespace
	if err := json.NewDecoder(r.Body).Decode(&k8sNS); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if k8sNS.Name == "" {
		http.Error(w, `"metadata.name" is required`, http.StatusBadRequest)
		return
	}

	name, domain, network, mode, dedicated := fromK8sNamespace(&k8sNS, m)

	ns, err := m.CreateNamespace(r.Context(), name, domain, network, mode, dedicated)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}

	writeJSON(w, http.StatusCreated, toK8sNamespace(ns))
}

func (m *Manager) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := m.DeleteNamespace(r.Context(), name); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "still has") {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)

	// Return a K8s-style status object
	writeJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
	})
}

// ─── K8s Conversion ─────────────────────────────────────────────────────────

func toK8sNamespace(ns *Namespace) corev1.Namespace {
	annotations := map[string]string{
		annDomain:  ns.Domain,
		annNetwork: ns.Network,
		annMode:    string(ns.Mode),
	}

	phase := corev1.NamespaceActive
	if len(ns.Containers) == 0 {
		// Still active, just empty
		phase = corev1.NamespaceActive
	}

	return corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              ns.Name,
			Annotations:       annotations,
			CreationTimestamp:  metav1.Time{Time: time.Time{}},
		},
		Status: corev1.NamespaceStatus{
			Phase: phase,
		},
	}
}

func fromK8sNamespace(k8sNS *corev1.Namespace, m *Manager) (name, domain, network string, mode NetworkingMode, dedicated bool) {
	name = k8sNS.Name

	ann := k8sNS.Annotations
	if ann == nil {
		ann = map[string]string{}
	}

	domain = ann[annDomain]
	network = ann[annNetwork]

	if modeStr, ok := ann[annMode]; ok {
		mode = NetworkingMode(modeStr)
	}

	if ann[annDedicatedDNS] == "true" {
		dedicated = true
	}

	return
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeErrorJSON writes a K8s-style error response.
func writeErrorJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Failure",
		Message:  message,
		Reason:   metav1.StatusReason(fmt.Sprintf("%d", status)),
		Code:     int32(status),
	})
}
