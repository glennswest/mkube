package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/runtime"
	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// ISCSIDisk is a cluster-scoped CRD representing a per-instance read/write iSCSI block device.
type ISCSIDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ISCSIDiskSpec   `json:"spec"`
	Status            ISCSIDiskStatus `json:"status,omitempty"`
}

// ISCSIDiskSpec defines the desired state of an ISCSIDisk.
type ISCSIDiskSpec struct {
	Source      string `json:"source"`                // source ISCSICdrom name, ISCSIDisk name, or raw image path
	SizeGB      int    `json:"sizeGB"`                // disk size in GB (must be >= source size)
	Format      string `json:"format,omitempty"`      // raw | qcow2 (default: raw)
	Host        string `json:"host,omitempty"`        // BMH name this disk is bound to
	Description string `json:"description,omitempty"` // human-readable description
}

// ISCSIDiskStatus reports the observed state of an ISCSIDisk.
type ISCSIDiskStatus struct {
	Phase      string `json:"phase"`                 // Pending, Cloning, Ready, Error, Deleting
	DiskPath   string `json:"diskPath"`              // full path to disk file
	DiskSize   int64  `json:"diskSize,omitempty"`    // actual size in bytes
	SourceRef  string `json:"sourceRef,omitempty"`   // resolved source reference
	TargetIQN  string `json:"targetIQN"`             // e.g. iqn.2000-02.com.mikrotik:disk-{name}
	PortalIP   string `json:"portalIP"`              // router IP for iSCSI
	PortalPort int    `json:"portalPort"`            // default 3260
	RouterOSID string `json:"routerosID,omitempty"`  // RouterOS .id for the file disk
	Message    string `json:"message,omitempty"`     // error message when phase=Error
}

// ISCSIDiskList is a list of ISCSIDisk objects.
type ISCSIDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ISCSIDisk `json:"items"`
}

// DeepCopy returns a deep copy of the ISCSIDisk.
func (d *ISCSIDisk) DeepCopy() *ISCSIDisk {
	out := *d
	out.ObjectMeta = *d.ObjectMeta.DeepCopy()
	return &out
}

const (
	diskBasePath     = "/raid1/disks"
	diskIQNPrefix    = "iqn.2000-02.com.mikrotik:disk-"
)

// diskCloneMu serializes clone/resize operations to avoid contention on I/O.
var diskCloneMu sync.Mutex

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadISCSIDisksFromStore loads ISCSIDisk objects from the NATS ISCSIDISKS bucket.
func (p *MicroKubeProvider) LoadISCSIDisksFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.ISCSIDisks == nil {
		return
	}

	keys, err := p.deps.Store.ISCSIDisks.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list iSCSI disks from store", "error", err)
		return
	}

	for _, key := range keys {
		var disk ISCSIDisk
		if _, err := p.deps.Store.ISCSIDisks.GetJSON(ctx, key, &disk); err != nil {
			p.deps.Logger.Warnw("failed to read iSCSI disk from store", "key", key, "error", err)
			continue
		}
		p.iscsiDisks[disk.Name] = &disk
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded iSCSI disks from store", "count", len(keys))
	}
}

func (p *MicroKubeProvider) persistISCSIDisk(ctx context.Context, disk *ISCSIDisk) {
	if p.deps.Store != nil && p.deps.Store.ISCSIDisks != nil {
		if _, err := p.deps.Store.ISCSIDisks.PutJSON(ctx, disk.Name, disk); err != nil {
			p.deps.Logger.Warnw("failed to persist iSCSI disk", "name", disk.Name, "error", err)
		}
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListISCSIDisks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchISCSIDisks(w, r)
		return
	}

	items := make([]ISCSIDisk, 0, len(p.iscsiDisks))
	for _, disk := range p.iscsiDisks {
		d := disk.DeepCopy()
		d.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}
		items = append(items, *d)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, iscsiDiskListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, ISCSIDiskList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDiskList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetISCSIDisk(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	disk, ok := p.iscsiDisks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI disk %q not found", name), http.StatusNotFound)
		return
	}

	d := disk.DeepCopy()
	d.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, iscsiDiskListToTable([]ISCSIDisk{*d}))
		return
	}

	podWriteJSON(w, http.StatusOK, d)
}

func (p *MicroKubeProvider) handleCreateISCSIDisk(w http.ResponseWriter, r *http.Request) {
	var disk ISCSIDisk
	if err := json.NewDecoder(r.Body).Decode(&disk); err != nil {
		http.Error(w, fmt.Sprintf("invalid ISCSIDisk JSON: %v", err), http.StatusBadRequest)
		return
	}

	if disk.Name == "" {
		http.Error(w, "iSCSI disk name is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.iscsiDisks[disk.Name]; exists {
		http.Error(w, fmt.Sprintf("iSCSI disk %q already exists", disk.Name), http.StatusConflict)
		return
	}

	if disk.Spec.Source == "" {
		http.Error(w, "spec.source is required", http.StatusBadRequest)
		return
	}

	if disk.Spec.SizeGB <= 0 {
		http.Error(w, "spec.sizeGB must be > 0", http.StatusBadRequest)
		return
	}

	// Resolve source: ISCSICdrom, ISCSIDisk, or raw path
	sourcePath, sourceType, err := p.resolveISCSIDiskSource(disk.Spec.Source)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid source: %v", err), http.StatusBadRequest)
		return
	}

	disk.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}
	if disk.CreationTimestamp.IsZero() {
		disk.CreationTimestamp = metav1.Now()
	}
	if disk.Spec.Format == "" {
		disk.Spec.Format = "raw"
	}

	diskFile := disk.Name + "." + disk.Spec.Format
	diskPath := filepath.Join(diskBasePath, diskFile)

	disk.Status = ISCSIDiskStatus{
		Phase:      "Pending",
		DiskPath:   diskPath,
		DiskSize:   int64(disk.Spec.SizeGB) * 1024 * 1024 * 1024,
		SourceRef:  disk.Spec.Source,
		TargetIQN:  diskIQNPrefix + disk.Name,
		PortalPort: iscsiDefaultPort,
	}

	// Set portal IP from config (gateway IP of gt network)
	if len(p.deps.Config.Networks) > 0 {
		disk.Status.PortalIP = p.deps.Config.Networks[0].Gateway
	}

	// Store in memory + NATS first (phase=Pending)
	p.iscsiDisks[disk.Name] = &disk
	p.persistISCSIDisk(r.Context(), &disk)

	// Clone in background
	go p.cloneISCSIDisk(context.Background(), disk.Name, sourcePath, sourceType, diskPath, disk.Spec.SizeGB)

	podWriteJSON(w, http.StatusCreated, &disk)
}

func (p *MicroKubeProvider) handleDeleteISCSIDisk(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	disk, ok := p.iscsiDisks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI disk %q not found", name), http.StatusNotFound)
		return
	}

	// Remove iSCSI target from RouterOS
	p.removeISCSIDiskTarget(r.Context(), disk)

	// Delete disk file
	if disk.Status.DiskPath != "" {
		if err := os.Remove(disk.Status.DiskPath); err != nil && !os.IsNotExist(err) {
			p.deps.Logger.Warnw("failed to delete disk file", "path", disk.Status.DiskPath, "error", err)
		} else {
			p.deps.Logger.Infow("deleted disk file", "path", disk.Status.DiskPath)
		}
	}

	if p.deps.Store != nil && p.deps.Store.ISCSIDisks != nil {
		if err := p.deps.Store.ISCSIDisks.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting iSCSI disk from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.iscsiDisks, name)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("iSCSI disk %q deleted", name),
	})
}

func (p *MicroKubeProvider) handlePatchISCSIDisk(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.iscsiDisks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI disk %q not found", name), http.StatusNotFound)
		return
	}

	merged := existing.DeepCopy()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading body: %v", err), http.StatusBadRequest)
		return
	}

	// Parse patch to check for immutable fields
	var patch struct {
		Spec struct {
			Host        *string `json:"host,omitempty"`
			Description *string `json:"description,omitempty"`
			SizeGB      *int    `json:"sizeGB,omitempty"`
		} `json:"spec,omitempty"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, fmt.Sprintf("invalid patch JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Only allow host, description, sizeGB (grow only) after creation
	if patch.Spec.Host != nil {
		merged.Spec.Host = *patch.Spec.Host
	}
	if patch.Spec.Description != nil {
		merged.Spec.Description = *patch.Spec.Description
	}
	if patch.Spec.SizeGB != nil {
		newSize := *patch.Spec.SizeGB
		if newSize < existing.Spec.SizeGB {
			http.Error(w, fmt.Sprintf("cannot shrink disk from %dGB to %dGB", existing.Spec.SizeGB, newSize), http.StatusBadRequest)
			return
		}
		if newSize > existing.Spec.SizeGB {
			merged.Spec.SizeGB = newSize
			// Grow the disk file
			if err := p.growDiskFile(merged.Status.DiskPath, newSize); err != nil {
				http.Error(w, fmt.Sprintf("growing disk: %v", err), http.StatusInternalServerError)
				return
			}
			merged.Status.DiskSize = int64(newSize) * 1024 * 1024 * 1024
		}
	}

	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}
	merged.CreationTimestamp = existing.CreationTimestamp

	p.iscsiDisks[name] = merged
	p.persistISCSIDisk(r.Context(), merged)

	podWriteJSON(w, http.StatusOK, merged)
}

// ─── Clone Handler ──────────────────────────────────────────────────────────

type cloneRequest struct {
	NewName string `json:"newName"`
	Host    string `json:"host,omitempty"`
}

func (p *MicroKubeProvider) handleCloneISCSIDisk(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	src, ok := p.iscsiDisks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI disk %q not found", name), http.StatusNotFound)
		return
	}

	if src.Status.Phase != "Ready" {
		http.Error(w, fmt.Sprintf("iSCSI disk %q is not ready (phase: %s)", name, src.Status.Phase), http.StatusConflict)
		return
	}

	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid clone JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.NewName == "" {
		http.Error(w, "newName is required", http.StatusBadRequest)
		return
	}
	if _, exists := p.iscsiDisks[req.NewName]; exists {
		http.Error(w, fmt.Sprintf("iSCSI disk %q already exists", req.NewName), http.StatusConflict)
		return
	}

	newDiskFile := req.NewName + "." + src.Spec.Format
	newDiskPath := filepath.Join(diskBasePath, newDiskFile)

	newDisk := &ISCSIDisk{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              req.NewName,
			CreationTimestamp: metav1.Now(),
		},
		Spec: ISCSIDiskSpec{
			Source:      name,
			SizeGB:     src.Spec.SizeGB,
			Format:     src.Spec.Format,
			Host:       req.Host,
			Description: fmt.Sprintf("Cloned from %s", name),
		},
		Status: ISCSIDiskStatus{
			Phase:      "Pending",
			DiskPath:   newDiskPath,
			DiskSize:   src.Status.DiskSize,
			SourceRef:  name,
			TargetIQN:  diskIQNPrefix + req.NewName,
			PortalIP:   src.Status.PortalIP,
			PortalPort: iscsiDefaultPort,
		},
	}

	p.iscsiDisks[req.NewName] = newDisk
	p.persistISCSIDisk(r.Context(), newDisk)

	// Clone in background (disk-to-disk copy)
	go p.cloneISCSIDisk(context.Background(), req.NewName, src.Status.DiskPath, "disk", newDiskPath, src.Spec.SizeGB)

	podWriteJSON(w, http.StatusCreated, newDisk)
}

// ─── Resize Handler ─────────────────────────────────────────────────────────

type resizeRequest struct {
	SizeGB int `json:"sizeGB"`
}

func (p *MicroKubeProvider) handleResizeISCSIDisk(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	disk, ok := p.iscsiDisks[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI disk %q not found", name), http.StatusNotFound)
		return
	}

	if disk.Status.Phase != "Ready" {
		http.Error(w, fmt.Sprintf("iSCSI disk %q is not ready (phase: %s)", name, disk.Status.Phase), http.StatusConflict)
		return
	}

	var req resizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid resize JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.SizeGB <= disk.Spec.SizeGB {
		http.Error(w, fmt.Sprintf("new size %dGB must be larger than current %dGB (grow only)", req.SizeGB, disk.Spec.SizeGB), http.StatusBadRequest)
		return
	}

	if err := p.growDiskFile(disk.Status.DiskPath, req.SizeGB); err != nil {
		http.Error(w, fmt.Sprintf("resizing disk: %v", err), http.StatusInternalServerError)
		return
	}

	disk.Spec.SizeGB = req.SizeGB
	disk.Status.DiskSize = int64(req.SizeGB) * 1024 * 1024 * 1024

	p.persistISCSIDisk(r.Context(), disk)

	podWriteJSON(w, http.StatusOK, disk)
}

// ─── Capacity Handler ───────────────────────────────────────────────────────

type diskCapacity struct {
	TotalGB      float64 `json:"totalGB"`
	UsedGB       float64 `json:"usedGB"`
	AvailableGB  float64 `json:"availableGB"`
	DiskCount    int     `json:"diskCount"`
	SparseUsedGB float64 `json:"sparseUsedGB"`
	AllocatedGB  float64 `json:"allocatedGB"`
}

func (p *MicroKubeProvider) handleISCSIDiskCapacity(w http.ResponseWriter, r *http.Request) {
	cap := diskCapacity{
		DiskCount: len(p.iscsiDisks),
	}

	// Sum allocated (apparent) sizes
	var allocatedBytes int64
	var sparseBytes int64
	for _, disk := range p.iscsiDisks {
		allocatedBytes += disk.Status.DiskSize

		// Get actual on-disk usage (sparse size)
		if fi, err := os.Stat(disk.Status.DiskPath); err == nil {
			sparseBytes += fi.Size()
		}
	}
	cap.AllocatedGB = float64(allocatedBytes) / (1024 * 1024 * 1024)
	cap.SparseUsedGB = float64(sparseBytes) / (1024 * 1024 * 1024)

	// Get filesystem stats for /raid1
	if usage, err := getDiskUsage("/raid1"); err == nil {
		cap.TotalGB = float64(usage.total) / (1024 * 1024 * 1024)
		cap.UsedGB = float64(usage.used) / (1024 * 1024 * 1024)
		cap.AvailableGB = float64(usage.avail) / (1024 * 1024 * 1024)
	}

	podWriteJSON(w, http.StatusOK, cap)
}

// ─── Source Resolution ──────────────────────────────────────────────────────

// resolveISCSIDiskSource resolves a source name to a file path and type.
// Returns (path, type, error) where type is "cdrom", "disk", or "path".
func (p *MicroKubeProvider) resolveISCSIDiskSource(source string) (string, string, error) {
	// Check ISCSICdrom
	if cdrom, ok := p.iscsiCdroms[source]; ok {
		if cdrom.Status.Phase != "Ready" {
			return "", "", fmt.Errorf("source ISCSICdrom %q is not ready (phase: %s)", source, cdrom.Status.Phase)
		}
		return cdrom.Status.ISOPath, "cdrom", nil
	}

	// Check ISCSIDisk
	if disk, ok := p.iscsiDisks[source]; ok {
		if disk.Status.Phase != "Ready" {
			return "", "", fmt.Errorf("source ISCSIDisk %q is not ready (phase: %s)", source, disk.Status.Phase)
		}
		return disk.Status.DiskPath, "disk", nil
	}

	// Check raw path
	if _, err := os.Stat(source); err == nil {
		return source, "path", nil
	}

	return "", "", fmt.Errorf("source %q not found (not an ISCSICdrom, ISCSIDisk, or file path)", source)
}

// ─── Disk Operations ────────────────────────────────────────────────────────

// sourceFormat identifies the image format of a source file.
type sourceFormat struct {
	compressed string // "xz", "gz", "" (none)
	diskFormat string // "raw", "vmdk", "qcow2", "iso"
}

// detectSourceFormat determines the format from file extension.
func detectSourceFormat(path string) sourceFormat {
	lower := strings.ToLower(path)
	sf := sourceFormat{diskFormat: "raw"}

	// Check for compression layer
	if strings.HasSuffix(lower, ".xz") {
		sf.compressed = "xz"
		lower = strings.TrimSuffix(lower, ".xz")
	} else if strings.HasSuffix(lower, ".gz") {
		sf.compressed = "gz"
		lower = strings.TrimSuffix(lower, ".gz")
	}

	// Check disk format
	switch {
	case strings.HasSuffix(lower, ".vmdk"):
		sf.diskFormat = "vmdk"
	case strings.HasSuffix(lower, ".qcow2"):
		sf.diskFormat = "qcow2"
	case strings.HasSuffix(lower, ".vhd"), strings.HasSuffix(lower, ".vhdx"):
		sf.diskFormat = "vpc" // qemu-img uses "vpc" for VHD
	case strings.HasSuffix(lower, ".iso"):
		sf.diskFormat = "iso"
	case strings.HasSuffix(lower, ".img"), strings.HasSuffix(lower, ".raw"):
		sf.diskFormat = "raw"
	}

	return sf
}

// needsConversion returns true if the source requires decompression or format conversion.
func (sf sourceFormat) needsConversion() bool {
	return sf.compressed != "" || (sf.diskFormat != "raw" && sf.diskFormat != "iso")
}

// cloneISCSIDisk performs the background clone: creates sparse file, copies source, registers target.
func (p *MicroKubeProvider) cloneISCSIDisk(ctx context.Context, name, sourcePath, sourceType, diskPath string, sizeGB int) {
	diskCloneMu.Lock()
	defer diskCloneMu.Unlock()

	p.mu.Lock()
	disk, ok := p.iscsiDisks[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	disk.Status.Phase = "Cloning"
	p.mu.Unlock()
	p.persistISCSIDisk(ctx, disk)

	log := p.deps.Logger.With("disk", name)

	// Ensure disk directory exists
	if err := os.MkdirAll(diskBasePath, 0o755); err != nil {
		p.setDiskError(ctx, name, fmt.Sprintf("creating disk directory: %v", err))
		return
	}

	targetBytes := int64(sizeGB) * 1024 * 1024 * 1024

	// Detect source format and convert if needed
	sf := detectSourceFormat(sourcePath)
	effectivePath := sourcePath

	if sf.needsConversion() {
		log.Infow("source needs conversion", "path", sourcePath,
			"compressed", sf.compressed, "format", sf.diskFormat)

		var err error
		effectivePath, err = p.convertSourceToRaw(ctx, sourcePath, sf, log)
		if err != nil {
			p.setDiskError(ctx, name, fmt.Sprintf("converting source: %v", err))
			return
		}
		defer os.Remove(effectivePath) // clean up intermediate file
		log.Infow("source converted to raw", "intermediate", effectivePath)

		// For converted sources, treat as raw path
		sourceType = "path"
	}

	// Create sparse disk file of target size
	if err := createSparseFile(diskPath, targetBytes); err != nil {
		p.setDiskError(ctx, name, fmt.Sprintf("creating sparse file: %v", err))
		return
	}
	log.Infow("sparse disk file created", "path", diskPath, "sizeGB", sizeGB)

	// Copy source into the disk
	switch sourceType {
	case "cdrom":
		// ISO source: dd the ISO into the beginning of the raw disk
		if err := ddCopy(effectivePath, diskPath); err != nil {
			os.Remove(diskPath)
			p.setDiskError(ctx, name, fmt.Sprintf("copying ISO to disk: %v", err))
			return
		}
		log.Infow("ISO copied to disk", "source", effectivePath)
	case "disk", "path":
		// Disk-to-disk: cp --sparse=auto
		if err := sparseCopy(effectivePath, diskPath); err != nil {
			os.Remove(diskPath)
			p.setDiskError(ctx, name, fmt.Sprintf("copying disk: %v", err))
			return
		}
		// If target is larger, extend the file
		if fi, err := os.Stat(diskPath); err == nil && fi.Size() < targetBytes {
			if err := truncateFile(diskPath, targetBytes); err != nil {
				log.Warnw("failed to extend disk after copy", "error", err)
			}
		}
		log.Infow("disk copied", "source", effectivePath)
	}

	// Configure iSCSI target on RouterOS
	p.mu.Lock()
	disk, ok = p.iscsiDisks[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	if err := p.configureISCSIDiskTarget(ctx, disk); err != nil {
		p.setDiskError(ctx, name, fmt.Sprintf("configuring iSCSI target: %v", err))
		return
	}

	// Update status to Ready
	p.mu.Lock()
	disk, ok = p.iscsiDisks[name]
	if ok {
		disk.Status.Phase = "Ready"
		disk.Status.Message = ""
		if fi, err := os.Stat(diskPath); err == nil {
			disk.Status.DiskSize = fi.Size()
		}
	}
	p.mu.Unlock()

	if ok {
		p.persistISCSIDisk(ctx, disk)
		log.Infow("iSCSI disk ready",
			"iqn", disk.Status.TargetIQN,
			"diskID", disk.Status.RouterOSID,
			"path", diskPath)
	}
}

// convertSourceToRaw handles decompression and format conversion, returning
// the path to a raw disk image (caller must clean up the temp file).
func (p *MicroKubeProvider) convertSourceToRaw(ctx context.Context, sourcePath string, sf sourceFormat, log interface{ Infow(string, ...interface{}) }) (string, error) {
	currentPath := sourcePath

	// Step 1: Decompress if needed
	if sf.compressed != "" {
		decompressed, err := decompressFile(currentPath, sf.compressed)
		if err != nil {
			return "", fmt.Errorf("decompressing %s: %w", sf.compressed, err)
		}
		currentPath = decompressed
		log.Infow("decompressed source", "format", sf.compressed, "output", currentPath)
	}

	// Step 2: Convert disk format to raw if needed
	if sf.diskFormat != "raw" && sf.diskFormat != "iso" {
		rawPath, err := convertDiskToRaw(currentPath, sf.diskFormat)
		if err != nil {
			// Clean up decompressed intermediate if we created one
			if currentPath != sourcePath {
				os.Remove(currentPath)
			}
			return "", fmt.Errorf("converting %s to raw: %w", sf.diskFormat, err)
		}
		// Clean up decompressed intermediate
		if currentPath != sourcePath {
			os.Remove(currentPath)
		}
		currentPath = rawPath
		log.Infow("converted to raw", "from", sf.diskFormat, "output", currentPath)
	}

	return currentPath, nil
}

// decompressFile decompresses an xz or gz file, returning the path to the
// decompressed output (temp file in the same directory as source).
func decompressFile(path, compression string) (string, error) {
	srcFile, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer srcFile.Close()

	// Create temp output file next to source (same filesystem for rename if needed)
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	// Strip compression extension for output name
	switch compression {
	case "xz":
		base = strings.TrimSuffix(base, ".xz")
		base = strings.TrimSuffix(base, ".XZ")
	case "gz":
		base = strings.TrimSuffix(base, ".gz")
		base = strings.TrimSuffix(base, ".GZ")
	}
	outPath := filepath.Join(dir, base+".decompressing")

	dstFile, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("creating output: %w", err)
	}
	defer func() {
		dstFile.Close()
		// Rename to final name on success (remove .decompressing suffix)
		finalPath := filepath.Join(dir, base)
		os.Rename(outPath, finalPath)
	}()

	switch compression {
	case "xz":
		reader, err := xz.NewReader(srcFile)
		if err != nil {
			os.Remove(outPath)
			return "", fmt.Errorf("creating xz reader: %w", err)
		}
		if _, err := io.Copy(dstFile, reader); err != nil {
			os.Remove(outPath)
			return "", fmt.Errorf("decompressing xz: %w", err)
		}
	case "gz":
		// Use exec for gzip (simpler than importing compress/gzip for large files)
		dstFile.Close()
		srcFile.Close()
		cmd := exec.Command("gzip", "-d", "-k", "-c", path)
		out, err := os.Create(outPath)
		if err != nil {
			return "", err
		}
		cmd.Stdout = out
		err = cmd.Run()
		out.Close()
		if err != nil {
			os.Remove(outPath)
			return "", fmt.Errorf("decompressing gz: %w", err)
		}
	default:
		os.Remove(outPath)
		return "", fmt.Errorf("unsupported compression: %s", compression)
	}

	finalPath := filepath.Join(dir, base)
	return finalPath, nil
}

// convertDiskToRaw converts a vmdk/qcow2/vhd image to raw format using qemu-img.
// Returns the path to the raw output (temp file, caller must clean up).
func convertDiskToRaw(inputPath, format string) (string, error) {
	outputPath := inputPath + ".converting.raw"

	cmd := exec.Command("qemu-img", "convert", "-f", format, "-O", "raw", inputPath, outputPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outputPath)
		return "", fmt.Errorf("qemu-img convert failed: %w\noutput: %s", err, string(out))
	}

	return outputPath, nil
}

func (p *MicroKubeProvider) setDiskError(ctx context.Context, name, msg string) {
	p.deps.Logger.Errorw("iSCSI disk error", "name", name, "error", msg)
	p.mu.Lock()
	if disk, ok := p.iscsiDisks[name]; ok {
		disk.Status.Phase = "Error"
		disk.Status.Message = msg
	}
	p.mu.Unlock()
	if disk, ok := p.iscsiDisks[name]; ok {
		p.persistISCSIDisk(ctx, disk)
	}
}

// ─── File Utilities ─────────────────────────────────────────────────────────

// createSparseFile creates a sparse file by seeking to the target size.
func createSparseFile(path string, sizeBytes int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(sizeBytes); err != nil {
		return err
	}
	return nil
}

// ddCopy copies src into the beginning of dst using dd (preserving dst size).
func ddCopy(src, dst string) error {
	// Get source file size
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Open source and destination
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening destination: %w", err)
	}
	defer dstFile.Close()

	// Copy source content to beginning of destination
	copied, err := io.CopyN(dstFile, srcFile, fi.Size())
	if err != nil {
		return fmt.Errorf("copy failed after %d bytes: %w", copied, err)
	}

	return dstFile.Sync()
}

// sparseCopy copies src to dst preserving sparse holes.
// Falls back to regular copy if cp --sparse=auto is not available.
func sparseCopy(src, dst string) error {
	// Try cp --sparse=auto first (Linux)
	cmd := exec.Command("cp", "--sparse=auto", src, dst)
	if err := cmd.Run(); err != nil {
		// Fallback: regular file copy
		return regularCopy(src, dst)
	}
	return nil
}

func regularCopy(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}
	return dstFile.Sync()
}

func truncateFile(path string, sizeBytes int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(sizeBytes)
}

func (p *MicroKubeProvider) growDiskFile(path string, newSizeGB int) error {
	newBytes := int64(newSizeGB) * 1024 * 1024 * 1024
	return truncateFile(path, newBytes)
}

// diskUsage holds filesystem usage stats.
type diskUsage struct {
	total uint64
	used  uint64
	avail uint64
}

// getDiskUsage returns filesystem usage for the given path.
// Uses syscall.Statfs on Linux/macOS.
func getDiskUsage(path string) (*diskUsage, error) {
	return getDiskUsagePlatform(path)
}

// ─── RouterOS iSCSI Integration ─────────────────────────────────────────────

func (p *MicroKubeProvider) configureISCSIDiskTarget(ctx context.Context, disk *ISCSIDisk) error {
	ros := p.deps.Runtime
	rosClient, ok := ros.(interface {
		CreateISCSITarget(ctx context.Context, name, filePath string) (string, error)
	})
	if !ok {
		p.deps.Logger.Warnw("runtime does not support iSCSI operations (not RouterOS), skipping")
		return nil
	}

	// Translate container path to host-visible path for RouterOS
	hostPath := p.deps.StorageMgr.HostVisiblePath(disk.Status.DiskPath)

	// Use "disk-" prefix to differentiate from cdrom targets
	targetName := "disk-" + disk.Name

	diskID, err := rosClient.CreateISCSITarget(ctx, targetName, hostPath)
	if err != nil {
		return fmt.Errorf("creating iSCSI target: %w", err)
	}
	disk.Status.RouterOSID = diskID

	// RouterOS auto-generates IQN from the slot name. Update with actual IQN.
	if rosRT, ok2 := ros.(*runtime.RouterOSRuntime); ok2 {
		if d, err := rosRT.GetISCSIDisk(ctx, diskID); err == nil && d.ISCSIServerIQN != "" {
			disk.Status.TargetIQN = d.ISCSIServerIQN
		}
	}

	p.deps.Logger.Infow("iSCSI disk target configured via ROSE /disk",
		"name", disk.Name,
		"iqn", disk.Status.TargetIQN,
		"diskID", diskID,
		"file", hostPath)

	return nil
}

func (p *MicroKubeProvider) removeISCSIDiskTarget(ctx context.Context, disk *ISCSIDisk) {
	if disk.Status.RouterOSID == "" {
		return
	}
	ros := p.deps.Runtime
	rosClient, ok := ros.(interface {
		RemoveISCSITarget(ctx context.Context, id string) error
	})
	if !ok {
		return
	}

	if err := rosClient.RemoveISCSITarget(ctx, disk.Status.RouterOSID); err != nil {
		p.deps.Logger.Warnw("failed to remove iSCSI disk target", "id", disk.Status.RouterOSID, "error", err)
	}
}

// ReconcileISCSIDiskTargets checks that iSCSI targets exist for all Ready disks
// and recreates any that are missing (e.g. after RouterOS reboot).
func (p *MicroKubeProvider) ReconcileISCSIDiskTargets(ctx context.Context) {
	for _, disk := range p.iscsiDisks {
		if disk.Status.Phase != "Ready" {
			continue
		}
		if disk.Status.RouterOSID != "" {
			continue // already configured
		}
		// Target missing — try to recreate
		if _, err := os.Stat(disk.Status.DiskPath); err != nil {
			continue // file doesn't exist, skip
		}
		if err := p.configureISCSIDiskTarget(ctx, disk); err != nil {
			p.deps.Logger.Warnw("failed to reconcile iSCSI disk target", "name", disk.Name, "error", err)
		} else {
			p.persistISCSIDisk(ctx, disk)
			p.deps.Logger.Infow("reconciled missing iSCSI disk target", "name", disk.Name)
		}
	}
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchISCSIDisks(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.ISCSIDisks == nil {
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

	// Send existing objects as ADDED events (snapshot under read lock)
	enc := json.NewEncoder(w)
	p.mu.RLock()
	diskSnapshot := make([]*ISCSIDisk, 0, len(p.iscsiDisks))
	for _, disk := range p.iscsiDisks {
		diskSnapshot = append(diskSnapshot, disk.DeepCopy())
	}
	p.mu.RUnlock()
	for _, d := range diskSnapshot {
		d.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}
		evt := K8sWatchEvent{Type: "ADDED", Object: d}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.ISCSIDisks.WatchAll(ctx)
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

			var disk ISCSIDisk
			if evt.Type == store.EventDelete {
				disk = ISCSIDisk{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &disk); err != nil {
					continue
				}
				disk.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSIDisk"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &disk,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func iscsiDiskListToTable(disks []ISCSIDisk) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Phase", Type: "string"},
			{Name: "Size", Type: "string"},
			{Name: "Host", Type: "string"},
			{Name: "Source", Type: "string"},
			{Name: "Target IQN", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range disks {
		disk := &disks[i]

		size := ""
		if disk.Spec.SizeGB > 0 {
			size = fmt.Sprintf("%dGi", disk.Spec.SizeGB)
		}

		age := "<unknown>"
		if !disk.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(disk.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              disk.Name,
				"creationTimestamp": disk.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				disk.Name,
				disk.Status.Phase,
				size,
				disk.Spec.Host,
				disk.Spec.Source,
				disk.Status.TargetIQN,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Consistency Checks ─────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkISCSIDiskCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.ISCSIDisks != nil {
		storeKeys, err := p.deps.Store.ISCSIDisks.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.iscsiDisks {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("iscsi-disk/%s", name),
						Status:  "pass",
						Message: "iSCSI disk CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("iscsi-disk/%s", name),
						Status:  "fail",
						Message: "iSCSI disk CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("iscsi-disk/%s", name),
					Status:  "warn",
					Message: "iSCSI disk CRD in NATS but not in memory",
				})
			}
		}
	}

	// Verify disk files exist for Ready disks
	for _, disk := range p.iscsiDisks {
		if disk.Status.Phase != "Ready" {
			continue
		}
		if disk.Status.DiskPath == "" {
			continue
		}
		if _, err := os.Stat(disk.Status.DiskPath); err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("iscsi-disk-file/%s", disk.Name),
				Status:  "fail",
				Message: fmt.Sprintf("disk file missing: %s", disk.Status.DiskPath),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("iscsi-disk-file/%s", disk.Name),
				Status:  "pass",
				Message: fmt.Sprintf("disk file exists: %s", disk.Status.DiskPath),
			})
		}
	}

	return items
}
