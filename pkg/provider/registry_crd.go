package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// Registry is a cluster-scoped CRD representing an OCI registry instance.
type Registry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              RegistrySpec   `json:"spec"`
	Status            RegistryStatus `json:"status,omitempty"`
}

// RegistrySpec defines the desired state of a Registry.
type RegistrySpec struct {
	Network            string              `json:"network"`                      // e.g. "gt"
	StaticIP           string              `json:"staticIP"`                     // e.g. "192.168.200.6"
	Hostname           string              `json:"hostname"`                     // e.g. "registry-stormbase.gt.lo"
	StorePath          string              `json:"storePath,omitempty"`          // default /raid1/registry/{name}
	ListenAddr         string              `json:"listenAddr,omitempty"`         // default ":5000"
	PullThrough        bool                `json:"pullThrough,omitempty"`
	UpstreamRegistries []string            `json:"upstreamRegistries,omitempty"`
	WatchImages        []config.WatchImage `json:"watchImages,omitempty"`
	WatchPollSeconds   int                 `json:"watchPollSeconds,omitempty"`   // default 120
	NotifyURL          string              `json:"notifyURL,omitempty"`          // webhook to mkube
	Managed            bool                `json:"managed,omitempty"`            // auto-deploy pod
	TLSCertFile        string              `json:"tlsCertFile,omitempty"`        // in-container path
	TLSKeyFile         string              `json:"tlsKeyFile,omitempty"`         // in-container path
}

// RegistryStatus reports the observed state of a Registry.
type RegistryStatus struct {
	Phase    string `json:"phase"`              // Active, Degraded, Error
	Alive    bool   `json:"alive,omitempty"`
	PodCount int    `json:"podCount,omitempty"`
}

// RegistryList is a list of Registry objects.
type RegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Registry `json:"items"`
}

// DeepCopy returns a deep copy of the Registry.
func (r *Registry) DeepCopy() *Registry {
	out := *r
	out.ObjectMeta = *r.ObjectMeta.DeepCopy()
	out.Spec.UpstreamRegistries = append([]string(nil), r.Spec.UpstreamRegistries...)
	out.Spec.WatchImages = append([]config.WatchImage(nil), r.Spec.WatchImages...)
	return &out
}

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadRegistriesFromStore loads Registry objects from the NATS REGISTRIES bucket.
func (p *MicroKubeProvider) LoadRegistriesFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Registries == nil {
		return
	}

	keys, err := p.deps.Store.Registries.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list registries from store", "error", err)
		return
	}

	for _, key := range keys {
		var reg Registry
		if _, err := p.deps.Store.Registries.GetJSON(ctx, key, &reg); err != nil {
			p.deps.Logger.Warnw("failed to read registry from store", "key", key, "error", err)
			continue
		}
		p.registries[reg.Name] = &reg
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded registries from store", "count", len(keys))
	}
}

// MigrateRegistryConfig migrates config.yaml RegistryConfig into a Registry CRD
// on first boot (when the REGISTRIES bucket is empty).
func (p *MicroKubeProvider) MigrateRegistryConfig(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Registries == nil {
		return
	}

	empty, err := p.deps.Store.Registries.IsEmpty(ctx)
	if err != nil {
		p.deps.Logger.Warnw("failed to check registries store", "error", err)
		return
	}
	if !empty {
		p.deps.Logger.Debugw("registries store already populated, skipping migration")
		return
	}

	cfg := p.deps.Config.Registry
	if !cfg.Enabled {
		return
	}

	p.deps.Logger.Infow("migrating config.yaml registry to Registry CRD")

	reg := Registry{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: RegistrySpec{
			Network:            "gt",
			StaticIP:           "192.168.200.3",
			Hostname:           "registry.gt.lo",
			StorePath:          cfg.StorePath,
			ListenAddr:         cfg.ListenAddr,
			PullThrough:        cfg.PullThrough,
			UpstreamRegistries: cfg.UpstreamRegistries,
			WatchImages:        cfg.WatchImages,
			WatchPollSeconds:   cfg.WatchPollSeconds,
			NotifyURL:          cfg.NotifyURL,
			Managed:            false, // installer owns the default registry
			TLSCertFile:        cfg.TLSCertFile,
			TLSKeyFile:         cfg.TLSKeyFile,
		},
		Status: RegistryStatus{
			Phase: "Active",
		},
	}

	if _, err := p.deps.Store.Registries.PutJSON(ctx, reg.Name, &reg); err != nil {
		p.deps.Logger.Warnw("failed to migrate registry config", "error", err)
		return
	}
	p.registries[reg.Name] = &reg
	p.deps.Logger.Infow("migrated registry config to CRD", "name", reg.Name)
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListRegistries(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchRegistries(w, r)
		return
	}

	items := make([]Registry, 0, len(p.registries))
	for _, reg := range p.registries {
		enriched := reg.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
		p.enrichRegistryStatus(r.Context(), enriched)
		items = append(items, *enriched)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, registryListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, RegistryList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "RegistryList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetRegistry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	reg, ok := p.registries[name]
	if !ok {
		http.Error(w, fmt.Sprintf("registry %q not found", name), http.StatusNotFound)
		return
	}

	enriched := reg.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
	p.enrichRegistryStatus(r.Context(), enriched)

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, registryListToTable([]Registry{*enriched}))
		return
	}

	podWriteJSON(w, http.StatusOK, enriched)
}

func (p *MicroKubeProvider) handleCreateRegistry(w http.ResponseWriter, r *http.Request) {
	var reg Registry
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, fmt.Sprintf("invalid Registry JSON: %v", err), http.StatusBadRequest)
		return
	}

	if reg.Name == "" {
		http.Error(w, "registry name is required", http.StatusBadRequest)
		return
	}
	if reg.Spec.Network == "" {
		http.Error(w, "registry network is required", http.StatusBadRequest)
		return
	}
	if reg.Spec.StaticIP == "" {
		http.Error(w, "registry staticIP is required", http.StatusBadRequest)
		return
	}

	// Validate network exists
	if _, exists := p.networks[reg.Spec.Network]; !exists {
		http.Error(w, fmt.Sprintf("network %q not found", reg.Spec.Network), http.StatusBadRequest)
		return
	}

	if _, exists := p.registries[reg.Name]; exists {
		http.Error(w, fmt.Sprintf("registry %q already exists", reg.Name), http.StatusConflict)
		return
	}

	reg.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
	if reg.CreationTimestamp.IsZero() {
		reg.CreationTimestamp = metav1.Now()
	}
	if reg.Status.Phase == "" {
		reg.Status.Phase = "Active"
	}

	// Apply defaults
	if reg.Spec.StorePath == "" {
		reg.Spec.StorePath = "/raid1/registry/" + reg.Name
	}
	if reg.Spec.ListenAddr == "" {
		reg.Spec.ListenAddr = ":5000"
	}
	if reg.Spec.Hostname == "" {
		net := p.networks[reg.Spec.Network]
		reg.Spec.Hostname = fmt.Sprintf("registry-%s.%s", reg.Name, net.Spec.DNS.Zone)
	}
	if reg.Spec.WatchPollSeconds == 0 {
		reg.Spec.WatchPollSeconds = 120
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.Registries != nil {
		if _, err := p.deps.Store.Registries.PutJSON(r.Context(), reg.Name, &reg); err != nil {
			http.Error(w, fmt.Sprintf("persisting registry: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.registries[reg.Name] = &reg

	// Auto-deploy managed registry pod
	if reg.Spec.Managed {
		if err := p.deployManagedRegistry(r.Context(), &reg); err != nil {
			p.deps.Logger.Warnw("auto-deploy registry failed", "registry", reg.Name, "error", err)
		}
	}

	podWriteJSON(w, http.StatusCreated, &reg)
}

func (p *MicroKubeProvider) handleUpdateRegistry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	old, ok := p.registries[name]
	if !ok {
		http.Error(w, fmt.Sprintf("registry %q not found", name), http.StatusNotFound)
		return
	}
	wasManaged := old.Spec.Managed

	var reg Registry
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		http.Error(w, fmt.Sprintf("invalid Registry JSON: %v", err), http.StatusBadRequest)
		return
	}
	reg.Name = name
	reg.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}

	if reg.CreationTimestamp.IsZero() {
		reg.CreationTimestamp = old.CreationTimestamp
	}

	if p.deps.Store != nil && p.deps.Store.Registries != nil {
		if _, err := p.deps.Store.Registries.PutJSON(r.Context(), name, &reg); err != nil {
			http.Error(w, fmt.Sprintf("persisting registry update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.registries[name] = &reg

	p.handleManagedRegistryTransition(r.Context(), wasManaged, &reg)

	podWriteJSON(w, http.StatusOK, &reg)
}

func (p *MicroKubeProvider) handlePatchRegistry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.registries[name]
	if !ok {
		http.Error(w, fmt.Sprintf("registry %q not found", name), http.StatusNotFound)
		return
	}
	wasManaged := existing.Spec.Managed

	merged := existing.DeepCopy()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading body: %v", err), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}
	merged.Name = name
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
	merged.CreationTimestamp = existing.CreationTimestamp

	if p.deps.Store != nil && p.deps.Store.Registries != nil {
		if _, err := p.deps.Store.Registries.PutJSON(r.Context(), name, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting registry patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.registries[name] = merged

	p.handleManagedRegistryTransition(r.Context(), wasManaged, merged)

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	reg, ok := p.registries[name]
	if !ok {
		http.Error(w, fmt.Sprintf("registry %q not found", name), http.StatusNotFound)
		return
	}

	// Teardown auto-deployed registry pod
	if reg.Spec.Managed {
		if err := p.teardownManagedRegistry(r.Context(), reg); err != nil {
			p.deps.Logger.Warnw("teardown managed registry failed", "registry", name, "error", err)
		}
	}

	if p.deps.Store != nil && p.deps.Store.Registries != nil {
		if err := p.deps.Store.Registries.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting registry from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.registries, name)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("registry %q deleted", name),
	})
}

// ─── Config Generation ──────────────────────────────────────────────────────

// handleGetRegistryConfig generates a registry config.yaml for a Registry CRD.
func (p *MicroKubeProvider) handleGetRegistryConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	reg, ok := p.registries[name]
	if !ok {
		http.Error(w, fmt.Sprintf("registry %q not found", name), http.StatusNotFound)
		return
	}

	cfgYAML := p.generateRegistryConfigYAML(reg)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(cfgYAML))
}

// generateRegistryConfigYAML produces a YAML config matching config.RegistryConfig format.
func (p *MicroKubeProvider) generateRegistryConfigYAML(reg *Registry) string {
	listenAddr := reg.Spec.ListenAddr
	if listenAddr == "" {
		listenAddr = ":5000"
	}
	storePath := reg.Spec.StorePath
	if storePath == "" {
		storePath = "/raid1/registry/" + reg.Name
	}
	watchPoll := reg.Spec.WatchPollSeconds
	if watchPoll == 0 {
		watchPoll = 120
	}

	var b []byte
	cfg := map[string]interface{}{
		"enabled":    true,
		"listenAddr": listenAddr,
		"storePath":  storePath,
	}

	if reg.Spec.PullThrough {
		cfg["pullThrough"] = true
	}
	if len(reg.Spec.UpstreamRegistries) > 0 {
		cfg["upstreamRegistries"] = reg.Spec.UpstreamRegistries
	}
	if reg.Spec.NotifyURL != "" {
		cfg["notifyURL"] = reg.Spec.NotifyURL
	}
	if reg.Spec.TLSCertFile != "" {
		cfg["tlsCertFile"] = reg.Spec.TLSCertFile
	}
	if reg.Spec.TLSKeyFile != "" {
		cfg["tlsKeyFile"] = reg.Spec.TLSKeyFile
	}
	if len(reg.Spec.WatchImages) > 0 {
		cfg["watchImages"] = reg.Spec.WatchImages
		cfg["watchPollSeconds"] = watchPoll
	}

	b, _ = json.MarshalIndent(map[string]interface{}{"registry": cfg}, "", "  ")
	return string(b)
}

// ─── Status Enrichment ──────────────────────────────────────────────────────

// enrichRegistryStatus computes live status: pod count and registry liveness.
func (p *MicroKubeProvider) enrichRegistryStatus(ctx context.Context, reg *Registry) {
	// Count pods matching this registry
	count := 0
	podName := "registry-" + reg.Name
	for _, pod := range p.pods {
		if pod.Name == podName && pod.Namespace == reg.Spec.Network {
			count++
		}
	}
	// Also check for default registry pod
	if reg.Name == "default" {
		for _, pod := range p.pods {
			if pod.Name == "registry" && pod.Namespace == reg.Spec.Network {
				count++
			}
		}
	}
	reg.Status.PodCount = count

	// Liveness: HTTP GET /v2/ on registry port
	if reg.Spec.StaticIP != "" {
		port := "5000"
		if reg.Spec.ListenAddr != "" && reg.Spec.ListenAddr != ":5000" {
			// Extract port from listenAddr like ":5000"
			if len(reg.Spec.ListenAddr) > 1 && reg.Spec.ListenAddr[0] == ':' {
				port = reg.Spec.ListenAddr[1:]
			}
		}
		reg.Status.Alive = probeHTTP(reg.Spec.StaticIP, port, "/v2/", 3*time.Second)
	}
}

// probeHTTP performs a simple HTTP GET health check.
func probeHTTP(host, port, path string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://%s:%s%s", host, port, path)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchRegistries(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.Registries == nil {
		http.Error(w, "watch requires NATS store", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Send existing Registry objects as ADDED events
	enc := json.NewEncoder(w)
	for _, reg := range p.registries {
		enriched := reg.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
		p.enrichRegistryStatus(ctx, enriched)
		evt := K8sWatchEvent{Type: "ADDED", Object: enriched}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.Registries.WatchAll(ctx)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			var reg Registry
			if evt.Type == store.EventDelete {
				reg = Registry{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &reg); err != nil {
					continue
				}
				reg.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Registry"}
				p.enrichRegistryStatus(ctx, &reg)
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &reg,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func registryListToTable(registries []Registry) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Network", Type: "string"},
			{Name: "IP", Type: "string"},
			{Name: "Hostname", Type: "string"},
			{Name: "Managed", Type: "string"},
			{Name: "Alive", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range registries {
		reg := &registries[i]

		managed := "false"
		if reg.Spec.Managed {
			managed = "true"
		}
		alive := "unknown"
		if reg.Status.Alive {
			alive = "true"
		} else if reg.Spec.StaticIP != "" {
			alive = "false"
		}

		age := "<unknown>"
		if !reg.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(reg.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              reg.Name,
				"creationTimestamp": reg.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				reg.Name,
				reg.Spec.Network,
				reg.Spec.StaticIP,
				reg.Spec.Hostname,
				managed,
				alive,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Managed Registry Auto-Deploy ────────────────────────────────────────────

// deployManagedRegistry creates a ConfigMap and registry pod for a managed registry.
func (p *MicroKubeProvider) deployManagedRegistry(ctx context.Context, reg *Registry) error {
	log := p.deps.Logger.With("registry", reg.Name)

	// 1. Generate and persist ConfigMap
	cfgYAML := p.generateRegistryConfigYAML(reg)
	cmName := "registry-" + reg.Name + "-config"
	cm := corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: reg.Spec.Network},
		Data:       map[string]string{"config.yaml": cfgYAML},
	}

	cmKey := reg.Spec.Network + "/" + cmName
	p.configMaps[cmKey] = &cm

	if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
		storeKey := reg.Spec.Network + "." + cmName
		if _, err := p.deps.Store.ConfigMaps.PutJSON(ctx, storeKey, &cm); err != nil {
			return fmt.Errorf("persisting registry configmap: %w", err)
		}
	}

	p.syncConfigMapsToDisk(ctx)
	log.Infow("created registry ConfigMap", "configmap", cmName)

	// 2. Build registry pod
	podName := "registry-" + reg.Name
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: reg.Spec.Network,
			Annotations: map[string]string{
				annotationNetwork:     reg.Spec.Network,
				annotationStaticIP:    reg.Spec.StaticIP,
				annotationImagePolicy: "auto",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "registry",
					Image: "192.168.200.3:5000/mkube-registry:edge",
					StartupProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/v2/",
								Port: intstr.FromInt32(5000),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       3,
						FailureThreshold:    10,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/v2/",
								Port: intstr.FromInt32(5000),
							},
						},
						PeriodSeconds:    30,
						FailureThreshold: 3,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.FromInt32(5000),
							},
						},
						PeriodSeconds: 10,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/etc/registry"},
						{Name: "data", MountPath: reg.Spec.StorePath},
					},
				},
			},
		},
	}

	// 3. Persist pod to NATS
	if p.deps.Store != nil && p.deps.Store.Pods != nil {
		storeKey := reg.Spec.Network + "." + podName
		if _, err := p.deps.Store.Pods.PutJSON(ctx, storeKey, &pod); err != nil {
			return fmt.Errorf("persisting registry pod: %w", err)
		}
	}

	// 4. Create the pod (goes through ContainerRuntime interface)
	if err := p.CreatePod(ctx, &pod); err != nil {
		return fmt.Errorf("creating registry pod: %w", err)
	}

	log.Infow("deployed managed registry pod",
		"pod", reg.Spec.Network+"/"+podName,
		"ip", reg.Spec.StaticIP,
		"hostname", reg.Spec.Hostname)
	return nil
}

// teardownManagedRegistry removes the auto-deployed registry pod and ConfigMap.
func (p *MicroKubeProvider) teardownManagedRegistry(ctx context.Context, reg *Registry) error {
	log := p.deps.Logger.With("registry", reg.Name)

	podName := "registry-" + reg.Name
	podMapKey := reg.Spec.Network + "/" + podName

	// Remove registry pod
	if pod, ok := p.pods[podMapKey]; ok {
		if err := p.DeletePod(ctx, pod); err != nil {
			log.Warnw("failed to delete managed registry pod", "error", err)
		} else {
			log.Infow("deleted managed registry pod")
		}
		if p.deps.Store != nil && p.deps.Store.Pods != nil {
			storeKey := reg.Spec.Network + "." + podName
			if err := p.deps.Store.Pods.Delete(ctx, storeKey); err != nil {
				log.Warnw("failed to delete registry pod from store", "error", err)
			}
		}
	}

	// Remove registry ConfigMap
	cmName := "registry-" + reg.Name + "-config"
	cmKey := reg.Spec.Network + "/" + cmName
	if _, ok := p.configMaps[cmKey]; ok {
		delete(p.configMaps, cmKey)
		if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
			storeKey := reg.Spec.Network + "." + cmName
			if err := p.deps.Store.ConfigMaps.Delete(ctx, storeKey); err != nil {
				log.Warnw("failed to delete registry configmap from store", "error", err)
			}
		}
		log.Infow("deleted managed registry ConfigMap")
	}

	return nil
}

// handleManagedRegistryTransition handles managed flag changes on update/patch.
func (p *MicroKubeProvider) handleManagedRegistryTransition(ctx context.Context, wasManaged bool, reg *Registry) {
	nowManaged := reg.Spec.Managed

	switch {
	case !wasManaged && nowManaged:
		// false→true: deploy registry
		if err := p.deployManagedRegistry(ctx, reg); err != nil {
			p.deps.Logger.Warnw("auto-deploy registry on managed transition failed",
				"registry", reg.Name, "error", err)
		}
	case wasManaged && !nowManaged:
		// true→false: teardown registry
		if err := p.teardownManagedRegistry(ctx, reg); err != nil {
			p.deps.Logger.Warnw("teardown registry on unmanaged transition failed",
				"registry", reg.Name, "error", err)
		}
	case wasManaged && nowManaged:
		// stays managed: update ConfigMap if config changed
		cfgYAML := p.generateRegistryConfigYAML(reg)
		cmName := "registry-" + reg.Name + "-config"
		cmKey := reg.Spec.Network + "/" + cmName
		if cm, ok := p.configMaps[cmKey]; ok {
			if cm.Data["config.yaml"] != cfgYAML {
				cm.Data["config.yaml"] = cfgYAML
				if p.deps.Store != nil && p.deps.Store.ConfigMaps != nil {
					storeKey := reg.Spec.Network + "." + cmName
					_, _ = p.deps.Store.ConfigMaps.PutJSON(ctx, storeKey, cm)
				}
				p.syncConfigMapsToDisk(ctx)
				p.deps.Logger.Infow("updated managed registry ConfigMap", "registry", reg.Name)
			}
		}
	}
}

// ─── Consistency Checks ─────────────────────────────────────────────────────

// checkRegistryCRDs verifies Registry CRD consistency between memory and NATS,
// and checks liveness for each registry.
func (p *MicroKubeProvider) checkRegistryCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.Registries != nil {
		storeKeys, err := p.deps.Store.Registries.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.registries {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("registry-crd/%s", name),
						Status:  "pass",
						Message: "registry CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("registry-crd/%s", name),
						Status:  "fail",
						Message: "registry CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("registry-crd/%s", name),
					Status:  "warn",
					Message: "registry CRD in NATS but not in memory",
				})
			}
		}
	}

	// Liveness per registry
	for _, reg := range p.registries {
		if reg.Spec.StaticIP == "" {
			continue
		}
		port := "5000"
		if reg.Spec.ListenAddr != "" && len(reg.Spec.ListenAddr) > 1 && reg.Spec.ListenAddr[0] == ':' {
			port = reg.Spec.ListenAddr[1:]
		}
		alive := probeHTTP(reg.Spec.StaticIP, port, "/v2/", 3*time.Second)
		if alive {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("registry-health/%s", reg.Name),
				Status:  "pass",
				Message: fmt.Sprintf("registry alive at %s:%s", reg.Spec.StaticIP, port),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("registry-health/%s", reg.Name),
				Status:  "fail",
				Message: fmt.Sprintf("registry not responding at %s:%s", reg.Spec.StaticIP, port),
			})
		}
	}

	return items
}
