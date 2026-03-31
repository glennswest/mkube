package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/store"
)

// handleCreateSecret creates or replaces a Secret.
func (p *MicroKubeProvider) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var secret corev1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		http.Error(w, fmt.Sprintf("invalid secret JSON: %v", err), http.StatusBadRequest)
		return
	}
	secret.Namespace = ns
	if secret.Name == "" {
		http.Error(w, "secret name is required", http.StatusBadRequest)
		return
	}

	// Merge StringData into Data (standard k8s behavior)
	mergeStringData(&secret)

	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	if secret.Type == "" {
		secret.Type = corev1.SecretTypeOpaque
	}

	key := ns + "/" + secret.Name

	// Persist encrypted to NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + secret.Name
		if _, err := p.deps.Store.PutSecretJSON(r.Context(), storeKey, &secret); err != nil {
			http.Error(w, fmt.Sprintf("persisting secret: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.secrets.Set(key, &secret)

	// Sync to disk and trigger pod updates if content changed
	p.syncSecretsToDisk(r.Context())

	podWriteJSON(w, http.StatusCreated, &secret)
}

// handleGetSecret returns a Secret by name (full data included).
func (p *MicroKubeProvider) handleGetSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	secret, ok := p.secrets.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("secret %s not found", key), http.StatusNotFound)
		return
	}

	enriched := secret.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	podWriteJSON(w, http.StatusOK, enriched)
}

// handleListSecrets returns all Secrets in a namespace with Data redacted.
func (p *MicroKubeProvider) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchSecrets(w, r, ns)
		return
	}

	items := make([]corev1.Secret, 0)
	for _, secret := range p.secrets.Snapshot() {
		if secret.Namespace != ns {
			continue
		}
		enriched := secret.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
		// Redact data in list responses
		enriched.Data = nil
		enriched.StringData = nil
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.SecretList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"},
		Items:    items,
	})
}

// handleUpdateSecret replaces a Secret (PUT).
func (p *MicroKubeProvider) handleUpdateSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if !p.secrets.Has(key) {
		http.Error(w, fmt.Sprintf("secret %s not found", key), http.StatusNotFound)
		return
	}

	var secret corev1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		http.Error(w, fmt.Sprintf("invalid secret JSON: %v", err), http.StatusBadRequest)
		return
	}
	secret.Namespace = ns
	secret.Name = name
	mergeStringData(&secret)
	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PutSecretJSON(r.Context(), storeKey, &secret); err != nil {
			http.Error(w, fmt.Sprintf("persisting secret: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.secrets.Set(key, &secret)
	p.syncSecretsToDisk(r.Context())

	podWriteJSON(w, http.StatusOK, &secret)
}

// handlePatchSecret applies a merge patch to a Secret.
func (p *MicroKubeProvider) handlePatchSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.secrets.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("secret %s not found", key), http.StatusNotFound)
		return
	}

	merged := existing.DeepCopy()
	if err := json.NewDecoder(r.Body).Decode(merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}
	merged.Namespace = ns
	merged.Name = name
	mergeStringData(merged)
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PutSecretJSON(r.Context(), storeKey, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting secret patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.secrets.Set(key, merged)
	p.syncSecretsToDisk(r.Context())

	podWriteJSON(w, http.StatusOK, merged)
}

// handleDeleteSecret removes a Secret.
func (p *MicroKubeProvider) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if !p.secrets.Has(key) {
		http.Error(w, fmt.Sprintf("secret %s not found", key), http.StatusNotFound)
		return
	}

	p.secrets.Delete(key)

	// Remove from NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.Secrets.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete secret from store", "key", storeKey, "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("secret %q deleted", name),
	})
}

// LoadSecretsFromStore loads Secret objects from the NATS SECRETS bucket (encrypted).
func (p *MicroKubeProvider) LoadSecretsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Secrets == nil {
		return
	}

	keys, err := p.deps.Store.Secrets.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list secrets from store", "error", err)
		return
	}

	loaded := 0
	for _, key := range keys {
		var secret corev1.Secret
		if _, err := p.deps.Store.GetSecretJSON(ctx, key, &secret); err != nil {
			p.deps.Logger.Warnw("failed to read secret from store", "key", key, "error", err)
			continue
		}
		mapKey := secret.Namespace + "/" + secret.Name
		p.secrets.Set(mapKey, &secret)
		loaded++
	}

	if loaded > 0 {
		p.deps.Logger.Infow("loaded secrets from store", "count", loaded)
	}
}

// resolveSecretVolume looks up Secret data for a pod's named volume.
// Returns a map of filename → decoded content, or nil if no Secret is associated.
func (p *MicroKubeProvider) resolveSecretVolume(pod *corev1.Pod, volumeName string) map[string]string {
	for _, v := range pod.Spec.Volumes {
		if v.Name != volumeName {
			continue
		}
		if v.Secret == nil {
			return nil
		}
		key := pod.Namespace + "/" + v.Secret.SecretName
		secret, ok := p.secrets.Get(key)
		if !ok {
			return nil
		}
		result := make(map[string]string, len(secret.Data))
		for k, v := range secret.Data {
			result[k] = string(v)
		}
		return result
	}
	return nil
}

// syncSecretsToDisk writes all Secret-backed volume data to disk for every
// tracked pod. Compares content against what is on disk and triggers a rolling
// update for pods whose Secret files are stale or missing.
func (p *MicroKubeProvider) syncSecretsToDisk(ctx context.Context) {
	log := p.deps.Logger

	podsToRecreate := make(map[string]*corev1.Pod)
	podsSnap := p.pods.Values()

	for _, pod := range podsSnap {
		for _, container := range pod.Spec.Containers {
			name := sanitizeName(pod, container.Name)
			for _, vm := range container.VolumeMounts {
				data := p.resolveSecretVolume(pod, vm.Name)
				if data == nil {
					continue
				}

				localDir := fmt.Sprintf("/data/secrets/%s/%s", name, vm.Name)
				if mkErr := os.MkdirAll(localDir, 0o700); mkErr != nil {
					log.Warnw("failed to create secret dir", "path", localDir, "error", mkErr)
					continue
				}

				for filename, content := range data {
					filePath := localDir + "/" + filename
					existing, readErr := os.ReadFile(filePath)
					if readErr != nil || string(existing) != content {
						if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
							log.Warnw("failed to write secret file", "path", filePath, "error", err)
							continue
						}
						key := podKey(pod)
						if _, already := podsToRecreate[key]; !already {
							log.Infow("Secret file updated on disk",
								"pod", key,
								"file", filePath,
								"new", readErr != nil,
							)
							podsToRecreate[key] = pod
						}
					}
				}
			}
		}
	}

	for key, pod := range podsToRecreate {
		imagesMissing := false
		if pod.Annotations[annotationFile] == "" {
			for _, c := range pod.Spec.Containers {
				if _, err := p.deps.StorageMgr.CheckImageAvailable(ctx, c.Image); err != nil {
					log.Warnw("skipping Secret recreation: image not available",
						"pod", key, "image", c.Image, "error", err)
					imagesMissing = true
					break
				}
			}
		}
		if imagesMissing {
			continue
		}

		log.Infow("recreating pod for Secret update", "pod", key)
		if err := p.UpdatePod(ctx, pod); err != nil {
			log.Errorw("failed to recreate pod after Secret change", "pod", key, "error", err)
		}
	}
}

// resolveContainerEnv resolves all environment variable sources for a container
// in standard Kubernetes order:
// 1. EnvFrom[].SecretRef — all keys from referenced Secret
// 2. EnvFrom[].ConfigMapRef — all keys from referenced ConfigMap
// 3. Env[].Value — plain values
// 4. Env[].ValueFrom.SecretKeyRef — single key from Secret
// 5. Env[].ValueFrom.ConfigMapKeyRef — single key from ConfigMap
// Returns a slice of "KEY=VALUE" strings.
func (p *MicroKubeProvider) resolveContainerEnv(pod *corev1.Pod, container *corev1.Container) []string {
	envMap := make(map[string]string)

	// 1 & 2: EnvFrom
	for _, ef := range container.EnvFrom {
		if ef.SecretRef != nil {
			key := pod.Namespace + "/" + ef.SecretRef.Name
			if secret, ok := p.secrets.Get(key); ok {
				for k, v := range secret.Data {
					envMap[ef.Prefix+k] = string(v)
				}
			}
		}
		if ef.ConfigMapRef != nil {
			key := pod.Namespace + "/" + ef.ConfigMapRef.Name
			if cm, ok := p.configMaps.Get(key); ok {
				for k, v := range cm.Data {
					envMap[ef.Prefix+k] = v
				}
			}
		}
	}

	// 3, 4, 5: Env (later entries override earlier, including EnvFrom)
	for _, e := range container.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
			continue
		}
		if e.ValueFrom != nil {
			if e.ValueFrom.SecretKeyRef != nil {
				ref := e.ValueFrom.SecretKeyRef
				key := pod.Namespace + "/" + ref.Name
				if secret, ok := p.secrets.Get(key); ok {
					if v, found := secret.Data[ref.Key]; found {
						envMap[e.Name] = string(v)
					}
				}
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				ref := e.ValueFrom.ConfigMapKeyRef
				key := pod.Namespace + "/" + ref.Name
				if cm, ok := p.configMaps.Get(key); ok {
					if v, found := cm.Data[ref.Key]; found {
						envMap[e.Name] = v
					}
				}
			}
		}
	}

	if len(envMap) == 0 {
		return nil
	}

	// Sort keys for deterministic output (avoid flapping)
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+envMap[k])
	}
	return result
}

// handleWatchSecrets streams secret changes via SSE-style chunked responses.
func (p *MicroKubeProvider) handleWatchSecrets(w http.ResponseWriter, r *http.Request, nsFilter string) {
	if p.deps.Store == nil {
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
	enc := json.NewEncoder(w)

	events, err := p.deps.Store.Secrets.WatchAll(ctx)
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

			var secret corev1.Secret
			if evt.Type == store.EventDelete {
				ns, name := parseStoreKey(evt.Key)
				if nsFilter != "" && ns != nsFilter {
					continue
				}
				secret = corev1.Secret{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				}
			} else {
				// Decrypt the value from the store
				if _, err := p.deps.Store.GetSecretJSON(ctx, evt.Key, &secret); err != nil {
					continue
				}
				if nsFilter != "" && secret.Namespace != nsFilter {
					continue
				}
				secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
				// Redact data in watch events
				secret.Data = nil
				secret.StringData = nil
			}

			watchEvt := map[string]interface{}{
				"type":   strings.ToUpper(string(evt.Type)),
				"object": &secret,
			}
			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// mergeStringData folds StringData values into Data (base64-encoding them)
// and clears StringData, matching standard Kubernetes API server behavior.
func mergeStringData(secret *corev1.Secret) {
	if len(secret.StringData) == 0 {
		return
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, len(secret.StringData))
	}
	for k, v := range secret.StringData {
		secret.Data[k] = []byte(v)
	}
	secret.StringData = nil
}

