package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleCreateConfigMap creates or replaces a ConfigMap.
func (p *MicroKubeProvider) handleCreateConfigMap(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var cm corev1.ConfigMap
	if err := json.NewDecoder(r.Body).Decode(&cm); err != nil {
		http.Error(w, fmt.Sprintf("invalid configmap JSON: %v", err), http.StatusBadRequest)
		return
	}
	cm.Namespace = ns
	if cm.Name == "" {
		http.Error(w, "configmap name is required", http.StatusBadRequest)
		return
	}

	key := ns + "/" + cm.Name
	cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

	// Persist to NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + cm.Name
		if _, err := p.deps.Store.ConfigMaps.PutJSON(r.Context(), storeKey, &cm); err != nil {
			http.Error(w, fmt.Sprintf("persisting configmap: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.configMaps[key] = &cm

	// Sync to disk and trigger pod updates if content changed
	p.syncConfigMapsToDisk(r.Context())

	podWriteJSON(w, http.StatusCreated, &cm)
}

// handleGetConfigMap returns a ConfigMap by name.
func (p *MicroKubeProvider) handleGetConfigMap(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	cm, ok := p.configMaps[key]
	if !ok {
		http.Error(w, fmt.Sprintf("configmap %s not found", key), http.StatusNotFound)
		return
	}

	enriched := cm.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	podWriteJSON(w, http.StatusOK, enriched)
}

// handleListConfigMaps returns all ConfigMaps in a namespace.
func (p *MicroKubeProvider) handleListConfigMaps(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchConfigMaps(w, r, ns)
		return
	}

	items := make([]corev1.ConfigMap, 0)
	for _, cm := range p.configMaps {
		if cm.Namespace != ns {
			continue
		}
		enriched := cm.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.ConfigMapList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"},
		Items:    items,
	})
}

// handleDeleteConfigMap removes a ConfigMap.
func (p *MicroKubeProvider) handleDeleteConfigMap(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.configMaps[key]; !ok {
		http.Error(w, fmt.Sprintf("configmap %s not found", key), http.StatusNotFound)
		return
	}

	delete(p.configMaps, key)

	// Remove from NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.ConfigMaps.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete configmap from store", "key", storeKey, "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("configmap %q deleted", name),
	})
}

// LoadConfigMapsFromStore loads ConfigMap objects from the NATS CONFIGMAPS bucket.
func (p *MicroKubeProvider) LoadConfigMapsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.ConfigMaps == nil {
		return
	}

	keys, err := p.deps.Store.ConfigMaps.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list configmaps from store", "error", err)
		return
	}

	loaded := 0
	for _, key := range keys {
		var cm corev1.ConfigMap
		if _, err := p.deps.Store.ConfigMaps.GetJSON(ctx, key, &cm); err != nil {
			p.deps.Logger.Warnw("failed to read configmap from store", "key", key, "error", err)
			continue
		}
		mapKey := cm.Namespace + "/" + cm.Name
		p.configMaps[mapKey] = &cm
		loaded++
	}

	if loaded > 0 {
		p.deps.Logger.Infow("loaded configmaps from store", "count", loaded)
	}
}

// resolveConfigMapVolume looks up ConfigMap data for a pod's named volume.
// Returns the ConfigMap's Data map, or nil if no ConfigMap is associated.
func (p *MicroKubeProvider) resolveConfigMapVolume(pod *corev1.Pod, volumeName string) map[string]string {
	for _, v := range pod.Spec.Volumes {
		if v.Name != volumeName {
			continue
		}
		if v.ConfigMap == nil {
			return nil
		}
		key := pod.Namespace + "/" + v.ConfigMap.Name
		p.mu.RLock()
		cm, ok := p.configMaps[key]
		p.mu.RUnlock()
		if ok {
			return cm.Data
		}
		return nil
	}
	return nil
}
