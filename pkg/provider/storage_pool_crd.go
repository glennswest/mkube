package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/routeros"
	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// StoragePool is a cluster-scoped CRD representing a physical storage device.
type StoragePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              StoragePoolSpec   `json:"spec"`
	Status            StoragePoolStatus `json:"status,omitempty"`
}

// StoragePoolSpec defines the desired state of a StoragePool.
type StoragePoolSpec struct {
	MountPoint string           `json:"mountPoint"`         // RouterOS mount-point name: "raid1", "sata1"
	Default    bool             `json:"default,omitempty"`  // exactly one pool is default (backward compat)
	Paths      StoragePoolPaths `json:"paths,omitempty"`    // optional overrides for subdirectory layout
}

// StoragePoolPaths allows overriding the default subdirectory layout.
type StoragePoolPaths struct {
	Images   string `json:"images,omitempty"`   // default: /{mount}/images
	Cache    string `json:"cache,omitempty"`    // default: /{mount}/cache
	Volumes  string `json:"volumes,omitempty"`  // default: /{mount}/volumes/pvc
	Disks    string `json:"disks,omitempty"`    // default: /{mount}/disks
	ISOs     string `json:"isos,omitempty"`     // default: /{mount}/isos
	Registry string `json:"registry,omitempty"` // default: /{mount}/registry
}

// StoragePoolStatus reports the observed state of a StoragePool.
type StoragePoolStatus struct {
	Phase       string `json:"phase"`                       // Active, Degraded, Offline, Discovered
	TotalBytes  int64  `json:"totalBytes,omitempty"`
	UsedBytes   int64  `json:"usedBytes,omitempty"`
	AvailBytes  int64  `json:"availBytes,omitempty"`
	Filesystem  string `json:"filesystem,omitempty"`        // ext4
	DeviceModel string `json:"deviceModel,omitempty"`
	DeviceType  string `json:"deviceType,omitempty"`        // hardware, raid
	RaidType    string `json:"raidType,omitempty"`          // raid-0, raid-1
	RaidDevices int    `json:"raidDevices,omitempty"`
	Interface   string `json:"interface,omitempty"`         // NVMe, SATA
	Slot        string `json:"slot,omitempty"`              // RouterOS slot
	DiskCount   int    `json:"diskCount,omitempty"`         // iSCSI disks on this pool
	PVCCount    int    `json:"pvcCount,omitempty"`          // PVCs on this pool
	LastSeen    string `json:"lastSeen,omitempty"`
}

// StoragePoolList is a list of StoragePool objects.
type StoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []StoragePool `json:"items"`
}

// DeepCopy returns a deep copy of the StoragePool.
func (sp *StoragePool) DeepCopy() *StoragePool {
	out := *sp
	out.ObjectMeta = *sp.ObjectMeta.DeepCopy()
	return &out
}

// ─── Path Helpers ───────────────────────────────────────────────────────────

// volumesPath returns the PVC volumes path for this pool.
func (sp *StoragePool) volumesPath() string {
	if sp.Spec.Paths.Volumes != "" {
		return sp.Spec.Paths.Volumes
	}
	return "/" + sp.Spec.MountPoint + "/volumes/pvc"
}

// disksPath returns the iSCSI disks path for this pool.
func (sp *StoragePool) disksPath() string {
	if sp.Spec.Paths.Disks != "" {
		return sp.Spec.Paths.Disks
	}
	return "/" + sp.Spec.MountPoint + "/disks"
}

// imagesPath returns the container images path for this pool.
func (sp *StoragePool) imagesPath() string {
	if sp.Spec.Paths.Images != "" {
		return sp.Spec.Paths.Images
	}
	return "/" + sp.Spec.MountPoint + "/images"
}

// ─── Pool Resolution ────────────────────────────────────────────────────────

// resolveStoragePool returns the pool by name, falling back to default, then "raid1".
func (p *MicroKubeProvider) resolveStoragePool(name string) *StoragePool {
	if name != "" {
		if pool, ok := p.storagePools.Get(name); ok {
			return pool
		}
	}
	// Fall back to default pool
	for _, pool := range p.storagePools.Snapshot() {
		if pool.Spec.Default {
			return pool
		}
	}
	// Ultimate fallback: synthetic "raid1" pool (backward compat)
	return &StoragePool{
		Spec: StoragePoolSpec{
			MountPoint: "raid1",
			Default:    true,
		},
		Status: StoragePoolStatus{Phase: "Active"},
	}
}

// defaultStoragePool returns the name of the default pool.
func (p *MicroKubeProvider) defaultStoragePool() string {
	for name, pool := range p.storagePools.Snapshot() {
		if pool.Spec.Default {
			return name
		}
	}
	return "raid1"
}

// pvcPoolName extracts the pool name from a PVC (storageClassName or annotation).
func (p *MicroKubeProvider) pvcPoolName(pvc interface{ GetAnnotations() map[string]string }) string {
	// Check annotation first
	if ann := pvc.GetAnnotations(); ann != nil {
		if pool := ann["vkube.io/storage-pool"]; pool != "" {
			return pool
		}
	}
	return ""
}

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadStoragePoolsFromStore loads StoragePool objects from the NATS bucket.
func (p *MicroKubeProvider) LoadStoragePoolsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.StoragePools == nil {
		return
	}

	keys, err := p.deps.Store.StoragePools.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list storage pools from store", "error", err)
		return
	}

	for _, key := range keys {
		var pool StoragePool
		if _, err := p.deps.Store.StoragePools.GetJSON(ctx, key, &pool); err != nil {
			p.deps.Logger.Warnw("failed to read storage pool from store", "key", key, "error", err)
			continue
		}
		p.storagePools.Set(pool.Name, &pool)
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded storage pools from store", "count", len(keys))
	}
}

func (p *MicroKubeProvider) persistStoragePool(ctx context.Context, pool *StoragePool) {
	if p.deps.Store != nil && p.deps.Store.StoragePools != nil {
		if _, err := p.deps.Store.StoragePools.PutJSON(ctx, pool.Name, pool); err != nil {
			p.deps.Logger.Warnw("failed to persist storage pool", "name", pool.Name, "error", err)
		}
	}
}

// ─── Discovery ──────────────────────────────────────────────────────────────

// DiscoverStoragePools queries RouterOS for physical/RAID disks and creates
// or updates StoragePool CRDs for each discovered disk with a mount-point.
func (p *MicroKubeProvider) DiscoverStoragePools(ctx context.Context) {
	rc := p.rosClient()
	if rc == nil {
		return // not a RouterOS backend
	}

	disks, err := rc.ListPhysicalDisks(ctx)
	if err != nil {
		p.deps.Logger.Warnw("failed to discover storage pools", "error", err)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	hasDefault := false
	for _, pool := range p.storagePools.Snapshot() {
		if pool.Spec.Default {
			hasDefault = true
			break
		}
	}

	for _, disk := range disks {
		name := disk.MountPoint
		totalBytes := parseRouterOSSize(disk.Size)
		freeBytes := parseRouterOSSize(disk.Free)
		usedBytes := totalBytes - freeBytes
		raidDevices, _ := strconv.Atoi(disk.RaidDeviceCount)

		existing, exists := p.storagePools.Get(name)
		if exists {
			// Update status only
			existing.Status.TotalBytes = totalBytes
			existing.Status.UsedBytes = usedBytes
			existing.Status.AvailBytes = freeBytes
			existing.Status.Filesystem = disk.Filesystem
			existing.Status.DeviceModel = disk.Model
			existing.Status.DeviceType = disk.Type
			existing.Status.RaidType = disk.RaidType
			existing.Status.RaidDevices = raidDevices
			existing.Status.Interface = disk.Interface
			existing.Status.Slot = disk.Slot
			existing.Status.LastSeen = now
			existing.Status.Phase = "Active"
			p.persistStoragePool(ctx, existing)
		} else {
			// Create new pool
			isDefault := !hasDefault && name == "raid1"
			pool := &StoragePool{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					CreationTimestamp: metav1.Now(),
				},
				Spec: StoragePoolSpec{
					MountPoint: name,
					Default:    isDefault,
				},
				Status: StoragePoolStatus{
					Phase:       "Active",
					TotalBytes:  totalBytes,
					UsedBytes:   usedBytes,
					AvailBytes:  freeBytes,
					Filesystem:  disk.Filesystem,
					DeviceModel: disk.Model,
					DeviceType:  disk.Type,
					RaidType:    disk.RaidType,
					RaidDevices: raidDevices,
					Interface:   disk.Interface,
					Slot:        disk.Slot,
					LastSeen:    now,
				},
			}
			if isDefault {
				hasDefault = true
			}
			p.storagePools.Set(name, pool)
			p.persistStoragePool(ctx, pool)
			p.deps.Logger.Infow("discovered storage pool", "name", name,
				"type", disk.Type, "size", formatBytes(totalBytes))
		}
	}

	// Enrich with usage counts
	p.enrichStoragePoolCounts()
}

// enrichStoragePoolCounts updates DiskCount and PVCCount on all pools.
func (p *MicroKubeProvider) enrichStoragePoolCounts() {
	// Reset counts
	for _, pool := range p.storagePools.Snapshot() {
		pool.Status.DiskCount = 0
		pool.Status.PVCCount = 0
	}

	// Count iSCSI disks per pool
	for _, disk := range p.iscsiDisks.Snapshot() {
		poolName := disk.Spec.StoragePool
		if poolName == "" {
			poolName = p.defaultStoragePool()
		}
		if pool, ok := p.storagePools.Get(poolName); ok {
			pool.Status.DiskCount++
		}
	}

	// Count PVCs per pool
	for _, pvc := range p.pvcs.Snapshot() {
		poolName := p.pvcPoolName(pvc)
		if poolName == "" {
			poolName = p.defaultStoragePool()
		}
		if pool, ok := p.storagePools.Get(poolName); ok {
			pool.Status.PVCCount++
		}
	}
}

// rosClient returns the RouterOS client from the existing provider helper.
// Returns nil if not RouterOS backend.
func (p *MicroKubeProvider) rosClient() *routeros.Client {
	return p.getRouterOSClient()
}

// parseRouterOSSize converts RouterOS size strings (bytes) to int64.
func parseRouterOSSize(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListStoragePools(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchStoragePools(w, r)
		return
	}

	snap := p.storagePools.Snapshot()
	items := make([]StoragePool, 0, len(snap))
	for _, pool := range snap {
		sp := pool.DeepCopy()
		sp.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
		items = append(items, *sp)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, storagePoolListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, StoragePoolList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePoolList"},
		Items:    items,
	})
}

// handleGetStoragePool returns a single storage pool with live status enrichment.
func (p *MicroKubeProvider) handleGetStoragePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	pool, ok := p.storagePools.Get(name)
	var sp *StoragePool
	if ok {
		sp = pool.DeepCopy()
	}

	if !ok {
		http.Error(w, fmt.Sprintf("storage pool %q not found", name), http.StatusNotFound)
		return
	}

	sp.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}

	// Live status enrichment — external HTTP call, no lock held
	p.enrichStoragePoolStatus(r.Context(), sp)

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, storagePoolListToTable([]StoragePool{*sp}))
		return
	}

	podWriteJSON(w, http.StatusOK, sp)
}

func (p *MicroKubeProvider) handleCreateStoragePool(w http.ResponseWriter, r *http.Request) {
	var pool StoragePool
	if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
		http.Error(w, fmt.Sprintf("invalid StoragePool JSON: %v", err), http.StatusBadRequest)
		return
	}

	if pool.Name == "" {
		http.Error(w, "storage pool name is required", http.StatusBadRequest)
		return
	}
	if p.storagePools.Has(pool.Name) {
		http.Error(w, fmt.Sprintf("storage pool %q already exists", pool.Name), http.StatusConflict)
		return
	}
	if pool.Spec.MountPoint == "" {
		pool.Spec.MountPoint = pool.Name
	}

	pool.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
	if pool.CreationTimestamp.IsZero() {
		pool.CreationTimestamp = metav1.Now()
	}
	if pool.Status.Phase == "" {
		pool.Status.Phase = "Active"
	}

	// Enforce single default
	if pool.Spec.Default {
		for _, existing := range p.storagePools.Snapshot() {
			if existing.Spec.Default {
				existing.Spec.Default = false
				p.persistStoragePool(r.Context(), existing)
			}
		}
	}

	p.storagePools.Set(pool.Name, &pool)
	p.persistStoragePool(r.Context(), &pool)

	p.deps.Logger.Infow("created storage pool", "name", pool.Name, "mount", pool.Spec.MountPoint)
	podWriteJSON(w, http.StatusCreated, &pool)
}

func (p *MicroKubeProvider) handleUpdateStoragePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.storagePools.Get(name)
	if !ok {
		http.Error(w, fmt.Sprintf("storage pool %q not found", name), http.StatusNotFound)
		return
	}

	var pool StoragePool
	if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
		http.Error(w, fmt.Sprintf("invalid StoragePool JSON: %v", err), http.StatusBadRequest)
		return
	}

	pool.Name = name
	pool.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
	pool.CreationTimestamp = existing.CreationTimestamp

	// Enforce single default
	if pool.Spec.Default && !existing.Spec.Default {
		for _, other := range p.storagePools.Snapshot() {
			if other.Name != name && other.Spec.Default {
				other.Spec.Default = false
				p.persistStoragePool(r.Context(), other)
			}
		}
	}

	p.storagePools.Set(name, &pool)
	p.persistStoragePool(r.Context(), &pool)

	podWriteJSON(w, http.StatusOK, &pool)
}

func (p *MicroKubeProvider) handlePatchStoragePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.storagePools.Get(name)
	if !ok {
		http.Error(w, fmt.Sprintf("storage pool %q not found", name), http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading body: %v", err), http.StatusBadRequest)
		return
	}

	merged := existing.DeepCopy()
	if err := json.Unmarshal(body, merged); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	merged.Name = name
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
	merged.CreationTimestamp = existing.CreationTimestamp

	// Enforce single default
	if merged.Spec.Default && !existing.Spec.Default {
		for _, other := range p.storagePools.Snapshot() {
			if other.Name != name && other.Spec.Default {
				other.Spec.Default = false
				p.persistStoragePool(r.Context(), other)
			}
		}
	}

	p.storagePools.Set(name, merged)
	p.persistStoragePool(r.Context(), merged)

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteStoragePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	pool, ok := p.storagePools.Get(name)
	if !ok {
		http.Error(w, fmt.Sprintf("storage pool %q not found", name), http.StatusNotFound)
		return
	}

	// Protect pools with active resources
	p.enrichStoragePoolCounts()
	if pool.Status.DiskCount > 0 || pool.Status.PVCCount > 0 {
		http.Error(w, fmt.Sprintf("cannot delete storage pool %q: %d disks and %d PVCs still reference it",
			name, pool.Status.DiskCount, pool.Status.PVCCount), http.StatusConflict)
		return
	}

	if pool.Spec.Default {
		http.Error(w, "cannot delete the default storage pool", http.StatusConflict)
		return
	}

	// Delete from NATS
	if p.deps.Store != nil && p.deps.Store.StoragePools != nil {
		if err := p.deps.Store.StoragePools.Delete(r.Context(), name); err != nil {
			p.deps.Logger.Warnw("failed to delete storage pool from store", "name", name, "error", err)
		}
	}

	p.storagePools.Delete(name)

	p.deps.Logger.Infow("deleted storage pool", "name", name)
	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("storage pool %q deleted", name),
	})
}

// ─── Migration API ──────────────────────────────────────────────────────────

// MigrateRequest describes a resource migration between storage pools.
type MigrateRequest struct {
	ResourceType string `json:"resourceType"` // "pvc" or "disk"
	ResourceName string `json:"resourceName"` // "namespace/name" for PVC, "name" for disk
	TargetPool   string `json:"targetPool"`   // target pool name
	PurgeSource  bool   `json:"purgeSource"`  // remove source after successful copy
}

// MigrateResult is the response from a migration.
type MigrateResult struct {
	Status      string `json:"status"`      // "success" or "error"
	Message     string `json:"message"`
	BytesCopied int64  `json:"bytesCopied,omitempty"`
	DurationMs  int64  `json:"durationMs,omitempty"`
}

func (p *MicroKubeProvider) handleMigrateStoragePool(w http.ResponseWriter, r *http.Request) {
	var req MigrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid migration request: %v", err), http.StatusBadRequest)
		return
	}

	targetPool, ok := p.storagePools.Get(req.TargetPool)
	if !ok {
		http.Error(w, fmt.Sprintf("target pool %q not found", req.TargetPool), http.StatusNotFound)
		return
	}
	if targetPool.Status.Phase != "Active" {
		http.Error(w, fmt.Sprintf("target pool %q is not Active (phase: %s)", req.TargetPool, targetPool.Status.Phase), http.StatusConflict)
		return
	}

	switch req.ResourceType {
	case "pvc":
		result := p.migratePVC(r.Context(), req, targetPool)
		podWriteJSON(w, http.StatusOK, result)
	case "disk":
		result := p.migrateISCSIDisk(r.Context(), req, targetPool)
		podWriteJSON(w, http.StatusOK, result)
	default:
		http.Error(w, fmt.Sprintf("unsupported resource type %q (use 'pvc' or 'disk')", req.ResourceType), http.StatusBadRequest)
	}
}

func (p *MicroKubeProvider) migratePVC(ctx context.Context, req MigrateRequest, targetPool *StoragePool) MigrateResult {
	parts := strings.SplitN(req.ResourceName, "/", 2)
	if len(parts) != 2 {
		return MigrateResult{Status: "error", Message: "resourceName must be namespace/name for PVC"}
	}
	ns, name := parts[0], parts[1]
	key := ns + "/" + name

	pvc, ok := p.pvcs.Get(key)
	if !ok {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("PVC %q not found", key)}
	}

	// Compute source and target paths
	srcPath := p.pvcHostPath(pvc)
	dstPath := fmt.Sprintf("%s/%s_%s", targetPool.volumesPath(), ns, name)

	if srcPath == dstPath {
		return MigrateResult{Status: "error", Message: "source and target paths are identical"}
	}

	start := time.Now()

	// Find pods using this PVC and stop them
	affectedPods := p.findPodsUsingPVC(ns, name)
	for _, pod := range affectedPods {
		p.deps.Logger.Infow("stopping pod for PVC migration", "pod", pod.Namespace+"/"+pod.Name)
		if err := p.stopPodContainers(ctx, pod); err != nil {
			p.deps.Logger.Warnw("failed to stop pod for migration", "pod", pod.Namespace+"/"+pod.Name, "error", err)
		}
	}

	// Ensure target directory
	if err := os.MkdirAll(dstPath, 0755); err != nil {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("creating target directory: %v", err)}
	}

	// Copy data
	bytesCopied, err := copyDirectory(srcPath, dstPath)
	if err != nil {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("copying data: %v", err)}
	}

	// Update PVC annotation with new pool
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations["vkube.io/storage-pool"] = req.TargetPool

	// Persist updated PVC
	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := ns + "." + name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("failed to persist PVC after migration", "key", storeKey, "error", err)
		}
	}

	// Purge source if requested
	if req.PurgeSource {
		if err := os.RemoveAll(srcPath); err != nil {
			p.deps.Logger.Warnw("failed to purge source after PVC migration", "path", srcPath, "error", err)
		}
	}

	// Kick reconcile to restart pods
	p.triggerReconcile()

	duration := time.Since(start)
	p.deps.Logger.Infow("PVC migrated", "pvc", key, "target", req.TargetPool,
		"bytes", bytesCopied, "duration", duration)

	return MigrateResult{
		Status:      "success",
		Message:     fmt.Sprintf("migrated PVC %s to pool %s", key, req.TargetPool),
		BytesCopied: bytesCopied,
		DurationMs:  duration.Milliseconds(),
	}
}

func (p *MicroKubeProvider) migrateISCSIDisk(ctx context.Context, req MigrateRequest, targetPool *StoragePool) MigrateResult {
	disk, ok := p.iscsiDisks.Get(req.ResourceName)
	if !ok {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("iSCSI disk %q not found", req.ResourceName)}
	}

	srcPath := disk.Status.DiskPath
	ext := filepath.Ext(srcPath)
	if ext == "" {
		ext = ".raw"
	}
	dstPath := filepath.Join(targetPool.disksPath(), disk.Name+ext)

	if srcPath == dstPath {
		return MigrateResult{Status: "error", Message: "source and target paths are identical"}
	}

	start := time.Now()

	// Remove iSCSI target to prevent active writes
	if disk.Status.RouterOSID != "" {
		p.removeISCSIDiskTarget(ctx, disk)
	}

	// Ensure target directory
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("creating target directory: %v", err)}
	}

	// Copy disk file
	if err := sparseCopy(srcPath, dstPath); err != nil {
		return MigrateResult{Status: "error", Message: fmt.Sprintf("copying disk file: %v", err)}
	}

	fi, _ := os.Stat(dstPath)
	var bytesCopied int64
	if fi != nil {
		bytesCopied = fi.Size()
	}

	// Update disk spec and status
	disk.Spec.StoragePool = req.TargetPool
	disk.Status.DiskPath = dstPath
	p.persistISCSIDisk(ctx, disk)

	// Re-register iSCSI target at new path
	if err := p.configureISCSIDiskTarget(ctx, disk); err != nil {
		p.deps.Logger.Warnw("failed to re-register iSCSI target after migration", "disk", disk.Name, "error", err)
	}

	// Purge source
	if req.PurgeSource {
		if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
			p.deps.Logger.Warnw("failed to purge source after disk migration", "path", srcPath, "error", err)
		}
	}

	duration := time.Since(start)
	p.deps.Logger.Infow("iSCSI disk migrated", "disk", disk.Name, "target", req.TargetPool,
		"bytes", bytesCopied, "duration", duration)

	return MigrateResult{
		Status:      "success",
		Message:     fmt.Sprintf("migrated iSCSI disk %s to pool %s", disk.Name, req.TargetPool),
		BytesCopied: bytesCopied,
		DurationMs:  duration.Milliseconds(),
	}
}

// findPodsUsingPVC returns all pods that reference the given PVC.
func (p *MicroKubeProvider) findPodsUsingPVC(namespace, claimName string) []*corev1.Pod {
	var result []*corev1.Pod
	for _, pod := range p.pods.Snapshot() {
		if pod.Namespace != namespace {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
				result = append(result, pod)
				break
			}
		}
	}
	return result
}

// stopPodContainers stops all containers in a pod.
func (p *MicroKubeProvider) stopPodContainers(ctx context.Context, pod *corev1.Pod) error {
	for _, c := range pod.Spec.Containers {
		cName := sanitizeName(pod, c.Name)
		if err := p.deps.Runtime.StopContainer(ctx, cName); err != nil {
			return err
		}
	}
	return nil
}

// copyDirectory recursively copies src directory contents to dst.
func copyDirectory(src, dst string) (int64, error) {
	var totalBytes int64

	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // empty PVC, nothing to copy
		}
		return 0, err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := os.MkdirAll(dstPath, 0755); err != nil {
				return totalBytes, err
			}
			n, err := copyDirectory(srcPath, dstPath)
			totalBytes += n
			if err != nil {
				return totalBytes, err
			}
		} else {
			n, err := copyFile(srcPath, dstPath)
			totalBytes += n
			if err != nil {
				return totalBytes, err
			}
		}
	}
	return totalBytes, nil
}

// copyFile copies a single file preserving sparse regions.
func copyFile(src, dst string) (int64, error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer dstFile.Close()

	n, err := io.Copy(dstFile, srcFile)
	return n, err
}

// ─── Status Enrichment ──────────────────────────────────────────────────────

// enrichStoragePoolStatus updates a pool with live capacity data.
func (p *MicroKubeProvider) enrichStoragePoolStatus(ctx context.Context, pool *StoragePool) {
	rc := p.rosClient()
	if rc == nil {
		return
	}

	disks, err := rc.ListPhysicalDisks(ctx)
	if err != nil {
		return
	}

	for _, disk := range disks {
		if disk.MountPoint == pool.Spec.MountPoint {
			pool.Status.TotalBytes = parseRouterOSSize(disk.Size)
			pool.Status.AvailBytes = parseRouterOSSize(disk.Free)
			pool.Status.UsedBytes = pool.Status.TotalBytes - pool.Status.AvailBytes
			pool.Status.Filesystem = disk.Filesystem
			pool.Status.DeviceModel = disk.Model
			pool.Status.DeviceType = disk.Type
			pool.Status.RaidType = disk.RaidType
			raidDevices, _ := strconv.Atoi(disk.RaidDeviceCount)
			pool.Status.RaidDevices = raidDevices
			pool.Status.Interface = disk.Interface
			pool.Status.Slot = disk.Slot
			pool.Status.LastSeen = time.Now().UTC().Format(time.RFC3339)
			break
		}
	}

	// Count resources
	pool.Status.DiskCount = 0
	pool.Status.PVCCount = 0
	defPool := p.defaultStoragePool()
	for _, d := range p.iscsiDisks.Snapshot() {
		pn := d.Spec.StoragePool
		if pn == "" {
			pn = defPool
		}
		if pn == pool.Name {
			pool.Status.DiskCount++
		}
	}
	for _, pvc := range p.pvcs.Snapshot() {
		pn := p.pvcPoolName(pvc)
		if pn == "" {
			pn = defPool
		}
		if pn == pool.Name {
			pool.Status.PVCCount++
		}
	}
}

// ─── Watch Handler ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchStoragePools(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.StoragePools == nil {
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

	// Send existing objects as ADDED events
	enc := json.NewEncoder(w)
	snapshot := make([]*StoragePool, 0, p.storagePools.Len())
	for _, pool := range p.storagePools.Snapshot() {
		snapshot = append(snapshot, pool.DeepCopy())
	}
	for _, sp := range snapshot {
		sp.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
		evt := K8sWatchEvent{Type: "ADDED", Object: sp}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.StoragePools.WatchAll(ctx)
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

			var pool StoragePool
			if evt.Type == store.EventDelete {
				pool = StoragePool{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &pool); err != nil {
					continue
				}
				pool.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "StoragePool"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &pool,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func storagePoolListToTable(pools []StoragePool) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Type", Type: "string"},
			{Name: "Interface", Type: "string"},
			{Name: "Mount", Type: "string"},
			{Name: "Total", Type: "string"},
			{Name: "Used", Type: "string"},
			{Name: "Avail", Type: "string"},
			{Name: "Phase", Type: "string"},
			{Name: "Default", Type: "string"},
			{Name: "Disks", Type: "string"},
			{Name: "PVCs", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range pools {
		sp := &pools[i]

		age := "<unknown>"
		if !sp.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(sp.CreationTimestamp.Time))
		}

		devType := sp.Status.DeviceType
		if sp.Status.RaidType != "" {
			devType = "RAID-" + sp.Status.RaidType
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              sp.Name,
				"creationTimestamp": sp.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				sp.Name,
				devType,
				sp.Status.Interface,
				sp.Spec.MountPoint,
				formatBytes(sp.Status.TotalBytes),
				formatBytes(sp.Status.UsedBytes),
				formatBytes(sp.Status.AvailBytes),
				sp.Status.Phase,
				fmt.Sprintf("%v", sp.Spec.Default),
				fmt.Sprintf("%d", sp.Status.DiskCount),
				fmt.Sprintf("%d", sp.Status.PVCCount),
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// formatBytes converts bytes to human-readable format.
func formatBytes(b int64) string {
	if b <= 0 {
		return "—"
	}
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

// ─── Consistency Checks ─────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkStoragePoolCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	poolSnap := p.storagePools.Snapshot()

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.StoragePools != nil {
		storeKeys, err := p.deps.Store.StoragePools.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range poolSnap {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("storage-pool/%s", name),
						Status:  "pass",
						Message: "storage pool synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("storage-pool/%s", name),
						Status:  "fail",
						Message: "storage pool in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("storage-pool/%s", name),
					Status:  "warn",
					Message: "storage pool in NATS but not in memory",
				})
			}
		}
	}

	// Verify mount-point exists on disk
	for name, pool := range poolSnap {
		mountPath := "/" + pool.Spec.MountPoint
		if _, err := os.Stat(mountPath); err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("storage-pool-mount/%s", name),
				Status:  "warn",
				Message: fmt.Sprintf("mount-point %s not accessible: %v", mountPath, err),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("storage-pool-mount/%s", name),
				Status:  "pass",
				Message: fmt.Sprintf("mount-point %s accessible", mountPath),
			})
		}
	}

	// Verify exactly one default pool
	defaultCount := 0
	for _, pool := range poolSnap {
		if pool.Spec.Default {
			defaultCount++
		}
	}
	if len(poolSnap) > 0 {
		if defaultCount == 1 {
			items = append(items, CheckItem{
				Name:    "storage-pool/default",
				Status:  "pass",
				Message: fmt.Sprintf("exactly one default pool (%s)", p.defaultStoragePool()),
			})
		} else if defaultCount == 0 {
			items = append(items, CheckItem{
				Name:    "storage-pool/default",
				Status:  "warn",
				Message: "no default pool set (will fall back to raid1)",
			})
		} else {
			items = append(items, CheckItem{
				Name:    "storage-pool/default",
				Status:  "fail",
				Message: fmt.Sprintf("%d pools marked as default (should be exactly 1)", defaultCount),
			})
		}
	}

	return items
}
