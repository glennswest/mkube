package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RegisterRoutes registers Kubernetes-compatible Pod API handlers on the provided mux.
func (p *MicroKubeProvider) RegisterRoutes(mux *http.ServeMux) {
	// Read
	mux.HandleFunc("GET /api/v1/pods", p.handleListAllPods)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods", p.handleListNamespacedPods)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}", p.handleGetPod)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}/status", p.handleGetPodStatus)

	// Write
	mux.HandleFunc("POST /api/v1/namespaces/{namespace}/pods", p.handleCreatePod)
	mux.HandleFunc("PUT /api/v1/namespaces/{namespace}/pods/{name}", p.handleUpdatePod)
	mux.HandleFunc("DELETE /api/v1/namespaces/{namespace}/pods/{name}", p.handleDeletePod)

	// Logs
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}/log", p.handleGetPodLog)

	// ConfigMaps
	mux.HandleFunc("POST /api/v1/namespaces/{namespace}/configmaps", p.handleCreateConfigMap)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/configmaps", p.handleListConfigMaps)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/configmaps/{name}", p.handleGetConfigMap)
	mux.HandleFunc("DELETE /api/v1/namespaces/{namespace}/configmaps/{name}", p.handleDeleteConfigMap)

	// Nodes
	mux.HandleFunc("GET /api/v1/nodes", p.handleListNodes)
	mux.HandleFunc("GET /api/v1/nodes/{name}", p.handleGetNode)

	// API discovery (kubectl compat)
	mux.HandleFunc("GET /api", p.handleAPIVersions)
	mux.HandleFunc("GET /api/v1", p.handleAPIResources)

	// Export/Import
	mux.HandleFunc("GET /api/v1/export", p.handleExport)
	mux.HandleFunc("POST /api/v1/import", p.handleImport)

	// Consistency
	mux.HandleFunc("GET /api/v1/consistency", p.handleConsistency)

	// Health
	mux.HandleFunc("GET /healthz", p.handleHealthz)
}

func (p *MicroKubeProvider) handleListAllPods(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchPods(w, r, "")
		return
	}

	pods, err := p.GetPods(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		enriched := pod.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		if status, err := p.GetPodStatus(r.Context(), pod.Namespace, pod.Name); err == nil {
			enriched.Status = *status
		}
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedPods(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchPods(w, r, ns)
		return
	}

	pods, err := p.GetPods(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]corev1.Pod, 0)
	for _, pod := range pods {
		if pod.Namespace != ns {
			continue
		}
		enriched := pod.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		if status, err := p.GetPodStatus(r.Context(), pod.Namespace, pod.Name); err == nil {
			enriched.Status = *status
		}
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetPod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	pod, err := p.GetPod(r.Context(), ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	enriched := pod.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	if status, err := p.GetPodStatus(r.Context(), ns, name); err == nil {
		enriched.Status = *status
	}

	podWriteJSON(w, http.StatusOK, enriched)
}

func (p *MicroKubeProvider) handleGetPodStatus(w http.ResponseWriter, r *http.Request) {
	p.handleGetPod(w, r)
}

// handleCreatePod decodes a Pod JSON body, sets namespace from path, and creates it.
func (p *MicroKubeProvider) handleCreatePod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var pod corev1.Pod
	if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
		http.Error(w, fmt.Sprintf("invalid pod JSON: %v", err), http.StatusBadRequest)
		return
	}
	pod.Namespace = ns
	if pod.Name == "" {
		http.Error(w, "pod name is required", http.StatusBadRequest)
		return
	}

	// Check for duplicate
	if _, err := p.GetPod(r.Context(), ns, pod.Name); err == nil {
		http.Error(w, fmt.Sprintf("pod %s/%s already exists", ns, pod.Name), http.StatusConflict)
		return
	}

	// Persist to NATS store first (source of truth)
	if p.deps.Store != nil {
		storeKey := ns + "." + pod.Name
		if _, err := p.deps.Store.Pods.PutJSON(r.Context(), storeKey, &pod); err != nil {
			http.Error(w, fmt.Sprintf("persisting pod: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if err := p.CreatePod(r.Context(), &pod); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	podWriteJSON(w, http.StatusCreated, &pod)
}

// handleUpdatePod decodes a Pod JSON body and performs a rolling update.
func (p *MicroKubeProvider) handleUpdatePod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	var pod corev1.Pod
	if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
		http.Error(w, fmt.Sprintf("invalid pod JSON: %v", err), http.StatusBadRequest)
		return
	}
	pod.Namespace = ns
	pod.Name = name

	if _, err := p.GetPod(r.Context(), ns, name); err != nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found", ns, name), http.StatusNotFound)
		return
	}

	// Persist updated spec to NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.Pods.PutJSON(r.Context(), storeKey, &pod); err != nil {
			http.Error(w, fmt.Sprintf("persisting pod update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if err := p.UpdatePod(r.Context(), &pod); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	podWriteJSON(w, http.StatusOK, &pod)
}

// handleDeletePod finds and deletes the specified pod.
func (p *MicroKubeProvider) handleDeletePod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	pod, err := p.GetPod(r.Context(), ns, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found", ns, name), http.StatusNotFound)
		return
	}

	if err := p.DeletePod(r.Context(), pod); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove from NATS store after successful deletion
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.Pods.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete pod from store", "key", storeKey, "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("pod %q deleted", name),
	})
}

// handleGetPodLog streams container logs for a pod.
func (p *MicroKubeProvider) handleGetPodLog(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	pod, err := p.GetPod(r.Context(), ns, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found", ns, name), http.StatusNotFound)
		return
	}

	// Determine which container to get logs for
	containerName := r.URL.Query().Get("container")
	if containerName == "" && len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	rosName := sanitizeName(pod, containerName)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Try micrologs first if configured
	if p.deps.Config.Logging.Enabled && p.deps.Config.Logging.URL != "" {
		logsURL := strings.TrimRight(p.deps.Config.Logging.URL, "/") +
			fmt.Sprintf("/api/v1/logs/%s/%s/%s", ns, name, containerName)
		req, err := http.NewRequestWithContext(r.Context(), "GET", logsURL, nil)
		if err == nil {
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				_, _ = io.Copy(w, resp.Body)
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}

	// Fall back to runtime logs filtered by container name
	logs, err := p.deps.Runtime.GetLogs(r.Context(), rosName)
	if err != nil {
		http.Error(w, fmt.Sprintf("error fetching logs: %v", err), http.StatusInternalServerError)
		return
	}

	for _, entry := range logs {
		if strings.Contains(entry.Message, rosName) {
			_, _ = fmt.Fprintf(w, "%s %s %s\n", entry.Timestamp, entry.Stream, entry.Message)
		}
	}
}

// handleListNodes returns a NodeList with this node.
func (p *MicroKubeProvider) handleListNodes(w http.ResponseWriter, r *http.Request) {
	node := p.buildNode(r)
	podWriteJSON(w, http.StatusOK, corev1.NodeList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
		Items:    []corev1.Node{*node},
	})
}

// handleGetNode returns the node object for the requested node name.
func (p *MicroKubeProvider) handleGetNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != p.nodeName {
		http.Error(w, fmt.Sprintf("node %q not found", name), http.StatusNotFound)
		return
	}
	node := p.buildNode(r)
	podWriteJSON(w, http.StatusOK, node)
}

// buildNode constructs a corev1.Node enriched with live system resource data.
func (p *MicroKubeProvider) buildNode(r *http.Request) *corev1.Node {
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              p.nodeName,
			CreationTimestamp: metav1.Time{Time: p.startTime},
		},
	}
	p.ConfigureNode(r.Context(), node)

	// Enrich with live system resource data
	sysRes, err := p.deps.Runtime.GetSystemResource(r.Context())
	if err == nil {
		if sysRes.CPUCount != "" {
			node.Status.Capacity[corev1.ResourceCPU] = resource.MustParse(sysRes.CPUCount)
			node.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse(sysRes.CPUCount)
		}
		if sysRes.TotalMemory != "" {
			node.Status.Capacity[corev1.ResourceMemory] = resource.MustParse(sysRes.TotalMemory)
		}
		if sysRes.FreeMemory != "" {
			node.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse(sysRes.FreeMemory)
		}
		node.Status.NodeInfo.Architecture = sysRes.Architecture
		node.Status.NodeInfo.KernelVersion = sysRes.Version
		node.Status.NodeInfo.OSImage = sysRes.Platform + " " + sysRes.Version

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations["mkube.io/board"] = sysRes.BoardName
		node.Annotations["mkube.io/uptime"] = sysRes.Uptime
		node.Annotations["mkube.io/cpu-load"] = sysRes.CPULoad
	}

	// Pod count
	pods, _ := p.GetPods(r.Context())
	node.Status.Allocatable[corev1.ResourcePods] = resource.MustParse(fmt.Sprintf("%d", 20-len(pods)))

	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: p.deps.Config.DefaultNetwork().Gateway},
		{Type: corev1.NodeHostName, Address: p.nodeName},
	}

	return node
}

// handleAPIVersions returns the supported API versions (kubectl discovery).
func (p *MicroKubeProvider) handleAPIVersions(w http.ResponseWriter, r *http.Request) {
	podWriteJSON(w, http.StatusOK, metav1.APIVersions{
		TypeMeta: metav1.TypeMeta{Kind: "APIVersions"},
		Versions: []string{"v1"},
		ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{
			{ClientCIDR: "0.0.0.0/0", ServerAddress: r.Host},
		},
	})
}

// handleAPIResources returns the available API resources for v1 (kubectl discovery).
func (p *MicroKubeProvider) handleAPIResources(w http.ResponseWriter, r *http.Request) {
	podWriteJSON(w, http.StatusOK, metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList"},
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{
				Name:       "pods",
				Namespaced: true,
				Kind:       "Pod",
				Verbs:      metav1.Verbs{"get", "list", "create", "update", "delete"},
			},
			{
				Name:       "pods/log",
				Namespaced: true,
				Kind:       "Pod",
				Verbs:      metav1.Verbs{"get"},
			},
			{
				Name:       "pods/status",
				Namespaced: true,
				Kind:       "Pod",
				Verbs:      metav1.Verbs{"get"},
			},
			{
				Name:       "configmaps",
				Namespaced: true,
				Kind:       "ConfigMap",
				Verbs:      metav1.Verbs{"get", "list", "create", "delete"},
			},
			{
				Name:       "namespaces",
				Namespaced: false,
				Kind:       "Namespace",
				Verbs:      metav1.Verbs{"get", "list"},
			},
			{
				Name:       "nodes",
				Namespaced: false,
				Kind:       "Node",
				Verbs:      metav1.Verbs{"get", "list"},
			},
		},
	})
}

// handleHealthz returns a simple health check response.
func (p *MicroKubeProvider) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "ok\nnode: %s\nuptime: %s\n", p.nodeName, time.Since(p.startTime).Truncate(time.Second))
}

// handleExport returns the full state as multi-document YAML.
func (p *MicroKubeProvider) handleExport(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil {
		http.Error(w, "NATS store not configured", http.StatusServiceUnavailable)
		return
	}

	data, err := p.deps.Store.ExportYAML(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleImport upserts pods and configmaps from a multi-document YAML body.
func (p *MicroKubeProvider) handleImport(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil {
		http.Error(w, "NATS store not configured", http.StatusServiceUnavailable)
		return
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading body: %v", err), http.StatusBadRequest)
		return
	}

	pods, cms, err := p.deps.Store.ImportYAML(r.Context(), data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	podWriteJSON(w, http.StatusOK, map[string]int{
		"pods":       pods,
		"configmaps": cms,
	})
}

func podWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
