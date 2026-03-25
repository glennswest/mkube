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
// If the pod spec references a PVC that doesn't exist yet, auto-creates it to prevent
// data loss from falling through to ephemeral volumes.
// Also ensures the PVC directory exists on disk via the container runtime.
func (p *MicroKubeProvider) resolvePVCVolume(ctx context.Context, pod *corev1.Pod, volumeName string) (string, bool) {
	for _, v := range pod.Spec.Volumes {
		if v.Name != volumeName {
			continue
		}
		if v.PersistentVolumeClaim == nil {
			return "", false
		}
		claimName := v.PersistentVolumeClaim.ClaimName
		key := pod.Namespace + "/" + claimName
		pvc, ok := p.pvcs.Get(key)
		if !ok {
			// Auto-create missing PVC to prevent data loss.
			// This handles boot-order pods that reference PVCs before they're
			// explicitly created by deployManagedDNS or the API.
			p.deps.Logger.Warnw("auto-creating missing PVC referenced by pod",
				"pod", pod.Namespace+"/"+pod.Name,
				"volume", volumeName,
				"claimName", claimName)
			newPVC := &corev1.PersistentVolumeClaim{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              claimName,
					Namespace:         pod.Namespace,
					CreationTimestamp: metav1.Now(),
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimBound,
				},
			}
			p.pvcs.Set(key, newPVC)
			// Persist to NATS if store is available
			if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
				storeKey := pod.Namespace + "." + claimName
				if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, newPVC); err != nil {
					p.deps.Logger.Warnw("failed to persist auto-created PVC", "key", storeKey, "error", err)
				}
			}
			pvc = newPVC
		}
		hostPath := p.pvcHostPath(pvc)
		// Ensure the PVC directory exists on disk
		if err := p.deps.Runtime.EnsureDirectory(ctx, hostPath); err != nil {
			p.deps.Logger.Warnw("failed to ensure PVC directory on disk",
				"path", hostPath, "pvc", key, "error", err)
		}
		return hostPath, true
	}
	return "", false
}

// pvcHostPath returns the host path for a PVC volume.
// Pool selection: annotation vkube.io/storage-pool > storageClassName > default pool.
// Backward compat: existing PVCs without annotation resolve to default "raid1".
func (p *MicroKubeProvider) pvcHostPath(pvc *corev1.PersistentVolumeClaim) string {
	poolName := p.pvcPoolName(pvc)
	// Also check storageClassName
	if poolName == "" && pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		poolName = *pvc.Spec.StorageClassName
	}
	pool := p.resolveStoragePool(poolName)
	return fmt.Sprintf("%s/%s_%s", pool.volumesPath(), pvc.Namespace, pvc.Name)
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
		// Migration: PVCs stored without namespace (pre-fix) are stale.
		// Delete from NATS and skip — they'll be recreated with correct keys.
		if pvc.Namespace == "" {
			p.deps.Logger.Infow("deleting stale PVC with empty namespace", "key", key, "name", pvc.Name)
			_ = p.deps.Store.PersistentVolumeClaims.Delete(ctx, key)
			continue
		}
		mapKey := pvc.Namespace + "/" + pvc.Name
		p.pvcs.Set(mapKey, &pvc)
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
	if p.pvcs.Has(key) {
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

	p.pvcs.Set(key, &pvc)

	// Branch on PVC type
	if pvcType(&pvc) == PVCTypeFile {
		if err := p.createFileBackedPVC(r.Context(), &pvc); err != nil {
			// Rollback in-memory and store
			p.pvcs.Delete(key)
			if p.deps.Store != nil {
				_ = p.deps.Store.PersistentVolumeClaims.Delete(r.Context(), ns+"."+pvc.Name)
			}
			http.Error(w, fmt.Sprintf("creating file-backed PVC: %v", err), http.StatusInternalServerError)
			return
		}
		// Re-persist with updated annotations
		if p.deps.Store != nil {
			storeKey := ns + "." + pvc.Name
			if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(r.Context(), storeKey, &pvc); err != nil {
				p.deps.Logger.Warnw("failed to persist file-backed PVC annotations", "key", storeKey, "error", err)
			}
		}
		p.pvcs.Set(key, &pvc)
	} else {
		// Ensure the PVC directory exists on disk (directory type)
		hostPath := p.pvcHostPath(&pvc)
		if err := p.deps.Runtime.EnsureDirectory(r.Context(), hostPath); err != nil {
			p.deps.Logger.Warnw("failed to ensure PVC directory on disk",
				"path", hostPath, "pvc", key, "error", err)
		}
	}

	podWriteJSON(w, http.StatusCreated, &pvc)
}

// handleGetPVC returns a PVC by name.
func (p *MicroKubeProvider) handleGetPVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs.Get(key)
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
	p.enrichPVCUsage(r.Context(), enriched)
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
	for _, pvc := range p.pvcs.Snapshot() {
		if pvc.Namespace != ns {
			continue
		}
		enriched := pvc.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		items = append(items, *enriched)
	}

	// Single /file fetch for all PVCs instead of one per PVC
	p.enrichPVCUsageBatch(r.Context(), items)

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

	snap := p.pvcs.Snapshot()
	items := make([]corev1.PersistentVolumeClaim, 0, len(snap))
	for _, pvc := range snap {
		enriched := pvc.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}
		items = append(items, *enriched)
	}

	// Single /file fetch for all PVCs instead of one per PVC
	p.enrichPVCUsageBatch(r.Context(), items)

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

	if !p.pvcs.Has(key) {
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

	p.pvcs.Set(key, &pvc)
	podWriteJSON(w, http.StatusOK, &pvc)
}

// handleDeletePVC removes a PVC.
// Returns 409 Conflict if any pod currently references it.
// Use ?purge=true to also remove the on-disk directory.
func (p *MicroKubeProvider) handleDeletePVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	// Check if any pod references this PVC
	for _, pod := range p.pods.Snapshot() {
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

	p.pvcs.Delete(key)

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
		// Also remove the image file for file-backed PVCs
		if pvcType(pvc) == PVCTypeFile {
			p.deleteFileBackedPVC(pvc)
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

	// Send existing PVCs as ADDED events (snapshot)
	enc := json.NewEncoder(w)
	pvcSnapshot := make([]*corev1.PersistentVolumeClaim, 0)
	for _, pvc := range p.pvcs.Snapshot() {
		if nsFilter != "" && pvc.Namespace != nsFilter {
			continue
		}
		pvcSnapshot = append(pvcSnapshot, pvc.DeepCopy())
	}
	for _, enriched := range pvcSnapshot {
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

// fixOrphanedVolumeMounts detects volumeMounts without a matching volume definition
// and auto-adds a PVC volume reference. Returns true if the pod was modified.
// This fixes pods created from boot-order.yaml that had "data" volumeMounts
// but no corresponding PVC volume definition, causing data loss on recreation.
func (p *MicroKubeProvider) fixOrphanedVolumeMounts(pod *corev1.Pod, ctx context.Context) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}

	// Build set of existing volume names
	volumeNames := make(map[string]bool, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumeNames[v.Name] = true
	}

	modified := false
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if volumeNames[vm.Name] {
				continue
			}
			// Orphaned volumeMount — the "data" volume for DNS pods
			// needs a PVC to persist redb across pod recreation.
			// Also handles registry pods with data at /raid1/registry/*.
			if vm.Name == "data" && vm.MountPath == "/data" {
				claimName := pod.Namespace + "-dns-data"
				p.deps.Logger.Infow("fixing orphaned volumeMount: adding PVC volume",
					"pod", pod.Namespace+"/"+pod.Name,
					"volume", vm.Name,
					"claimName", claimName)
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
						},
					},
				})
				volumeNames["data"] = true
				modified = true
			} else if vm.Name == "data" && strings.HasPrefix(pod.Name, "registry-") {
				// Registry pod data volume — derive PVC name from pod name.
				claimName := pod.Name + "-data"
				p.deps.Logger.Infow("fixing orphaned volumeMount: adding PVC volume for registry",
					"pod", pod.Namespace+"/"+pod.Name,
					"volume", vm.Name,
					"mountPath", vm.MountPath,
					"claimName", claimName)
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
						},
					},
				})
				volumeNames["data"] = true
				modified = true
			} else {
				p.deps.Logger.Warnw("orphaned volumeMount has no matching volume definition",
					"pod", pod.Namespace+"/"+pod.Name,
					"volumeMount", vm.Name,
					"mountPath", vm.MountPath)
			}
		}
	}
	return modified
}

// enrichPVCUsage adds a vkube.io/used-bytes annotation with actual disk usage.
// For single-PVC gets only; list handlers should use enrichPVCUsageBatch.
func (p *MicroKubeProvider) enrichPVCUsage(ctx context.Context, pvc *corev1.PersistentVolumeClaim) {
	hostPath := p.pvcHostPath(pvc)
	used, err := p.deps.Runtime.DirectoryDiskUsage(ctx, hostPath)
	if err != nil {
		return
	}
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations["vkube.io/used-bytes"] = fmt.Sprintf("%d", used)
	enrichFileBackedPVCUsage(pvc)
}

// enrichPVCUsageBatch enriches all PVCs with disk usage in a single RouterOS
// /file fetch instead of one per PVC.
func (p *MicroKubeProvider) enrichPVCUsageBatch(ctx context.Context, pvcs []corev1.PersistentVolumeClaim) {
	rc := p.getRouterOSClient()
	if rc == nil {
		return
	}
	idx, err := rc.FetchFileUsageIndex(ctx)
	if err != nil {
		p.deps.Logger.Debugw("failed to fetch file usage index for PVC enrichment", "error", err)
		return
	}
	for i := range pvcs {
		hostPath := p.pvcHostPath(&pvcs[i])
		used := idx.DirectoryUsage(hostPath)
		if used > 0 {
			if pvcs[i].Annotations == nil {
				pvcs[i].Annotations = make(map[string]string)
			}
			pvcs[i].Annotations["vkube.io/used-bytes"] = fmt.Sprintf("%d", used)
		}
		enrichFileBackedPVCUsage(&pvcs[i])
	}
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
