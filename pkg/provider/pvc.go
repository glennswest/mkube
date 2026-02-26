package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// resolvePVCVolume checks if a pod volume mount is backed by a PVC.
// Returns the host path for the PVC volume and true if found, or empty string and false.
func (p *MicroKubeProvider) resolvePVCVolume(pod *corev1.Pod, volumeName string) (string, bool) {
	for _, v := range pod.Spec.Volumes {
		if v.Name != volumeName {
			continue
		}
		if v.PersistentVolumeClaim == nil {
			return "", false
		}
		key := pod.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
		pvc, ok := p.pvcs[key]
		if !ok {
			return "", false
		}
		return p.pvcHostPath(pvc), true
	}
	return "", false
}

// pvcHostPath returns the host path for a PVC volume.
// PVC volumes live under /raid1/volumes/pvc/{namespace}_{claimName}
func (p *MicroKubeProvider) pvcHostPath(pvc *corev1.PersistentVolumeClaim) string {
	volumesBase := strings.TrimSuffix(p.deps.Config.Storage.BasePath, "/images") + "/volumes/pvc"
	return fmt.Sprintf("%s/%s_%s", volumesBase, pvc.Namespace, pvc.Name)
}

// LoadPVCsFromStore loads PVC objects from NATS store into the in-memory map.
func (p *MicroKubeProvider) LoadPVCsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.PersistentVolumeClaims == nil {
		return
	}

	keys, err := p.deps.Store.PersistentVolumeClaims.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list PVCs from store", "error", err)
		return
	}

	for _, key := range keys {
		var pvc corev1.PersistentVolumeClaim
		if _, err := p.deps.Store.PersistentVolumeClaims.GetJSON(ctx, key, &pvc); err != nil {
			p.deps.Logger.Warnw("failed to read PVC from store", "key", key, "error", err)
			continue
		}
		mapKey := pvc.Namespace + "/" + pvc.Name
		p.pvcs[mapKey] = &pvc
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded PVCs from store", "count", len(keys))
	}
}

// handleCreatePVC creates a PersistentVolumeClaim.
func (p *MicroKubeProvider) handleCreatePVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var pvc corev1.PersistentVolumeClaim
	if err := json.NewDecoder(r.Body).Decode(&pvc); err != nil {
		http.Error(w, fmt.Sprintf("invalid PVC JSON: %v", err), http.StatusBadRequest)
		return
	}
	pvc.Namespace = ns
	if pvc.Name == "" {
		http.Error(w, "PVC name is required", http.StatusBadRequest)
		return
	}

	key := ns + "/" + pvc.Name
	if _, exists := p.pvcs[key]; exists {
		http.Error(w, fmt.Sprintf("PVC %s already exists", key), http.StatusConflict)
		return
	}

	pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
	if pvc.CreationTimestamp.IsZero() {
		pvc.CreationTimestamp = metav1.Now()
	}

	// Set status to Bound (single-node, no dynamic provisioning)
	pvc.Status = corev1.PersistentVolumeClaimStatus{
		Phase:       corev1.ClaimBound,
		AccessModes: pvc.Spec.AccessModes,
		Capacity:    pvc.Spec.Resources.Requests,
	}

	// Persist to NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + pvc.Name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(r.Context(), storeKey, &pvc); err != nil {
			http.Error(w, fmt.Sprintf("persisting PVC: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.pvcs[key] = &pvc
	podWriteJSON(w, http.StatusCreated, &pvc)
}

// handleGetPVC returns a PVC by name.
func (p *MicroKubeProvider) handleGetPVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	if r.Header.Get("Accept") == "application/json;as=Table;v=v1;g=meta.k8s.io" {
		podWriteJSON(w, http.StatusOK, pvcListToTable([]corev1.PersistentVolumeClaim{*pvc}))
		return
	}

	enriched := pvc.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
	podWriteJSON(w, http.StatusOK, enriched)
}

// handleListPVCs returns all PVCs in a namespace.
func (p *MicroKubeProvider) handleListPVCs(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchPVCs(w, r, ns)
		return
	}

	items := make([]corev1.PersistentVolumeClaim, 0)
	for _, pvc := range p.pvcs {
		if pvc.Namespace != ns {
			continue
		}
		enriched := pvc.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		items = append(items, *enriched)
	}

	if r.Header.Get("Accept") == "application/json;as=Table;v=v1;g=meta.k8s.io" {
		podWriteJSON(w, http.StatusOK, pvcListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, corev1.PersistentVolumeClaimList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"},
		Items:    items,
	})
}

// handleListAllPVCs returns all PVCs across all namespaces.
func (p *MicroKubeProvider) handleListAllPVCs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchPVCs(w, r, "")
		return
	}

	items := make([]corev1.PersistentVolumeClaim, 0, len(p.pvcs))
	for _, pvc := range p.pvcs {
		enriched := pvc.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		items = append(items, *enriched)
	}

	if r.Header.Get("Accept") == "application/json;as=Table;v=v1;g=meta.k8s.io" {
		podWriteJSON(w, http.StatusOK, pvcListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, corev1.PersistentVolumeClaimList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"},
		Items:    items,
	})
}

// handleUpdatePVC replaces a PVC (PUT).
func (p *MicroKubeProvider) handleUpdatePVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.pvcs[key]; !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	var pvc corev1.PersistentVolumeClaim
	if err := json.NewDecoder(r.Body).Decode(&pvc); err != nil {
		http.Error(w, fmt.Sprintf("invalid PVC JSON: %v", err), http.StatusBadRequest)
		return
	}
	pvc.Namespace = ns
	pvc.Name = name
	pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}

	// Maintain Bound status
	pvc.Status = corev1.PersistentVolumeClaimStatus{
		Phase:       corev1.ClaimBound,
		AccessModes: pvc.Spec.AccessModes,
		Capacity:    pvc.Spec.Resources.Requests,
	}

	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(r.Context(), storeKey, &pvc); err != nil {
			http.Error(w, fmt.Sprintf("persisting PVC: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.pvcs[key] = &pvc
	podWriteJSON(w, http.StatusOK, &pvc)
}

// handleDeletePVC removes a PVC.
// Returns 409 Conflict if any pod currently references it.
// Use ?purge=true to also remove the on-disk directory.
func (p *MicroKubeProvider) handleDeletePVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	// Check if any pod references this PVC
	for _, pod := range p.pods {
		if pod.Namespace != ns {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == name {
				http.Error(w, fmt.Sprintf("PVC %s is in use by pod %s/%s", key, pod.Namespace, pod.Name), http.StatusConflict)
				return
			}
		}
	}

	delete(p.pvcs, key)

	// Remove from NATS store
	if p.deps.Store != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.PersistentVolumeClaims.Delete(r.Context(), storeKey); err != nil {
			p.deps.Logger.Warnw("failed to delete PVC from store", "key", storeKey, "error", err)
		}
	}

	// Optionally purge the on-disk directory
	if r.URL.Query().Get("purge") == "true" {
		hostPath := p.pvcHostPath(pvc)
		if err := p.deps.Runtime.RemoveFile(r.Context(), hostPath); err != nil {
			p.deps.Logger.Warnw("failed to purge PVC directory", "path", hostPath, "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("persistentvolumeclaim %q deleted", name),
	})
}

// handleWatchPVCs streams PVC events as newline-delimited JSON (K8s watch format).
func (p *MicroKubeProvider) handleWatchPVCs(w http.ResponseWriter, r *http.Request, nsFilter string) {
	if p.deps.Store == nil || p.deps.Store.PersistentVolumeClaims == nil {
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

	// Send existing PVCs as ADDED events
	enc := json.NewEncoder(w)
	for _, pvc := range p.pvcs {
		if nsFilter != "" && pvc.Namespace != nsFilter {
			continue
		}
		enriched := pvc.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		evt := K8sWatchEvent{Type: "ADDED", Object: enriched}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch for changes via NATS
	events, err := p.deps.Store.PersistentVolumeClaims.WatchAll(ctx)
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

			var pvc corev1.PersistentVolumeClaim
			if evt.Type == store.EventDelete {
				ns, name := parseStoreKey(evt.Key)
				if nsFilter != "" && ns != nsFilter {
					continue
				}
				pvc = corev1.PersistentVolumeClaim{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &pvc); err != nil {
					continue
				}
				if nsFilter != "" && pvc.Namespace != nsFilter {
					continue
				}
				pvc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &pvc,
			}
			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// pvcListToTable converts PVCs to a Table for `oc get pvc` / `kubectl get pvc`.
func pvcListToTable(pvcs []corev1.PersistentVolumeClaim) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Status", Type: "string"},
			{Name: "Volume", Type: "string"},
			{Name: "Capacity", Type: "string"},
			{Name: "Access Modes", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range pvcs {
		pvc := &pvcs[i]

		status := string(pvc.Status.Phase)
		if status == "" {
			status = "Bound"
		}

		volume := fmt.Sprintf("pvc-%s", pvc.Name)

		capacity := ""
		if stor, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = stor.String()
		} else if stor, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			capacity = stor.String()
		}

		accessModes := formatAccessModes(pvc.Spec.AccessModes)

		age := "<unknown>"
		if !pvc.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(pvc.CreationTimestamp.Time))
		}

		partialMeta := map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              pvc.Name,
				"namespace":         pvc.Namespace,
				"creationTimestamp": pvc.CreationTimestamp.Format(time.RFC3339),
			},
		}
		raw, _ := json.Marshal(partialMeta)
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				pvc.Name,
				status,
				volume,
				capacity,
				accessModes,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// formatAccessModes returns a short string representation of access modes.
func formatAccessModes(modes []corev1.PersistentVolumeAccessMode) string {
	abbrevs := make([]string, 0, len(modes))
	for _, m := range modes {
		switch m {
		case corev1.ReadWriteOnce:
			abbrevs = append(abbrevs, "RWO")
		case corev1.ReadOnlyMany:
			abbrevs = append(abbrevs, "ROX")
		case corev1.ReadWriteMany:
			abbrevs = append(abbrevs, "RWX")
		case corev1.ReadWriteOncePod:
			abbrevs = append(abbrevs, "RWOP")
		default:
			abbrevs = append(abbrevs, string(m))
		}
	}
	return strings.Join(abbrevs, ",")
}
