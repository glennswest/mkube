package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PVC type annotation values.
const (
	PVCTypeDirectory = "directory"
	PVCTypeFile      = "file-backed"

	annPVCType     = "vkube.io/pvc-type"
	annPVCCapacity = "vkube.io/pvc-capacity"
	annPVCImage    = "vkube.io/image-path"
)

// pvcType returns the PVC type from annotations, defaulting to directory.
func pvcType(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Annotations != nil {
		if t := pvc.Annotations[annPVCType]; t == PVCTypeFile {
			return PVCTypeFile
		}
	}
	return PVCTypeDirectory
}

// pvcImagePath returns the sparse image file path for a file-backed PVC.
func (p *MicroKubeProvider) pvcImagePath(pvc *corev1.PersistentVolumeClaim) string {
	poolName := p.pvcPoolName(pvc)
	if poolName == "" && pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		poolName = *pvc.Spec.StorageClassName
	}
	pool := p.resolveStoragePool(poolName)
	return fmt.Sprintf("%s/%s_%s.img", pool.volumesPath(), pvc.Namespace, pvc.Name)
}

// createFileBackedPVC creates the sparse image file and directory for a file-backed PVC.
func (p *MicroKubeProvider) createFileBackedPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	sizeBytes, err := pvcRequestedBytes(pvc)
	if err != nil {
		return fmt.Errorf("parsing PVC size: %w", err)
	}
	if sizeBytes <= 0 {
		return fmt.Errorf("file-backed PVC requires a positive storage size")
	}

	imgPath := p.pvcImagePath(pvc)
	dirPath := p.pvcHostPath(pvc)

	// Ensure the directory exists
	if err := p.deps.Runtime.EnsureDirectory(ctx, dirPath); err != nil {
		return fmt.Errorf("creating PVC directory: %w", err)
	}

	// Create sparse image file (capacity marker)
	f, err := os.Create(imgPath)
	if err != nil {
		return fmt.Errorf("creating image file: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		os.Remove(imgPath)
		return fmt.Errorf("setting image size: %w", err)
	}
	f.Close()

	// Set annotations
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations[annPVCType] = PVCTypeFile
	pvc.Annotations[annPVCCapacity] = strconv.FormatInt(sizeBytes, 10)
	pvc.Annotations[annPVCImage] = imgPath

	p.deps.Logger.Infow("created file-backed PVC",
		"pvc", pvc.Namespace+"/"+pvc.Name,
		"capacity", sizeBytes,
		"image", imgPath)
	return nil
}

// deleteFileBackedPVC removes the sparse image file for a file-backed PVC.
func (p *MicroKubeProvider) deleteFileBackedPVC(pvc *corev1.PersistentVolumeClaim) {
	imgPath := p.pvcImagePath(pvc)
	if err := os.Remove(imgPath); err != nil && !os.IsNotExist(err) {
		p.deps.Logger.Warnw("failed to remove PVC image file", "path", imgPath, "error", err)
	}
}

// resizeFileBackedPVC updates the sparse image file to a new capacity.
func (p *MicroKubeProvider) resizeFileBackedPVC(pvc *corev1.PersistentVolumeClaim, newSizeBytes int64) error {
	imgPath := p.pvcImagePath(pvc)
	f, err := os.OpenFile(imgPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening image file: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(newSizeBytes); err != nil {
		return fmt.Errorf("resizing image file: %w", err)
	}
	pvc.Annotations[annPVCCapacity] = strconv.FormatInt(newSizeBytes, 10)
	return nil
}

// fileBackedPVCCapacity returns the capacity in bytes from the sparse image file.
func fileBackedPVCCapacity(pvc *corev1.PersistentVolumeClaim) int64 {
	if pvc.Annotations == nil {
		return 0
	}
	if s := pvc.Annotations[annPVCCapacity]; s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// pvcRequestedBytes extracts the requested storage size in bytes from a PVC.
func pvcRequestedBytes(pvc *corev1.PersistentVolumeClaim) (int64, error) {
	stor, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return 0, fmt.Errorf("no storage request in PVC spec")
	}
	return stor.Value(), nil
}

// handleMigratePVCType handles conversion between PVC types.
// POST /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}/migrate
func (p *MicroKubeProvider) handleMigratePVCType(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	var req struct {
		TargetType string `json:"targetType"` // "file-backed" or "directory"
		Capacity   string `json:"capacity"`   // e.g. "10Gi" — required when converting to file-backed
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	currentType := pvcType(pvc)
	if req.TargetType == currentType {
		http.Error(w, fmt.Sprintf("PVC is already %s", currentType), http.StatusConflict)
		return
	}

	if req.TargetType != PVCTypeDirectory && req.TargetType != PVCTypeFile {
		http.Error(w, fmt.Sprintf("targetType must be %q or %q", PVCTypeDirectory, PVCTypeFile), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	start := time.Now()

	// Stop pods using this PVC
	affectedPods := p.findPodsUsingPVC(ns, name)
	for _, pod := range affectedPods {
		p.deps.Logger.Infow("stopping pod for PVC type migration", "pod", pod.Namespace+"/"+pod.Name)
		if err := p.stopPodContainers(ctx, pod); err != nil {
			p.deps.Logger.Warnw("failed to stop pod", "pod", pod.Namespace+"/"+pod.Name, "error", err)
		}
	}

	switch req.TargetType {
	case PVCTypeFile:
		// directory → file-backed: create sparse image file
		if req.Capacity == "" {
			http.Error(w, "capacity is required when converting to file-backed", http.StatusBadRequest)
			return
		}
		qty, err := resource.ParseQuantity(req.Capacity)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid capacity %q: %v", req.Capacity, err), http.StatusBadRequest)
			return
		}
		sizeBytes := qty.Value()
		if sizeBytes <= 0 {
			http.Error(w, "capacity must be positive", http.StatusBadRequest)
			return
		}

		imgPath := p.pvcImagePath(pvc)
		f, err := os.Create(imgPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("creating image file: %v", err), http.StatusInternalServerError)
			return
		}
		if err := f.Truncate(sizeBytes); err != nil {
			f.Close()
			os.Remove(imgPath)
			http.Error(w, fmt.Sprintf("setting image size: %v", err), http.StatusInternalServerError)
			return
		}
		f.Close()

		if pvc.Annotations == nil {
			pvc.Annotations = make(map[string]string)
		}
		pvc.Annotations[annPVCType] = PVCTypeFile
		pvc.Annotations[annPVCCapacity] = strconv.FormatInt(sizeBytes, 10)
		pvc.Annotations[annPVCImage] = imgPath

		// Update spec capacity to match
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = qty
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: qty}

	case PVCTypeDirectory:
		// file-backed → directory: remove sparse image file
		p.deleteFileBackedPVC(pvc)
		delete(pvc.Annotations, annPVCType)
		delete(pvc.Annotations, annPVCCapacity)
		delete(pvc.Annotations, annPVCImage)
	}

	// Persist updated PVC
	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("failed to persist PVC after type migration", "key", storeKey, "error", err)
		}
	}
	p.pvcs.Set(key, pvc)

	// Restart pods
	p.triggerReconcile()

	duration := time.Since(start)
	p.deps.Logger.Infow("PVC type migrated", "pvc", key, "from", currentType, "to", req.TargetType, "duration", duration)

	podWriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "success",
		"message":    fmt.Sprintf("migrated PVC %s from %s to %s", key, currentType, req.TargetType),
		"durationMs": duration.Milliseconds(),
	})
}

// enrichFileBackedPVCUsage adds capacity and thin ratio annotations for file-backed PVCs.
func enrichFileBackedPVCUsage(pvc *corev1.PersistentVolumeClaim) {
	if pvcType(pvc) != PVCTypeFile {
		return
	}
	capacity := fileBackedPVCCapacity(pvc)
	if capacity <= 0 {
		return
	}
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations[annPVCCapacity] = strconv.FormatInt(capacity, 10)

	// Compute thin ratio from used-bytes (already set by enrichPVCUsage/Batch)
	if usedStr := pvc.Annotations["vkube.io/used-bytes"]; usedStr != "" {
		if used, err := strconv.ParseInt(usedStr, 10, 64); err == nil && capacity > 0 {
			ratio := float64(used) / float64(capacity)
			pvc.Annotations["vkube.io/thin-ratio"] = fmt.Sprintf("%.3f", ratio)
		}
	}
}

// fmtBytesHuman formats bytes into a human-readable string.
func fmtBytesHuman(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// parseSizeAnnotation parses a size string like "10Gi" or "500Mi" into bytes.
func parseSizeAnnotation(s string) (int64, error) {
	s = strings.TrimSpace(s)
	// Try as K8s quantity first
	qty, err := resource.ParseQuantity(s)
	if err == nil {
		return qty.Value(), nil
	}
	// Try as plain integer (bytes)
	return strconv.ParseInt(s, 10, 64)
}

// handleResizePVC resizes a file-backed PVC's capacity.
// POST /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}/resize
func (p *MicroKubeProvider) handleResizePVC(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	pvc, ok := p.pvcs.Get(key)
	if !ok {
		http.Error(w, fmt.Sprintf("PVC %s not found", key), http.StatusNotFound)
		return
	}

	if pvcType(pvc) != PVCTypeFile {
		http.Error(w, "resize is only supported for file-backed PVCs", http.StatusBadRequest)
		return
	}

	var req struct {
		Capacity string `json:"capacity"` // e.g. "20Gi"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	qty, err := resource.ParseQuantity(req.Capacity)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid capacity %q: %v", req.Capacity, err), http.StatusBadRequest)
		return
	}
	newSize := qty.Value()
	if newSize <= 0 {
		http.Error(w, "capacity must be positive", http.StatusBadRequest)
		return
	}

	oldCapacity := fileBackedPVCCapacity(pvc)

	if err := p.resizeFileBackedPVC(pvc, newSize); err != nil {
		http.Error(w, fmt.Sprintf("resize failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Update spec and status
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = qty
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: qty}

	// Persist
	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(r.Context(), storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("failed to persist PVC after resize", "key", storeKey, "error", err)
		}
	}
	p.pvcs.Set(key, pvc)

	p.deps.Logger.Infow("resized file-backed PVC",
		"pvc", key,
		"from", fmtBytesHuman(oldCapacity),
		"to", fmtBytesHuman(newSize))

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("PVC %s resized to %s", key, req.Capacity),
	})
}
