package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// HostReservation is a namespaced CRD that claims a BareMetalHost for a job pool.
type HostReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              HostReservationSpec   `json:"spec"`
	Status            HostReservationStatus `json:"status,omitempty"`
}

// HostReservationSpec defines the desired state of a HostReservation.
type HostReservationSpec struct {
	BMHRef    string `json:"bmhRef"`              // BareMetalHost name (must exist)
	Pool      string `json:"pool"`                // pool name (matches JobRunner.spec.pool)
	Owner     string `json:"owner,omitempty"`     // user/team
	Purpose   string `json:"purpose,omitempty"`   // human-readable
	ExpiresAt string `json:"expiresAt,omitempty"` // RFC3339 expiry
}

// HostReservationStatus reports the observed state of a HostReservation.
type HostReservationStatus struct {
	Phase      string `json:"phase"`                // Active, Expired, Released
	BMHNetwork string `json:"bmhNetwork,omitempty"` // resolved from BMH
	BMHIP      string `json:"bmhIP,omitempty"`      // resolved from BMH
	ActiveJob  string `json:"activeJob,omitempty"`   // currently running job key
	ReservedAt string `json:"reservedAt,omitempty"`
}

// HostReservationList is a list of HostReservation objects.
type HostReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []HostReservation `json:"items"`
}

// DeepCopy returns a deep copy of the HostReservation.
func (h *HostReservation) DeepCopy() *HostReservation {
	out := *h
	out.ObjectMeta = *h.ObjectMeta.DeepCopy()
	return &out
}

// ─── Store Operations ────────────────────────────────────────────────────────

func (p *MicroKubeProvider) LoadHostReservationsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.HostReservations == nil {
		return
	}

	keys, err := p.deps.Store.HostReservations.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list host reservations from store", "error", err)
		return
	}

	for _, key := range keys {
		var hr HostReservation
		if _, err := p.deps.Store.HostReservations.GetJSON(ctx, key, &hr); err != nil {
			p.deps.Logger.Warnw("failed to read host reservation from store", "key", key, "error", err)
			continue
		}
		p.hostReservations[hr.Namespace+"/"+hr.Name] = &hr
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded host reservations from store", "count", len(keys))
	}
}

func (p *MicroKubeProvider) persistHostReservation(ctx context.Context, hr *HostReservation) {
	if p.deps.Store != nil && p.deps.Store.HostReservations != nil {
		key := hr.Namespace + "." + hr.Name
		if _, err := p.deps.Store.HostReservations.PutJSON(ctx, key, hr); err != nil {
			p.deps.Logger.Warnw("failed to persist HostReservation", "name", hr.Name, "error", err)
		}
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListAllHostReservations(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchHostReservations(w, r)
		return
	}

	items := make([]HostReservation, 0, len(p.hostReservations))
	for _, hr := range p.hostReservations {
		c := hr.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
		items = append(items, *c)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, hostReservationListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, HostReservationList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservationList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedHostReservations(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchHostReservations(w, r)
		return
	}

	ns := r.PathValue("namespace")
	items := make([]HostReservation, 0)
	for _, hr := range p.hostReservations {
		if hr.Namespace == ns {
			c := hr.DeepCopy()
			c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
			items = append(items, *c)
		}
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, hostReservationListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, HostReservationList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservationList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetHostReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	hr, ok := p.hostReservations[key]
	if !ok {
		http.Error(w, fmt.Sprintf("HostReservation %q not found", name), http.StatusNotFound)
		return
	}

	c := hr.DeepCopy()
	c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, hostReservationListToTable([]HostReservation{*c}))
		return
	}

	podWriteJSON(w, http.StatusOK, c)
}

func (p *MicroKubeProvider) handleCreateHostReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var hr HostReservation
	if err := json.NewDecoder(r.Body).Decode(&hr); err != nil {
		http.Error(w, fmt.Sprintf("invalid HostReservation JSON: %v", err), http.StatusBadRequest)
		return
	}

	if hr.Name == "" {
		http.Error(w, "HostReservation name is required", http.StatusBadRequest)
		return
	}

	hr.Namespace = ns
	key := ns + "/" + hr.Name

	if _, exists := p.hostReservations[key]; exists {
		http.Error(w, fmt.Sprintf("HostReservation %q already exists", hr.Name), http.StatusConflict)
		return
	}

	// Validate BMHRef
	if hr.Spec.BMHRef == "" {
		http.Error(w, "spec.bmhRef is required", http.StatusBadRequest)
		return
	}

	if hr.Spec.Pool == "" {
		http.Error(w, "spec.pool is required", http.StatusBadRequest)
		return
	}

	// Check BMH exists
	bmhFound := false
	for _, bmh := range p.bareMetalHosts {
		if bmh.Name == hr.Spec.BMHRef {
			bmhFound = true
			hr.Status.BMHNetwork = bmh.Spec.Network
			hr.Status.BMHIP = bmh.Spec.IP
			break
		}
	}
	if !bmhFound {
		http.Error(w, fmt.Sprintf("BareMetalHost %q not found", hr.Spec.BMHRef), http.StatusBadRequest)
		return
	}

	// Check no other reservation references same BMH
	for _, existing := range p.hostReservations {
		if existing.Spec.BMHRef == hr.Spec.BMHRef {
			http.Error(w, fmt.Sprintf("BareMetalHost %q is already reserved by %s/%s",
				hr.Spec.BMHRef, existing.Namespace, existing.Name), http.StatusConflict)
			return
		}
	}

	hr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
	if hr.CreationTimestamp.IsZero() {
		hr.CreationTimestamp = metav1.Now()
	}
	if hr.Status.Phase == "" {
		hr.Status.Phase = "Active"
	}
	if hr.Status.ReservedAt == "" {
		hr.Status.ReservedAt = time.Now().UTC().Format(time.RFC3339)
	}

	p.persistHostReservation(r.Context(), &hr)
	p.hostReservations[key] = &hr

	podWriteJSON(w, http.StatusCreated, &hr)
}

func (p *MicroKubeProvider) handleUpdateHostReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	old, ok := p.hostReservations[key]
	if !ok {
		http.Error(w, fmt.Sprintf("HostReservation %q not found", name), http.StatusNotFound)
		return
	}

	var hr HostReservation
	if err := json.NewDecoder(r.Body).Decode(&hr); err != nil {
		http.Error(w, fmt.Sprintf("invalid HostReservation JSON: %v", err), http.StatusBadRequest)
		return
	}
	hr.Name = name
	hr.Namespace = ns
	hr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}

	if hr.CreationTimestamp.IsZero() {
		hr.CreationTimestamp = old.CreationTimestamp
	}
	if hr.Status.Phase == "" {
		hr.Status = old.Status
	}

	p.persistHostReservation(r.Context(), &hr)
	p.hostReservations[key] = &hr

	podWriteJSON(w, http.StatusOK, &hr)
}

func (p *MicroKubeProvider) handlePatchHostReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.hostReservations[key]
	if !ok {
		http.Error(w, fmt.Sprintf("HostReservation %q not found", name), http.StatusNotFound)
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
	merged.Namespace = ns
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
	merged.CreationTimestamp = existing.CreationTimestamp

	p.persistHostReservation(r.Context(), merged)
	p.hostReservations[key] = merged

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteHostReservation(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	hr, ok := p.hostReservations[key]
	if !ok {
		http.Error(w, fmt.Sprintf("HostReservation %q not found", name), http.StatusNotFound)
		return
	}

	// Block delete if an active job is running
	if hr.Status.ActiveJob != "" {
		http.Error(w, fmt.Sprintf("HostReservation %q has active job %q — cancel the job first",
			name, hr.Status.ActiveJob), http.StatusConflict)
		return
	}

	if p.deps.Store != nil && p.deps.Store.HostReservations != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.HostReservations.Delete(r.Context(), storeKey); err != nil {
			http.Error(w, fmt.Sprintf("deleting HostReservation from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.hostReservations, key)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("HostReservation %q deleted", name),
	})
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchHostReservations(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.HostReservations == nil {
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

	p.mu.RLock()
	snapshot := make([]*HostReservation, 0, len(p.hostReservations))
	for _, hr := range p.hostReservations {
		snapshot = append(snapshot, hr.DeepCopy())
	}
	p.mu.RUnlock()

	for _, c := range snapshot {
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
		if err := enc.Encode(K8sWatchEvent{Type: "ADDED", Object: c}); err != nil {
			return
		}
		flusher.Flush()
	}

	events, err := p.deps.Store.HostReservations.WatchAll(ctx)
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
			var hr HostReservation
			if evt.Type == store.EventDelete {
				hr = HostReservation{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &hr); err != nil {
					continue
				}
				hr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "HostReservation"}
			}
			if err := enc.Encode(K8sWatchEvent{Type: string(evt.Type), Object: &hr}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func hostReservationListToTable(items []HostReservation) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Pool", Type: "string"},
			{Name: "BMH", Type: "string"},
			{Name: "Owner", Type: "string"},
			{Name: "Status", Type: "string"},
			{Name: "Active-Job", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	for i := range items {
		hr := &items[i]

		age := "<unknown>"
		if !hr.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(hr.CreationTimestamp.Time))
		}

		activeJob := hr.Status.ActiveJob
		if activeJob == "" {
			activeJob = "-"
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              hr.Name,
				"namespace":         hr.Namespace,
				"creationTimestamp": hr.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				hr.Name,
				hr.Spec.Pool,
				hr.Spec.BMHRef,
				hr.Spec.Owner,
				hr.Status.Phase,
				activeJob,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Consistency ────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkHostReservationCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	if p.deps.Store != nil && p.deps.Store.HostReservations != nil {
		storeKeys, err := p.deps.Store.HostReservations.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for key, hr := range p.hostReservations {
				storeKey := hr.Namespace + "." + hr.Name
				if storeSet[storeKey] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("hostreservation/%s", key),
						Status:  "pass",
						Message: "HostReservation CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("hostreservation/%s", key),
						Status:  "fail",
						Message: "HostReservation CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, storeKey)
			}

			for storeKey := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("hostreservation/%s", storeKey),
					Status:  "warn",
					Message: "HostReservation CRD in NATS but not in memory",
				})
			}
		}
	}

	// Verify BMH refs
	for key, hr := range p.hostReservations {
		found := false
		for _, bmh := range p.bareMetalHosts {
			if bmh.Name == hr.Spec.BMHRef {
				found = true
				break
			}
		}
		if !found {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("hostreservation-ref/%s", key),
				Status:  "warn",
				Message: fmt.Sprintf("references BMH %q which does not exist", hr.Spec.BMHRef),
			})
		}
	}

	return items
}
