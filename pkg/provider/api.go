package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
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
	mux.HandleFunc("PATCH /api/v1/namespaces/{namespace}/pods/{name}", p.handlePatchPod)
	mux.HandleFunc("DELETE /api/v1/namespaces/{namespace}/pods/{name}", p.handleDeletePod)

	// Logs
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}/log", p.handleGetPodLog)

	// ConfigMaps
	mux.HandleFunc("POST /api/v1/namespaces/{namespace}/configmaps", p.handleCreateConfigMap)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/configmaps", p.handleListConfigMaps)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/configmaps/{name}", p.handleGetConfigMap)
	mux.HandleFunc("PUT /api/v1/namespaces/{namespace}/configmaps/{name}", p.handleUpdateConfigMap)
	mux.HandleFunc("PATCH /api/v1/namespaces/{namespace}/configmaps/{name}", p.handlePatchConfigMap)
	mux.HandleFunc("DELETE /api/v1/namespaces/{namespace}/configmaps/{name}", p.handleDeleteConfigMap)

	// BareMetalHosts
	mux.HandleFunc("GET /api/v1/baremetalhosts", p.handleListAllBMH)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/baremetalhosts", p.handleListNamespacedBMH)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/baremetalhosts/{name}", p.handleGetBMH)
	mux.HandleFunc("POST /api/v1/namespaces/{namespace}/baremetalhosts", p.handleCreateBMH)
	mux.HandleFunc("PUT /api/v1/namespaces/{namespace}/baremetalhosts/{name}", p.handleUpdateBMH)
	mux.HandleFunc("PATCH /api/v1/namespaces/{namespace}/baremetalhosts/{name}", p.handlePatchBMH)
	mux.HandleFunc("DELETE /api/v1/namespaces/{namespace}/baremetalhosts/{name}", p.handleDeleteBMH)

	// Namespaces — registered by namespace.Manager.RegisterRoutes()

	// Nodes
	mux.HandleFunc("GET /api/v1/nodes", p.handleListNodes)
	mux.HandleFunc("GET /api/v1/nodes/{name}", p.handleGetNode)

	// Events (stub)
	mux.HandleFunc("GET /api/v1/events", p.handleListEvents)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/events", p.handleListEvents)

	// Services (stub)
	mux.HandleFunc("GET /api/v1/services", p.handleListServices)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/services", p.handleListServices)

	// API discovery (kubectl compat)
	mux.HandleFunc("GET /api", p.handleAPIVersions)
	mux.HandleFunc("GET /api/v1", p.handleAPIResources)
	mux.HandleFunc("GET /apis", p.handleAPIGroups)
	mux.HandleFunc("GET /version", p.handleVersion)

	// Export/Import
	mux.HandleFunc("GET /api/v1/export", p.handleExport)
	mux.HandleFunc("POST /api/v1/import", p.handleImport)

	// Consistency
	mux.HandleFunc("GET /api/v1/consistency", p.handleConsistency)
	mux.HandleFunc("POST /api/v1/consistency/repair", p.handleConsistencyRepair)

	// DNS validation
	mux.HandleFunc("GET /api/v1/dns/validate", p.handleDNSValidate)

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
		p.enrichPod(r.Context(), enriched)
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, podListToTable(items))
		return
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
		p.enrichPod(r.Context(), enriched)
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, podListToTable(items))
		return
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
	p.enrichPod(r.Context(), enriched)

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, podListToTable([]corev1.Pod{*enriched}))
		return
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
	if pod.CreationTimestamp.IsZero() {
		pod.CreationTimestamp = metav1.Now()
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

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, nodeListToTable([]corev1.Node{*node}))
		return
	}

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
	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, nodeListToTable([]corev1.Node{*node}))
		return
	}
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

// handleAPIGroups returns an empty API group list (satisfies oc/kubectl discovery of /apis).
func (p *MicroKubeProvider) handleAPIGroups(w http.ResponseWriter, r *http.Request) {
	podWriteJSON(w, http.StatusOK, metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{Kind: "APIGroupList"},
		Groups:   []metav1.APIGroup{},
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
				Verbs:      metav1.Verbs{"get", "list", "create", "update", "patch", "delete"},
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
				Verbs:      metav1.Verbs{"get", "list", "create", "update", "patch", "delete"},
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
			{
				Name:       "events",
				Namespaced: true,
				Kind:       "Event",
				Verbs:      metav1.Verbs{"get", "list"},
			},
			{
				Name:       "services",
				Namespaced: true,
				Kind:       "Service",
				Verbs:      metav1.Verbs{"get", "list"},
			},
			{
				Name:       "baremetalhosts",
				Namespaced: true,
				Kind:       "BareMetalHost",
				ShortNames: []string{"bmh"},
				Verbs:      metav1.Verbs{"get", "list", "create", "update", "patch", "delete"},
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

// enrichPod sets TypeMeta, CreationTimestamp, and live status on a pod for API responses.
func (p *MicroKubeProvider) enrichPod(ctx context.Context, pod *corev1.Pod) {
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	if pod.CreationTimestamp.IsZero() {
		pod.CreationTimestamp = metav1.Time{Time: p.startTime}
	}
	if status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err == nil {
		pod.Status = *status
	}
}

// handlePatchPod applies a patch (treated as full replace) to a pod.
func (p *MicroKubeProvider) handlePatchPod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	existing, err := p.GetPod(r.Context(), ns, name)
	if err != nil {
		http.Error(w, fmt.Sprintf("pod %s/%s not found", ns, name), http.StatusNotFound)
		return
	}

	var patch corev1.Pod
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Preserve identity and creation time from existing pod
	patch.Namespace = ns
	patch.Name = name
	if patch.CreationTimestamp.IsZero() {
		patch.CreationTimestamp = existing.CreationTimestamp
	}

	// Persist and update
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.Pods.PutJSON(r.Context(), storeKey, &patch); err != nil {
			http.Error(w, fmt.Sprintf("persisting pod patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if err := p.UpdatePod(r.Context(), &patch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	patch.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	podWriteJSON(w, http.StatusOK, &patch)
}

// handleUpdateConfigMap replaces a ConfigMap (PUT).
func (p *MicroKubeProvider) handleUpdateConfigMap(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.configMaps[key]; !ok {
		http.Error(w, fmt.Sprintf("configmap %s not found", key), http.StatusNotFound)
		return
	}

	var cm corev1.ConfigMap
	if err := json.NewDecoder(r.Body).Decode(&cm); err != nil {
		http.Error(w, fmt.Sprintf("invalid configmap JSON: %v", err), http.StatusBadRequest)
		return
	}
	cm.Namespace = ns
	cm.Name = name
	cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.ConfigMaps.PutJSON(r.Context(), storeKey, &cm); err != nil {
			http.Error(w, fmt.Sprintf("persisting configmap: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.configMaps[key] = &cm
	podWriteJSON(w, http.StatusOK, &cm)
}

// handlePatchConfigMap applies a patch (treated as replace) to a ConfigMap.
func (p *MicroKubeProvider) handlePatchConfigMap(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.configMaps[key]; !ok {
		http.Error(w, fmt.Sprintf("configmap %s not found", key), http.StatusNotFound)
		return
	}

	var cm corev1.ConfigMap
	if err := json.NewDecoder(r.Body).Decode(&cm); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}
	cm.Namespace = ns
	cm.Name = name
	cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.ConfigMaps.PutJSON(r.Context(), storeKey, &cm); err != nil {
			http.Error(w, fmt.Sprintf("persisting configmap patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.configMaps[key] = &cm
	podWriteJSON(w, http.StatusOK, &cm)
}

// handleListNamespaces returns namespaces derived from tracked pods and configmaps.
func (p *MicroKubeProvider) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	nsSet := make(map[string]bool)
	for _, pod := range p.pods {
		nsSet[pod.Namespace] = true
	}
	for _, cm := range p.configMaps {
		nsSet[cm.Namespace] = true
	}
	for _, bmh := range p.bareMetalHosts {
		nsSet[bmh.Namespace] = true
	}
	// Always include "default"
	nsSet["default"] = true

	items := make([]corev1.Namespace, 0, len(nsSet))
	for ns := range nsSet {
		items = append(items, corev1.Namespace{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{
				Name:              ns,
				CreationTimestamp: metav1.Time{Time: p.startTime},
			},
			Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
		})
	}

	podWriteJSON(w, http.StatusOK, corev1.NamespaceList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"},
		Items:    items,
	})
}

// handleGetNamespace returns a single namespace.
func (p *MicroKubeProvider) handleGetNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Check if namespace exists in tracked resources
	found := name == "default"
	if !found {
		for _, pod := range p.pods {
			if pod.Namespace == name {
				found = true
				break
			}
		}
	}
	if !found {
		for _, cm := range p.configMaps {
			if cm.Namespace == name {
				found = true
				break
			}
		}
	}

	if !found {
		podWriteJSON(w, http.StatusNotFound, metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   "Failure",
			Message:  fmt.Sprintf("namespaces %q not found", name),
			Reason:   metav1.StatusReasonNotFound,
			Code:     http.StatusNotFound,
		})
		return
	}

	podWriteJSON(w, http.StatusOK, corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.Time{Time: p.startTime},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})
}

// handleListEvents returns recorded events, optionally filtered by namespace.
func (p *MicroKubeProvider) handleListEvents(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	fieldSelector := r.URL.Query().Get("fieldSelector")

	items := make([]corev1.Event, 0)
	for _, evt := range p.events {
		if ns != "" && evt.Namespace != ns {
			continue
		}
		// Support fieldSelector=involvedObject.name=<name> (used by oc describe)
		if fieldSelector != "" {
			if strings.HasPrefix(fieldSelector, "involvedObject.name=") {
				objName := strings.TrimPrefix(fieldSelector, "involvedObject.name=")
				// Handle compound selectors
				if idx := strings.Index(objName, ","); idx >= 0 {
					objName = objName[:idx]
				}
				if evt.InvolvedObject.Name != objName {
					continue
				}
			}
		}
		items = append(items, evt)
	}

	podWriteJSON(w, http.StatusOK, corev1.EventList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"},
		Items:    items,
	})
}

// handleListServices returns an empty ServiceList.
func (p *MicroKubeProvider) handleListServices(w http.ResponseWriter, r *http.Request) {
	podWriteJSON(w, http.StatusOK, corev1.ServiceList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceList"},
		Items:    []corev1.Service{},
	})
}

// handleVersion returns server version info.
func (p *MicroKubeProvider) handleVersion(w http.ResponseWriter, r *http.Request) {
	podWriteJSON(w, http.StatusOK, map[string]string{
		"major":        "1",
		"minor":        "29",
		"gitVersion":   "v1.29.0-mkube",
		"gitCommit":    "",
		"gitTreeState": "clean",
		"buildDate":    p.startTime.Format(time.RFC3339),
		"goVersion":    "go1.22",
		"compiler":     "gc",
		"platform":     "linux/arm64",
	})
}

// wantsTable checks if the client requested Table format via Accept header.
func wantsTable(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "as=Table")
}

// nodeListToTable converts a NodeList to a Table response for oc/kubectl.
func nodeListToTable(nodes []corev1.Node) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string"},
			{Name: "Roles", Type: "string"},
			{Name: "Age", Type: "string"},
			{Name: "Version", Type: "string"},
		},
	}

	for i := range nodes {
		node := &nodes[i]

		status := "NotReady"
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				status = "Ready"
				break
			}
		}

		roles := "<none>"
		for label := range node.Labels {
			if strings.HasPrefix(label, "node-role.kubernetes.io/") {
				role := strings.TrimPrefix(label, "node-role.kubernetes.io/")
				if role != "" {
					roles = role
				}
			}
		}

		age := "<unknown>"
		if !node.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(node.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              node.Name,
				"creationTimestamp": node.CreationTimestamp.Format(time.RFC3339),
			},
		})
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				node.Name,
				status,
				roles,
				age,
				node.Status.NodeInfo.KubeletVersion,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// podListToTable converts a PodList to a Table response for oc/kubectl.
func podListToTable(pods []corev1.Pod) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Ready", Type: "string"},
			{Name: "Status", Type: "string"},
			{Name: "Restarts", Type: "integer"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range pods {
		pod := &pods[i]
		ready, total := 0, len(pod.Spec.Containers)
		var restarts int32
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}

		status := string(pod.Status.Phase)
		// Derive status from container states (like kubectl does)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				status = cs.State.Waiting.Reason
			}
			if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
				status = cs.State.Terminated.Reason
			}
		}

		age := "<unknown>"
		if !pod.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(pod.CreationTimestamp.Time))
		}

		// Wrap as PartialObjectMetadata so oc can extract namespace/name
		partialMeta := map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              pod.Name,
				"namespace":         pod.Namespace,
				"creationTimestamp": pod.CreationTimestamp.Format(time.RFC3339),
			},
		}
		raw, _ := json.Marshal(partialMeta)
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				pod.Name,
				fmt.Sprintf("%d/%d", ready, total),
				status,
				restarts,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// formatAge returns a human-readable age string similar to kubectl.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func podWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
