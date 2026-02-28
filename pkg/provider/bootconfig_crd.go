package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// BootConfig is a cluster-scoped CRD representing a boot configuration template
// (ignition, cloud-init, kickstart, or custom) that can be assigned to BareMetalHost
// objects. Servers fetch their config via source IP lookup during PXE boot.
type BootConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              BootConfigSpec   `json:"spec"`
	Status            BootConfigStatus `json:"status,omitempty"`
}

// BootConfigSpec defines the desired state of a BootConfig.
type BootConfigSpec struct {
	Format      string            `json:"format"`                // "ignition", "cloud-init", "kickstart", "custom"
	Description string            `json:"description,omitempty"` // human-readable
	Data        map[string]string `json:"data"`                  // key=filename, value=content
}

// BootConfigStatus reports the observed state of a BootConfig.
type BootConfigStatus struct {
	Phase      string   `json:"phase"`                 // Active, Inactive
	AssignedTo []string `json:"assignedTo,omitempty"`   // BMH names referencing this config
}

// BootConfigList is a list of BootConfig objects.
type BootConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []BootConfig `json:"items"`
}

// DeepCopy returns a deep copy of the BootConfig.
func (b *BootConfig) DeepCopy() *BootConfig {
	out := *b
	out.ObjectMeta = *b.ObjectMeta.DeepCopy()
	if b.Spec.Data != nil {
		out.Spec.Data = make(map[string]string, len(b.Spec.Data))
		for k, v := range b.Spec.Data {
			out.Spec.Data[k] = v
		}
	}
	out.Status.AssignedTo = append([]string(nil), b.Status.AssignedTo...)
	return &out
}

// ─── Store Operations ────────────────────────────────────────────────────────

// LoadBootConfigsFromStore loads BootConfig objects from the NATS BOOTCONFIGS bucket.
func (p *MicroKubeProvider) LoadBootConfigsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.BootConfigs == nil {
		return
	}

	keys, err := p.deps.Store.BootConfigs.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list boot configs from store", "error", err)
		return
	}

	for _, key := range keys {
		var bc BootConfig
		if _, err := p.deps.Store.BootConfigs.GetJSON(ctx, key, &bc); err != nil {
			p.deps.Logger.Warnw("failed to read boot config from store", "key", key, "error", err)
			continue
		}
		p.bootConfigs[bc.Name] = &bc
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded boot configs from store", "count", len(keys))
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListBootConfigs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchBootConfigs(w, r)
		return
	}

	items := make([]BootConfig, 0, len(p.bootConfigs))
	for _, bc := range p.bootConfigs {
		c := bc.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}
		items = append(items, *c)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, bootConfigListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, BootConfigList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfigList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetBootConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	bc, ok := p.bootConfigs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found", name), http.StatusNotFound)
		return
	}

	c := bc.DeepCopy()
	c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, bootConfigListToTable([]BootConfig{*c}))
		return
	}

	podWriteJSON(w, http.StatusOK, c)
}

func (p *MicroKubeProvider) handleCreateBootConfig(w http.ResponseWriter, r *http.Request) {
	var bc BootConfig
	if err := json.NewDecoder(r.Body).Decode(&bc); err != nil {
		http.Error(w, fmt.Sprintf("invalid BootConfig JSON: %v", err), http.StatusBadRequest)
		return
	}

	if bc.Name == "" {
		http.Error(w, "BootConfig name is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.bootConfigs[bc.Name]; exists {
		http.Error(w, fmt.Sprintf("BootConfig %q already exists", bc.Name), http.StatusConflict)
		return
	}

	bc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}
	if bc.CreationTimestamp.IsZero() {
		bc.CreationTimestamp = metav1.Now()
	}

	// Defaults
	if bc.Spec.Format == "" {
		bc.Spec.Format = "custom"
	}
	if bc.Status.Phase == "" {
		bc.Status.Phase = "Active"
	}

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		if _, err := p.deps.Store.BootConfigs.PutJSON(r.Context(), bc.Name, &bc); err != nil {
			http.Error(w, fmt.Sprintf("persisting BootConfig: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bootConfigs[bc.Name] = &bc

	podWriteJSON(w, http.StatusCreated, &bc)
}

func (p *MicroKubeProvider) handleUpdateBootConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	old, ok := p.bootConfigs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found", name), http.StatusNotFound)
		return
	}

	var bc BootConfig
	if err := json.NewDecoder(r.Body).Decode(&bc); err != nil {
		http.Error(w, fmt.Sprintf("invalid BootConfig JSON: %v", err), http.StatusBadRequest)
		return
	}
	bc.Name = name
	bc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}

	if bc.CreationTimestamp.IsZero() {
		bc.CreationTimestamp = old.CreationTimestamp
	}
	// Preserve status if not provided
	if bc.Status.Phase == "" {
		bc.Status = old.Status
	}

	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		if _, err := p.deps.Store.BootConfigs.PutJSON(r.Context(), name, &bc); err != nil {
			http.Error(w, fmt.Sprintf("persisting BootConfig update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bootConfigs[name] = &bc

	podWriteJSON(w, http.StatusOK, &bc)
}

func (p *MicroKubeProvider) handlePatchBootConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.bootConfigs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found", name), http.StatusNotFound)
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
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}
	merged.CreationTimestamp = existing.CreationTimestamp

	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		if _, err := p.deps.Store.BootConfigs.PutJSON(r.Context(), name, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting BootConfig patch: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bootConfigs[name] = merged

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteBootConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	bc, ok := p.bootConfigs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found", name), http.StatusNotFound)
		return
	}

	// Block delete if BMHs reference this config
	if len(bc.Status.AssignedTo) > 0 {
		http.Error(w, fmt.Sprintf("BootConfig %q is referenced by %d BMH(s): %s — remove references first",
			name, len(bc.Status.AssignedTo), strings.Join(bc.Status.AssignedTo, ", ")), http.StatusConflict)
		return
	}

	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		if err := p.deps.Store.BootConfigs.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting BootConfig from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.bootConfigs, name)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("BootConfig %q deleted", name),
	})
}

// ─── Serve by Name ──────────────────────────────────────────────────────────

// handleServeBootConfig serves raw boot config content by name.
// Usage: coreos-installer --ignition-url http://192.168.200.2:8082/api/v1/bootconfigs/coreos-base/serve
func (p *MicroKubeProvider) handleServeBootConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	bc, ok := p.bootConfigs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found", name), http.StatusNotFound)
		return
	}

	if len(bc.Spec.Data) == 0 {
		http.Error(w, fmt.Sprintf("BootConfig %q has no data entries", bc.Name), http.StatusNotFound)
		return
	}

	// Return the first data entry
	var content string
	for _, v := range bc.Spec.Data {
		content = v
		break
	}

	switch bc.Spec.Format {
	case "ignition":
		w.Header().Set("Content-Type", "application/vnd.coreos.ignition+json")
	case "cloud-init":
		w.Header().Set("Content-Type", "text/yaml")
	case "kickstart":
		w.Header().Set("Content-Type", "text/plain")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, content)
}

// ─── Source IP Lookup ────────────────────────────────────────────────────────

// handleBootConfigLookup serves boot config content based on the caller's source IP.
// Flow: source IP → match BMH with spec.ip == sourceIP → follow bootConfigRef → return data.
// Usage: kernel cmdline ignition.config.url=http://192.168.200.2:8082/api/v1/bootconfig
func (p *MicroKubeProvider) handleBootConfigLookup(w http.ResponseWriter, r *http.Request) {
	// Extract source IP (strip port)
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	// Search all BMH objects for spec.ip == sourceIP
	var matchedBMH *BareMetalHost
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, fmt.Sprintf("no BareMetalHost found with IP %s", sourceIP), http.StatusNotFound)
		return
	}

	if matchedBMH.Spec.BootConfigRef == "" {
		http.Error(w, fmt.Sprintf("BareMetalHost %q has no bootConfigRef", matchedBMH.Name), http.StatusNotFound)
		return
	}

	bc, ok := p.bootConfigs[matchedBMH.Spec.BootConfigRef]
	if !ok {
		http.Error(w, fmt.Sprintf("BootConfig %q not found (referenced by BMH %q)",
			matchedBMH.Spec.BootConfigRef, matchedBMH.Name), http.StatusNotFound)
		return
	}

	if len(bc.Spec.Data) == 0 {
		http.Error(w, fmt.Sprintf("BootConfig %q has no data entries", bc.Name), http.StatusNotFound)
		return
	}

	// Return the first data entry
	var content string
	for _, v := range bc.Spec.Data {
		content = v
		break
	}

	// Set Content-Type by format
	switch bc.Spec.Format {
	case "ignition":
		w.Header().Set("Content-Type", "application/vnd.coreos.ignition+json")
	case "cloud-init":
		w.Header().Set("Content-Type", "text/yaml")
	case "kickstart":
		w.Header().Set("Content-Type", "text/plain")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, content)
}

// handleBootComplete is called by a booting server after install completes.
// Resolves source IP → BMH, sets spec.image to "localboot" so next reboot goes to disk.
// Usage: curl -X POST http://192.168.200.2:8082/api/v1/boot-complete
func (p *MicroKubeProvider) handleBootComplete(w http.ResponseWriter, r *http.Request) {
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	// Find BMH by source IP
	var matchedBMH *BareMetalHost
	var matchedKey string
	for key, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			matchedKey = key
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, fmt.Sprintf("no BareMetalHost found with IP %s", sourceIP), http.StatusNotFound)
		return
	}

	// Set image to localboot
	merged := matchedBMH.DeepCopy()
	merged.Spec.Image = "localboot"

	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := matchedBMH.Namespace + "." + matchedBMH.Name
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(r.Context(), storeKey, merged); err != nil {
			http.Error(w, fmt.Sprintf("persisting BMH update: %v", err), http.StatusInternalServerError)
			return
		}
	}

	p.bareMetalHosts[matchedKey] = merged
	p.deps.Logger.Infow("boot-complete: switched to localboot", "bmh", matchedBMH.Name, "ip", sourceIP)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","host":%q,"image":"localboot"}`, matchedBMH.Name)
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchBootConfigs(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.BootConfigs == nil {
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
	for _, bc := range p.bootConfigs {
		c := bc.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}
		evt := K8sWatchEvent{Type: "ADDED", Object: c}
		if err := enc.Encode(evt); err != nil {
			return
		}
		flusher.Flush()
	}

	// Watch NATS for live updates
	events, err := p.deps.Store.BootConfigs.WatchAll(ctx)
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

			var bc BootConfig
			if evt.Type == store.EventDelete {
				bc = BootConfig{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &bc); err != nil {
					continue
				}
				bc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "BootConfig"}
			}

			watchEvt := K8sWatchEvent{
				Type:   string(evt.Type),
				Object: &bc,
			}

			if err := enc.Encode(watchEvt); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func bootConfigListToTable(configs []BootConfig) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Format", Type: "string"},
			{Name: "Description", Type: "string"},
			{Name: "Assigned", Type: "integer"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range configs {
		bc := &configs[i]

		age := "<unknown>"
		if !bc.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(bc.CreationTimestamp.Time))
		}

		desc := bc.Spec.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              bc.Name,
				"creationTimestamp": bc.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				bc.Name,
				bc.Spec.Format,
				desc,
				len(bc.Status.AssignedTo),
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Consistency Checks ─────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkBootConfigCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Verify memory <-> NATS sync
	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		storeKeys, err := p.deps.Store.BootConfigs.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.bootConfigs {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("bootconfig/%s", name),
						Status:  "pass",
						Message: "BootConfig CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("bootconfig/%s", name),
						Status:  "fail",
						Message: "BootConfig CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("bootconfig/%s", name),
					Status:  "warn",
					Message: "BootConfig CRD in NATS but not in memory",
				})
			}
		}
	}

	// Verify assignedTo references are valid BMHs
	for _, bc := range p.bootConfigs {
		for _, bmhName := range bc.Status.AssignedTo {
			found := false
			for _, bmh := range p.bareMetalHosts {
				if bmh.Name == bmhName && bmh.Spec.BootConfigRef == bc.Name {
					found = true
					break
				}
			}
			if !found {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("bootconfig-ref/%s/%s", bc.Name, bmhName),
					Status:  "warn",
					Message: fmt.Sprintf("assignedTo references BMH %q which no longer references this config", bmhName),
				})
			}
		}
	}

	return items
}

// ─── BMH BootConfigRef Sync ─────────────────────────────────────────────────

// syncBootConfigRef updates BootConfig assignedTo when a BMH's bootConfigRef changes.
func (p *MicroKubeProvider) syncBootConfigRef(ctx context.Context, bmhName, oldRef, newRef string) {
	// Remove from old
	if oldRef != "" && oldRef != newRef {
		if bc, ok := p.bootConfigs[oldRef]; ok {
			filtered := make([]string, 0, len(bc.Status.AssignedTo))
			for _, n := range bc.Status.AssignedTo {
				if n != bmhName {
					filtered = append(filtered, n)
				}
			}
			bc.Status.AssignedTo = filtered
			p.persistBootConfig(ctx, bc)
		}
	}

	// Add to new
	if newRef != "" {
		if bc, ok := p.bootConfigs[newRef]; ok {
			// Check if already present
			found := false
			for _, n := range bc.Status.AssignedTo {
				if n == bmhName {
					found = true
					break
				}
			}
			if !found {
				bc.Status.AssignedTo = append(bc.Status.AssignedTo, bmhName)
				p.persistBootConfig(ctx, bc)
			}
		}
	}
}

// removeBootConfigRef removes a BMH from its referenced BootConfig's assignedTo list.
func (p *MicroKubeProvider) removeBootConfigRef(ctx context.Context, bmhName, ref string) {
	if ref == "" {
		return
	}
	bc, ok := p.bootConfigs[ref]
	if !ok {
		return
	}
	filtered := make([]string, 0, len(bc.Status.AssignedTo))
	for _, n := range bc.Status.AssignedTo {
		if n != bmhName {
			filtered = append(filtered, n)
		}
	}
	bc.Status.AssignedTo = filtered
	p.persistBootConfig(ctx, bc)
}

func (p *MicroKubeProvider) persistBootConfig(ctx context.Context, bc *BootConfig) {
	if p.deps.Store != nil && p.deps.Store.BootConfigs != nil {
		if _, err := p.deps.Store.BootConfigs.PutJSON(ctx, bc.Name, bc); err != nil {
			p.deps.Logger.Warnw("failed to persist BootConfig", "name", bc.Name, "error", err)
		}
	}
}
