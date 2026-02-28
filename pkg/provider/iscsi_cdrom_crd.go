package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/runtime"
	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// ISCSICdrom is a cluster-scoped CRD representing an ISO image shared via iSCSI.
type ISCSICdrom struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              ISCSICdromSpec   `json:"spec"`
	Status            ISCSICdromStatus `json:"status,omitempty"`
}

// ISCSICdromSpec defines the desired state of an ISCSICdrom.
type ISCSICdromSpec struct {
	ISOFile     string   `json:"isoFile"`               // ISO file name under /raid1/iso/
	Description string   `json:"description,omitempty"`  // human-readable description
	Version     string   `json:"version,omitempty"`      // version string, e.g. "4.17.8" or "rhcos-4.18"
	ReadOnly    bool     `json:"readOnly"`               // always true for CDROMs
	BootConfigs []string `json:"bootConfigs,omitempty"`  // compatible BootConfig names for this ISO
	DerivedFrom string   `json:"derivedFrom,omitempty"`  // base ISCSICdrom name (if derived via ISO patching)
}

// ISCSICdromStatus reports the observed state of an ISCSICdrom.
type ISCSICdromStatus struct {
	Phase       string            `json:"phase"`                  // Pending, Uploading, Ready, Error
	ISOPath     string            `json:"isoPath"`                // full container path
	ISOSize     int64             `json:"isoSize,omitempty"`      // bytes
	TargetIQN   string            `json:"targetIQN"`              // e.g. iqn.2024-01.lo.gt:cdrom-{name}
	PortalIP    string            `json:"portalIP"`               // rose1 IP for iSCSI
	PortalPort  int               `json:"portalPort"`             // default 3260
	RouterOSID  string            `json:"routerosID,omitempty"`   // RouterOS .id for the file disk
	Subscribers []ISCSISubscriber `json:"subscribers,omitempty"`
}

// ISCSISubscriber tracks a consumer of the iSCSI CDROM.
type ISCSISubscriber struct {
	Name         string `json:"name"`                    // subscriber identifier
	InitiatorIQN string `json:"initiatorIQN,omitempty"` // optional iSCSI initiator IQN
	Since        string `json:"since"`                   // ISO 8601 timestamp
}

// ISCSICdromList is a list of ISCSICdrom objects.
type ISCSICdromList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ISCSICdrom `json:"items"`
}

// DeepCopy returns a deep copy of the ISCSICdrom.
func (c *ISCSICdrom) DeepCopy() *ISCSICdrom {
	out := *c
	out.ObjectMeta = *c.ObjectMeta.DeepCopy()
	out.Spec.BootConfigs = append([]string(nil), c.Spec.BootConfigs...)
	out.Status.Subscribers = append([]ISCSISubscriber(nil), c.Status.Subscribers...)
	return &out
}

const (
	isoBasePath       = "/raid1/iso"
	iscsiDefaultPort  = 3260
	iscsiIQNPrefix    = "iqn.2024-01.lo.gt:cdrom-"
)

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadISCSICdromsFromStore loads ISCSICdrom objects from the NATS ISCSICDROMS bucket.
func (p *MicroKubeProvider) LoadISCSICdromsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.ISCSICdroms == nil {
		return
	}

	keys, err := p.deps.Store.ISCSICdroms.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list iSCSI CDROMs from store", "error", err)
		return
	}

	for _, key := range keys {
		var cdrom ISCSICdrom
		if _, err := p.deps.Store.ISCSICdroms.GetJSON(ctx, key, &cdrom); err != nil {
			p.deps.Logger.Warnw("failed to read iSCSI CDROM from store", "key", key, "error", err)
			continue
		}
		p.iscsiCdroms[cdrom.Name] = &cdrom
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded iSCSI CDROMs from store", "count", len(keys))
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListISCSICdroms(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchISCSICdroms(w, r)
		return
	}

	items := make([]ISCSICdrom, 0, len(p.iscsiCdroms))
	for _, cdrom := range p.iscsiCdroms {
		c := cdrom.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}
		items = append(items, *c)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, iscsiCdromListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, ISCSICdromList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdromList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	c := cdrom.DeepCopy()
	c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, iscsiCdromListToTable([]ISCSICdrom{*c}))
		return
	}

	podWriteJSON(w, http.StatusOK, c)
}

func (p *MicroKubeProvider) handleCreateISCSICdrom(w http.ResponseWriter, r *http.Request) {
	var cdrom ISCSICdrom
	if err := json.NewDecoder(r.Body).Decode(&cdrom); err != nil {
		http.Error(w, fmt.Sprintf("invalid ISCSICdrom JSON: %v", err), http.StatusBadRequest)
		return
	}

	if cdrom.Name == "" {
		http.Error(w, "iSCSI CDROM name is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.iscsiCdroms[cdrom.Name]; exists {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q already exists", cdrom.Name), http.StatusConflict)
		return
	}

	cdrom.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}
	if cdrom.CreationTimestamp.IsZero() {
		cdrom.CreationTimestamp = metav1.Now()
	}

	// Defaults
	cdrom.Spec.ReadOnly = true
	if cdrom.Spec.ISOFile == "" {
		cdrom.Spec.ISOFile = cdrom.Name + ".iso"
	}

	cdrom.Status.Phase = "Pending"
	cdrom.Status.ISOPath = filepath.Join(isoBasePath, cdrom.Spec.ISOFile)
	cdrom.Status.TargetIQN = iscsiIQNPrefix + cdrom.Name
	cdrom.Status.PortalPort = iscsiDefaultPort

	// Set portal IP from config (gateway IP of gt network)
	if len(p.deps.Config.Networks) > 0 {
		cdrom.Status.PortalIP = p.deps.Config.Networks[0].Gateway
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), cdrom.Name, &cdrom); err != nil {
			http.Error(w, fmt.Sprintf("persisting iSCSI CDROM: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.iscsiCdroms[cdrom.Name] = &cdrom

	podWriteJSON(w, http.StatusCreated, &cdrom)
}

func (p *MicroKubeProvider) handleUpdateISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	old, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	var cdrom ISCSICdrom
	if err := json.NewDecoder(r.Body).Decode(&cdrom); err != nil {
		http.Error(w, fmt.Sprintf("invalid ISCSICdrom JSON: %v", err), http.StatusBadRequest)
		return
	}
	cdrom.Name = name
	cdrom.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}

	if cdrom.CreationTimestamp.IsZero() {
		cdrom.CreationTimestamp = old.CreationTimestamp
	}
	// Preserve status fields that shouldn't be overwritten by a spec update
	if cdrom.Status.Phase == "" {
		cdrom.Status = old.Status
	}

	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), name, &cdrom); err != nil {
			http.Error(w, fmt.Sprintf("persisting iSCSI CDROM update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.iscsiCdroms[name] = &cdrom

	podWriteJSON(w, http.StatusOK, &cdrom)
}

func (p *MicroKubeProvider) handlePatchISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

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
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}
	merged.CreationTimestamp = existing.CreationTimestamp

	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), name, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting iSCSI CDROM patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.iscsiCdroms[name] = merged

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	// Block delete if subscribers exist
	if len(cdrom.Status.Subscribers) > 0 {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q has %d active subscribers — unsubscribe all first",
			name, len(cdrom.Status.Subscribers)), http.StatusConflict)
		return
	}

	// Remove iSCSI target from RouterOS
	p.removeISCSITarget(r.Context(), cdrom)

	// Optionally delete ISO file
	if r.URL.Query().Get("deleteISO") == "true" {
		if cdrom.Status.ISOPath != "" {
			if err := os.Remove(cdrom.Status.ISOPath); err != nil && !os.IsNotExist(err) {
				p.deps.Logger.Warnw("failed to delete ISO file", "path", cdrom.Status.ISOPath, "error", err)
			} else {
				p.deps.Logger.Infow("deleted ISO file", "path", cdrom.Status.ISOPath)
			}
		}
	}

	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if err := p.deps.Store.ISCSICdroms.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting iSCSI CDROM from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.iscsiCdroms, name)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("iSCSI CDROM %q deleted", name),
	})
}

// ─── Upload Handler ─────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleUploadISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	// Ensure ISO directory exists
	if err := os.MkdirAll(isoBasePath, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("creating ISO directory: %v", err), http.StatusInternalServerError)
		return
	}

	// Stream multipart data directly to disk — no memory buffering.
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, fmt.Sprintf("reading multipart stream: %v", err), http.StatusBadRequest)
		return
	}

	var written int64
	isoPath := cdrom.Status.ISOPath
	tmpPath := isoPath + ".uploading"

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("reading multipart part: %v", err), http.StatusBadRequest)
			return
		}
		if part.FormName() != "iso" {
			part.Close()
			continue
		}

		cdrom.Status.Phase = "Uploading"

		// Write to temp file first, rename on success (atomic)
		dst, err := os.Create(tmpPath)
		if err != nil {
			part.Close()
			http.Error(w, fmt.Sprintf("creating ISO file: %v", err), http.StatusInternalServerError)
			return
		}

		written, err = io.Copy(dst, part)
		part.Close()

		if err != nil {
			dst.Close()
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("writing ISO file: %v", err), http.StatusInternalServerError)
			return
		}

		// Flush all data to stable storage before closing
		if err := dst.Sync(); err != nil {
			dst.Close()
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("syncing ISO file to disk: %v", err), http.StatusInternalServerError)
			return
		}

		if err := dst.Close(); err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("closing ISO file: %v", err), http.StatusInternalServerError)
			return
		}

		// Verify written size matches file on disk
		fi, err := os.Stat(tmpPath)
		if err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("stat ISO file: %v", err), http.StatusInternalServerError)
			return
		}
		if fi.Size() != written {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("ISO file size mismatch: wrote %d bytes but file is %d bytes", written, fi.Size()), http.StatusInternalServerError)
			return
		}

		// Atomic rename from temp to final path
		if err := os.Rename(tmpPath, isoPath); err != nil {
			os.Remove(tmpPath)
			http.Error(w, fmt.Sprintf("renaming ISO file: %v", err), http.StatusInternalServerError)
			return
		}

		p.deps.Logger.Infow("ISO upload complete", "name", name, "bytes", written, "path", isoPath)
		break
	}

	if written == 0 {
		http.Error(w, "no iso field found in multipart form", http.StatusBadRequest)
		return
	}

	cdrom.Status.ISOSize = written

	// Configure iSCSI target on RouterOS
	if err := p.configureISCSITarget(r.Context(), cdrom); err != nil {
		p.deps.Logger.Warnw("failed to configure iSCSI target", "name", name, "error", err)
		cdrom.Status.Phase = "Error"
	} else {
		cdrom.Status.Phase = "Ready"
	}

	// Persist updated status
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), name, cdrom); err != nil {
			p.deps.Logger.Warnw("failed to persist iSCSI CDROM after upload", "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, cdrom)
}

// ─── Subscribe/Unsubscribe Handlers ─────────────────────────────────────────

type subscribeRequest struct {
	Name         string `json:"name"`
	InitiatorIQN string `json:"initiatorIQN,omitempty"`
}

func (p *MicroKubeProvider) handleSubscribeISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	var req subscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid subscribe JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "subscriber name is required", http.StatusBadRequest)
		return
	}

	// Check duplicate
	for _, sub := range cdrom.Status.Subscribers {
		if sub.Name == req.Name {
			http.Error(w, fmt.Sprintf("subscriber %q already subscribed", req.Name), http.StatusConflict)
			return
		}
	}

	cdrom.Status.Subscribers = append(cdrom.Status.Subscribers, ISCSISubscriber{
		Name:         req.Name,
		InitiatorIQN: req.InitiatorIQN,
		Since:        time.Now().UTC().Format(time.RFC3339),
	})

	// Persist
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), name, cdrom); err != nil {
			p.deps.Logger.Warnw("failed to persist iSCSI CDROM subscribe", "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, map[string]interface{}{
		"targetIQN":  cdrom.Status.TargetIQN,
		"portalIP":   cdrom.Status.PortalIP,
		"portalPort": cdrom.Status.PortalPort,
	})
}

type unsubscribeRequest struct {
	Name string `json:"name"`
}

func (p *MicroKubeProvider) handleUnsubscribeISCSICdrom(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	var req unsubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid unsubscribe JSON: %v", err), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "subscriber name is required", http.StatusBadRequest)
		return
	}

	// Remove subscriber
	found := false
	filtered := make([]ISCSISubscriber, 0, len(cdrom.Status.Subscribers))
	for _, sub := range cdrom.Status.Subscribers {
		if sub.Name == req.Name {
			found = true
			continue
		}
		filtered = append(filtered, sub)
	}

	if !found {
		http.Error(w, fmt.Sprintf("subscriber %q not found", req.Name), http.StatusNotFound)
		return
	}

	cdrom.Status.Subscribers = filtered

	// Persist
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), name, cdrom); err != nil {
			p.deps.Logger.Warnw("failed to persist iSCSI CDROM unsubscribe", "error", err)
		}
	}

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("subscriber %q removed from iSCSI CDROM %q", req.Name, name),
	})
}

// ─── RouterOS iSCSI Integration ─────────────────────────────────────────────

func (p *MicroKubeProvider) configureISCSITarget(ctx context.Context, cdrom *ISCSICdrom) error {
	ros := p.deps.Runtime
	rosClient, ok := ros.(interface {
		CreateISCSITarget(ctx context.Context, name, filePath string) (string, error)
	})
	if !ok {
		p.deps.Logger.Warnw("runtime does not support iSCSI operations (not RouterOS), skipping")
		return nil
	}

	// Translate container path to host-visible path for RouterOS
	hostPath := p.deps.StorageMgr.HostVisiblePath(cdrom.Status.ISOPath)

	// Create file-backed disk with iSCSI export enabled
	diskID, err := rosClient.CreateISCSITarget(ctx, cdrom.Name, hostPath)
	if err != nil {
		return fmt.Errorf("creating iSCSI target: %w", err)
	}
	cdrom.Status.RouterOSID = diskID

	// RouterOS auto-generates IQN from the slot name. Update status with
	// the actual IQN if we can retrieve it.
	if rosRT, ok2 := ros.(*runtime.RouterOSRuntime); ok2 {
		if disk, err := rosRT.GetISCSIDisk(ctx, diskID); err == nil && disk.ISCSIServerIQN != "" {
			cdrom.Status.TargetIQN = disk.ISCSIServerIQN
		}
	}

	p.deps.Logger.Infow("iSCSI target configured via ROSE /disk",
		"name", cdrom.Name,
		"iqn", cdrom.Status.TargetIQN,
		"diskID", diskID,
		"file", hostPath)

	return nil
}

func (p *MicroKubeProvider) removeISCSITarget(ctx context.Context, cdrom *ISCSICdrom) {
	if cdrom.Status.RouterOSID == "" {
		return
	}
	ros := p.deps.Runtime
	rosClient, ok := ros.(interface {
		RemoveISCSITarget(ctx context.Context, id string) error
	})
	if !ok {
		return
	}

	if err := rosClient.RemoveISCSITarget(ctx, cdrom.Status.RouterOSID); err != nil {
		p.deps.Logger.Warnw("failed to remove iSCSI disk", "id", cdrom.Status.RouterOSID, "error", err)
	}
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchISCSICdroms(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.ISCSICdroms == nil {
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
	for _, cdrom := range p.iscsiCdroms {
		c := cdrom.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}
		evt := K8sWatchEvent{Type: "ADDED", Object: c}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.ISCSICdroms.WatchAll(ctx)
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

			var cdrom ISCSICdrom
			if evt.Type == store.EventDelete {
				cdrom = ISCSICdrom{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &cdrom); err != nil {
					continue
				}
				cdrom.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &cdrom,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func iscsiCdromListToTable(cdroms []ISCSICdrom) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Version", Type: "string"},
			{Name: "Phase", Type: "string"},
			{Name: "ISO Size", Type: "string"},
			{Name: "Target IQN", Type: "string"},
			{Name: "Subscribers", Type: "integer"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range cdroms {
		cdrom := &cdroms[i]

		isoSize := ""
		if cdrom.Status.ISOSize > 0 {
			isoSize = formatISOSize(cdrom.Status.ISOSize)
		}

		age := "<unknown>"
		if !cdrom.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(cdrom.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              cdrom.Name,
				"creationTimestamp": cdrom.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				cdrom.Name,
				cdrom.Spec.Version,
				cdrom.Status.Phase,
				isoSize,
				cdrom.Status.TargetIQN,
				len(cdrom.Status.Subscribers),
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

func formatISOSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1fGi", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1fMi", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1fKi", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// ─── Read File / Derive Handlers ─────────────────────────────────────────────

// handleISCSICdromReadFile reads a file from an ISO or lists a directory.
// GET /api/v1/iscsi-cdroms/{name}/files?path=/EFI/BOOT/grub.cfg
// GET /api/v1/iscsi-cdroms/{name}/files?path=/EFI/BOOT&list=true
func (p *MicroKubeProvider) handleISCSICdromReadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	cdrom, ok := p.iscsiCdroms[name]
	if !ok {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q not found", name), http.StatusNotFound)
		return
	}

	if cdrom.Status.Phase != "Ready" {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q is not ready (phase: %s)", name, cdrom.Status.Phase), http.StatusConflict)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		filePath = "/"
	}

	isoPath := cdrom.Status.ISOPath

	// List directory mode
	if r.URL.Query().Get("list") == "true" {
		entries, err := iso9660ListDirectoryByPath(isoPath, filePath)
		if err != nil {
			http.Error(w, fmt.Sprintf("listing directory: %v", err), http.StatusInternalServerError)
			return
		}

		var result []iso9660DirEntry
		for _, e := range entries {
			result = append(result, iso9660DirEntry{
				Name:  e.Name(),
				Size:  e.DataLen,
				IsDir: e.IsDir,
			})
		}

		podWriteJSON(w, http.StatusOK, result)
		return
	}

	// Read file mode
	data, err := iso9660ReadFileByPath(isoPath, filePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleISCSICdromDerive creates a new ISCSICdrom by copying and patching the ISO.
// POST /api/v1/iscsi-cdroms/{name}/derive
func (p *MicroKubeProvider) handleISCSICdromDerive(w http.ResponseWriter, r *http.Request) {
	baseName := r.PathValue("name")

	baseCdrom, ok := p.iscsiCdroms[baseName]
	if !ok {
		http.Error(w, fmt.Sprintf("base iSCSI CDROM %q not found", baseName), http.StatusNotFound)
		return
	}

	if baseCdrom.Status.Phase != "Ready" {
		http.Error(w, fmt.Sprintf("base iSCSI CDROM %q is not ready (phase: %s)", baseName, baseCdrom.Status.Phase), http.StatusConflict)
		return
	}

	var req iso9660DeriveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid derive request JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "derived CDROM name is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.iscsiCdroms[req.Name]; exists {
		http.Error(w, fmt.Sprintf("iSCSI CDROM %q already exists", req.Name), http.StatusConflict)
		return
	}

	if len(req.Operations) == 0 {
		http.Error(w, "at least one operation is required", http.StatusBadRequest)
		return
	}

	// Decode base64 content in operations
	for i := range req.Operations {
		op := &req.Operations[i]
		if op.Op == "replace" || op.Op == "add" {
			if len(op.Content) == 0 {
				http.Error(w, fmt.Sprintf("operation %d (%s) requires content", i, op.Op), http.StatusBadRequest)
				return
			}
			// Try base64 decode — if it fails, treat content as raw
			decoded, err := base64.StdEncoding.DecodeString(string(op.Content))
			if err == nil {
				op.Content = decoded
			}
		}
	}

	// Copy base ISO to new path
	newISOFile := req.Name + ".iso"
	newISOPath := filepath.Join(isoBasePath, newISOFile)
	tmpPath := newISOPath + ".deriving"

	p.deps.Logger.Infow("deriving ISO", "base", baseName, "new", req.Name, "ops", len(req.Operations))

	if err := iso9660CopyISO(baseCdrom.Status.ISOPath, tmpPath); err != nil {
		http.Error(w, fmt.Sprintf("copying base ISO: %v", err), http.StatusInternalServerError)
		return
	}

	// Apply patch operations
	if err := iso9660ApplyPatches(tmpPath, req.Operations); err != nil {
		os.Remove(tmpPath)
		http.Error(w, fmt.Sprintf("patching ISO: %v", err), http.StatusInternalServerError)
		return
	}

	// Rename temp to final path
	if err := os.Rename(tmpPath, newISOPath); err != nil {
		os.Remove(tmpPath)
		http.Error(w, fmt.Sprintf("renaming ISO: %v", err), http.StatusInternalServerError)
		return
	}

	// Get file size
	fi, err := os.Stat(newISOPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("stat new ISO: %v", err), http.StatusInternalServerError)
		return
	}

	// Inherit version from base if not specified in request
	version := req.Version
	if version == "" {
		version = baseCdrom.Spec.Version
	}

	// Create new ISCSICdrom object
	newCdrom := &ISCSICdrom{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ISCSICdrom"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              req.Name,
			CreationTimestamp: metav1.Now(),
		},
		Spec: ISCSICdromSpec{
			ISOFile:     newISOFile,
			Description: req.Description,
			Version:     version,
			ReadOnly:    true,
			BootConfigs: baseCdrom.Spec.BootConfigs,
			DerivedFrom: baseName,
		},
		Status: ISCSICdromStatus{
			Phase:      "Pending",
			ISOPath:    newISOPath,
			ISOSize:    fi.Size(),
			TargetIQN:  iscsiIQNPrefix + req.Name,
			PortalPort: iscsiDefaultPort,
		},
	}

	// Set portal IP from config
	if len(p.deps.Config.Networks) > 0 {
		newCdrom.Status.PortalIP = p.deps.Config.Networks[0].Gateway
	}

	// Configure iSCSI target on RouterOS
	if err := p.configureISCSITarget(r.Context(), newCdrom); err != nil {
		p.deps.Logger.Warnw("failed to configure iSCSI target for derived CDROM", "name", req.Name, "error", err)
		newCdrom.Status.Phase = "Error"
	} else {
		newCdrom.Status.Phase = "Ready"
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		if _, err := p.deps.Store.ISCSICdroms.PutJSON(r.Context(), req.Name, newCdrom); err != nil {
			http.Error(w, fmt.Sprintf("persisting derived CDROM: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.iscsiCdroms[req.Name] = newCdrom

	p.deps.Logger.Infow("derived ISO created",
		"base", baseName,
		"new", req.Name,
		"size", fi.Size(),
		"phase", newCdrom.Status.Phase,
		"ops", len(req.Operations))

	podWriteJSON(w, http.StatusCreated, newCdrom)
}

// ─── Consistency Checks ─────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkISCSICdromCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.ISCSICdroms != nil {
		storeKeys, err := p.deps.Store.ISCSICdroms.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.iscsiCdroms {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("iscsi-cdrom/%s", name),
						Status:  "pass",
						Message: "iSCSI CDROM CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("iscsi-cdrom/%s", name),
						Status:  "fail",
						Message: "iSCSI CDROM CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("iscsi-cdrom/%s", name),
					Status:  "warn",
					Message: "iSCSI CDROM CRD in NATS but not in memory",
				})
			}
		}
	}

	// Verify ISO files exist for Ready CDROMs
	for _, cdrom := range p.iscsiCdroms {
		if cdrom.Status.Phase != "Ready" {
			continue
		}
		if cdrom.Status.ISOPath == "" {
			continue
		}
		if _, err := os.Stat(cdrom.Status.ISOPath); err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("iscsi-cdrom-iso/%s", cdrom.Name),
				Status:  "fail",
				Message: fmt.Sprintf("ISO file missing: %s", cdrom.Status.ISOPath),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("iscsi-cdrom-iso/%s", cdrom.Name),
				Status:  "pass",
				Message: fmt.Sprintf("ISO file exists: %s", cdrom.Status.ISOPath),
			})
		}
	}

	return items
}
