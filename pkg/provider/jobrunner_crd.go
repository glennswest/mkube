package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// JobRunner is a cluster-scoped CRD that defines a runner template for a job pool.
type JobRunner struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              JobRunnerSpec   `json:"spec"`
	Status            JobRunnerStatus `json:"status,omitempty"`
}

// WorkSchedule defines when hosts in a pool should be kept alive.
type WorkSchedule struct {
	Days     []string `json:"days"`               // "Mon","Tue","Wed","Thu","Fri","Sat","Sun"
	Start    string   `json:"start"`              // "09:00" (24h format)
	End      string   `json:"end"`                // "17:00"
	Timezone string   `json:"timezone,omitempty"` // IANA timezone (default "Local")
}

// IsActive returns true if the given time falls within the schedule window.
func (s *WorkSchedule) IsActive(now time.Time) bool {
	if s == nil || len(s.Days) == 0 || s.Start == "" || s.End == "" {
		return false
	}

	loc := time.Local
	if s.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(s.Timezone)
		if err != nil {
			return false
		}
	}

	t := now.In(loc)
	dayName := t.Weekday().String()[:3] // "Mon", "Tue", etc.

	dayMatch := false
	for _, d := range s.Days {
		if strings.EqualFold(d, dayName) {
			dayMatch = true
			break
		}
	}
	if !dayMatch {
		return false
	}

	startParts := strings.SplitN(s.Start, ":", 2)
	endParts := strings.SplitN(s.End, ":", 2)
	if len(startParts) != 2 || len(endParts) != 2 {
		return false
	}

	startH, startM := 0, 0
	fmt.Sscanf(startParts[0], "%d", &startH)
	fmt.Sscanf(startParts[1], "%d", &startM)
	endH, endM := 0, 0
	fmt.Sscanf(endParts[0], "%d", &endH)
	fmt.Sscanf(endParts[1], "%d", &endM)

	startMin := startH*60 + startM
	endMin := endH*60 + endM
	nowMin := t.Hour()*60 + t.Minute()

	return nowMin >= startMin && nowMin < endMin
}

// FormatSummary returns a human-readable summary like "Mon-Fri 09:00-17:00".
func (s *WorkSchedule) FormatSummary() string {
	if s == nil || len(s.Days) == 0 {
		return "-"
	}

	// Try to compress consecutive days into a range
	dayOrder := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	dayIdx := map[string]int{"Mon": 0, "Tue": 1, "Wed": 2, "Thu": 3, "Fri": 4, "Sat": 5, "Sun": 6}

	indices := make([]int, 0, len(s.Days))
	for _, d := range s.Days {
		if idx, ok := dayIdx[d]; ok {
			indices = append(indices, idx)
		}
	}
	sort.Ints(indices)

	// Check if consecutive
	consecutive := len(indices) > 1
	for i := 1; i < len(indices); i++ {
		if indices[i] != indices[i-1]+1 {
			consecutive = false
			break
		}
	}

	var daysStr string
	if consecutive && len(indices) > 2 {
		daysStr = dayOrder[indices[0]] + "-" + dayOrder[indices[len(indices)-1]]
	} else {
		names := make([]string, len(indices))
		for i, idx := range indices {
			names[i] = dayOrder[idx]
		}
		daysStr = strings.Join(names, ",")
	}

	result := daysStr + " " + s.Start + "-" + s.End
	if s.Timezone != "" && s.Timezone != "Local" {
		result += " " + s.Timezone
	}
	return result
}

// JobRunnerSpec defines the desired state of a JobRunner.
type JobRunnerSpec struct {
	Pool          string            `json:"pool"`                    // pool name
	BootConfigRef string            `json:"bootConfigRef,omitempty"` // BootConfig name (legacy — use Template for cloudid)
	Template      string            `json:"template,omitempty"`      // cloudid template ref (takes precedence over bootConfigRef)
	Image         string            `json:"image,omitempty"`         // iSCSI CDROM or PXE image
	IdleTimeout   int               `json:"idleTimeout,omitempty"`   // seconds before powering off idle host
	ReclaimPolicy string            `json:"reclaimPolicy,omitempty"` // PowerOff (default), Retain
	AllowOverflow bool              `json:"allowOverflow,omitempty"` // use unreserved BMHs as overflow
	MaxConcurrent int               `json:"maxConcurrent,omitempty"` // max concurrent jobs (0=unlimited)
	Labels        map[string]string `json:"labels,omitempty"`        // constraint labels
	Schedule      *WorkSchedule     `json:"schedule,omitempty"`      // keep hosts alive during these hours
}

// JobRunnerStatus reports the observed state of a JobRunner.
type JobRunnerStatus struct {
	Phase          string `json:"phase"`          // Active, Suspended
	ReservedHosts  int    `json:"reservedHosts"`
	ActiveJobs     int    `json:"activeJobs"`
	TotalCompleted int    `json:"totalCompleted"`
	TotalFailed    int    `json:"totalFailed"`
}

// JobRunnerList is a list of JobRunner objects.
type JobRunnerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []JobRunner `json:"items"`
}

// DeepCopy returns a deep copy of the JobRunner.
func (j *JobRunner) DeepCopy() *JobRunner {
	out := *j
	out.ObjectMeta = *j.ObjectMeta.DeepCopy()
	if j.Spec.Labels != nil {
		out.Spec.Labels = make(map[string]string, len(j.Spec.Labels))
		for k, v := range j.Spec.Labels {
			out.Spec.Labels[k] = v
		}
	}
	if j.Spec.Schedule != nil {
		s := *j.Spec.Schedule
		s.Days = make([]string, len(j.Spec.Schedule.Days))
		copy(s.Days, j.Spec.Schedule.Days)
		out.Spec.Schedule = &s
	}
	return &out
}

// ─── Store Operations ────────────────────────────────────────────────────────

func (p *MicroKubeProvider) LoadJobRunnersFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.JobRunners == nil {
		return
	}

	keys, err := p.deps.Store.JobRunners.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list job runners from store", "error", err)
		return
	}

	for _, key := range keys {
		var jr JobRunner
		if _, err := p.deps.Store.JobRunners.GetJSON(ctx, key, &jr); err != nil {
			p.deps.Logger.Warnw("failed to read job runner from store", "key", key, "error", err)
			continue
		}
		p.jobRunners[jr.Name] = &jr
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded job runners from store", "count", len(keys))
	}
}

func (p *MicroKubeProvider) persistJobRunner(ctx context.Context, jr *JobRunner) {
	if p.deps.Store != nil && p.deps.Store.JobRunners != nil {
		if _, err := p.deps.Store.JobRunners.PutJSON(ctx, jr.Name, jr); err != nil {
			p.deps.Logger.Warnw("failed to persist JobRunner", "name", jr.Name, "error", err)
		}
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListJobRunners(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchJobRunners(w, r)
		return
	}

	items := make([]JobRunner, 0, len(p.jobRunners))
	for _, jr := range p.jobRunners {
		c := jr.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
		// Enrich status
		p.enrichJobRunnerStatus(c)
		items = append(items, *c)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobRunnerListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, JobRunnerList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunnerList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetJobRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	jr, ok := p.jobRunners[name]
	if !ok {
		http.Error(w, fmt.Sprintf("JobRunner %q not found", name), http.StatusNotFound)
		return
	}

	c := jr.DeepCopy()
	c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
	p.enrichJobRunnerStatus(c)

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobRunnerListToTable([]JobRunner{*c}))
		return
	}

	podWriteJSON(w, http.StatusOK, c)
}

func (p *MicroKubeProvider) handleCreateJobRunner(w http.ResponseWriter, r *http.Request) {
	var jr JobRunner
	if err := json.NewDecoder(r.Body).Decode(&jr); err != nil {
		http.Error(w, fmt.Sprintf("invalid JobRunner JSON: %v", err), http.StatusBadRequest)
		return
	}

	if jr.Name == "" {
		http.Error(w, "JobRunner name is required", http.StatusBadRequest)
		return
	}

	if _, exists := p.jobRunners[jr.Name]; exists {
		http.Error(w, fmt.Sprintf("JobRunner %q already exists", jr.Name), http.StatusConflict)
		return
	}

	if jr.Spec.Pool == "" {
		http.Error(w, "spec.pool is required", http.StatusBadRequest)
		return
	}
	if jr.Spec.Template == "" && jr.Spec.BootConfigRef == "" {
		http.Error(w, "spec.template or spec.bootConfigRef is required", http.StatusBadRequest)
		return
	}

	// Validate BootConfigRef if set (not required when using cloudid Template)
	if jr.Spec.BootConfigRef != "" {
		if _, ok := p.bootConfigs[jr.Spec.BootConfigRef]; !ok {
			http.Error(w, fmt.Sprintf("BootConfig %q not found", jr.Spec.BootConfigRef), http.StatusBadRequest)
			return
		}
	}

	jr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
	if jr.CreationTimestamp.IsZero() {
		jr.CreationTimestamp = metav1.Now()
	}
	if jr.Spec.ReclaimPolicy == "" {
		jr.Spec.ReclaimPolicy = "PowerOff"
	}
	if jr.Status.Phase == "" {
		jr.Status.Phase = "Active"
	}

	p.persistJobRunner(r.Context(), &jr)
	p.jobRunners[jr.Name] = &jr

	podWriteJSON(w, http.StatusCreated, &jr)
}

func (p *MicroKubeProvider) handleUpdateJobRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	old, ok := p.jobRunners[name]
	if !ok {
		http.Error(w, fmt.Sprintf("JobRunner %q not found", name), http.StatusNotFound)
		return
	}

	var jr JobRunner
	if err := json.NewDecoder(r.Body).Decode(&jr); err != nil {
		http.Error(w, fmt.Sprintf("invalid JobRunner JSON: %v", err), http.StatusBadRequest)
		return
	}
	jr.Name = name
	jr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}

	if jr.CreationTimestamp.IsZero() {
		jr.CreationTimestamp = old.CreationTimestamp
	}
	if jr.Status.Phase == "" {
		jr.Status = old.Status
	}

	p.persistJobRunner(r.Context(), &jr)
	p.jobRunners[name] = &jr

	podWriteJSON(w, http.StatusOK, &jr)
}

func (p *MicroKubeProvider) handlePatchJobRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	existing, ok := p.jobRunners[name]
	if !ok {
		http.Error(w, fmt.Sprintf("JobRunner %q not found", name), http.StatusNotFound)
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
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
	merged.CreationTimestamp = existing.CreationTimestamp

	p.persistJobRunner(r.Context(), merged)
	p.jobRunners[name] = merged

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteJobRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	jr, ok := p.jobRunners[name]
	if !ok {
		http.Error(w, fmt.Sprintf("JobRunner %q not found", name), http.StatusNotFound)
		return
	}

	// Block delete if active jobs exist
	activeCount := 0
	for _, job := range p.jobs {
		if job.Status.RunnerRef == name && (job.Status.Phase == "Running" || job.Status.Phase == "Provisioning" || job.Status.Phase == "Scheduling") {
			activeCount++
		}
	}
	if activeCount > 0 {
		http.Error(w, fmt.Sprintf("JobRunner %q has %d active job(s) — cancel them first",
			name, activeCount), http.StatusConflict)
		return
	}
	_ = jr

	if p.deps.Store != nil && p.deps.Store.JobRunners != nil {
		if err := p.deps.Store.JobRunners.Delete(r.Context(), name); err != nil {
			http.Error(w, fmt.Sprintf("deleting JobRunner from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	delete(p.jobRunners, name)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("JobRunner %q deleted", name),
	})
}

// enrichJobRunnerStatus computes live status from current state.
func (p *MicroKubeProvider) enrichJobRunnerStatus(jr *JobRunner) {
	jr.Status.ReservedHosts = 0
	jr.Status.ActiveJobs = 0
	jr.Status.TotalCompleted = 0
	jr.Status.TotalFailed = 0

	for _, hr := range p.hostReservations {
		if hr.Spec.Pool == jr.Spec.Pool {
			jr.Status.ReservedHosts++
		}
	}

	for _, job := range p.jobs {
		if job.Spec.Pool != jr.Spec.Pool {
			continue
		}
		switch job.Status.Phase {
		case "Running", "Provisioning", "Scheduling":
			jr.Status.ActiveJobs++
		case "Completed":
			jr.Status.TotalCompleted++
		case "Failed", "TimedOut":
			jr.Status.TotalFailed++
		}
	}
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchJobRunners(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.JobRunners == nil {
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
	snapshot := make([]*JobRunner, 0, len(p.jobRunners))
	for _, jr := range p.jobRunners {
		snapshot = append(snapshot, jr.DeepCopy())
	}
	p.mu.RUnlock()

	for _, c := range snapshot {
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
		if err := enc.Encode(K8sWatchEvent{Type: "ADDED", Object: c}); err != nil {
			return
		}
		flusher.Flush()
	}

	events, err := p.deps.Store.JobRunners.WatchAll(ctx)
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
			var jr JobRunner
			if evt.Type == store.EventDelete {
				jr = JobRunner{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &jr); err != nil {
					continue
				}
				jr.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "JobRunner"}
			}
			if err := enc.Encode(K8sWatchEvent{Type: string(evt.Type), Object: &jr}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func jobRunnerListToTable(items []JobRunner) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Pool", Type: "string"},
			{Name: "Boot-Config", Type: "string"},
			{Name: "Hosts", Type: "integer"},
			{Name: "Active", Type: "integer"},
			{Name: "Completed", Type: "integer"},
			{Name: "Failed", Type: "integer"},
			{Name: "Idle-Timeout", Type: "string"},
			{Name: "Schedule", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	for i := range items {
		jr := &items[i]

		age := "<unknown>"
		if !jr.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(jr.CreationTimestamp.Time))
		}

		idleTimeout := "-"
		if jr.Spec.IdleTimeout > 0 {
			idleTimeout = fmt.Sprintf("%ds", jr.Spec.IdleTimeout)
		}

		schedule := "-"
		if jr.Spec.Schedule != nil {
			schedule = jr.Spec.Schedule.FormatSummary()
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              jr.Name,
				"creationTimestamp": jr.CreationTimestamp.Format(time.RFC3339),
			},
		})

		// Show Template (cloudid) or BootConfigRef (legacy)
		configRef := jr.Spec.BootConfigRef
		if jr.Spec.Template != "" {
			configRef = "tpl:" + jr.Spec.Template
		}

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				jr.Name,
				jr.Spec.Pool,
				configRef,
				jr.Status.ReservedHosts,
				jr.Status.ActiveJobs,
				jr.Status.TotalCompleted,
				jr.Status.TotalFailed,
				idleTimeout,
				schedule,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Consistency ────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkJobRunnerCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	if p.deps.Store != nil && p.deps.Store.JobRunners != nil {
		storeKeys, err := p.deps.Store.JobRunners.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for name := range p.jobRunners {
				if storeSet[name] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("jobrunner/%s", name),
						Status:  "pass",
						Message: "JobRunner CRD synced with NATS",
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("jobrunner/%s", name),
						Status:  "fail",
						Message: "JobRunner CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, name)
			}

			for name := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("jobrunner/%s", name),
					Status:  "warn",
					Message: "JobRunner CRD in NATS but not in memory",
				})
			}
		}
	}

	// Validate provisioning config (Template or BootConfigRef)
	for name, jr := range p.jobRunners {
		if jr.Spec.Template != "" {
			// cloudid template — no local validation (cloudid resolves templates)
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("jobrunner-ref/%s", name),
				Status:  "pass",
				Message: fmt.Sprintf("uses cloudid template %q", jr.Spec.Template),
			})
		} else if jr.Spec.BootConfigRef != "" {
			if _, ok := p.bootConfigs[jr.Spec.BootConfigRef]; !ok {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("jobrunner-ref/%s", name),
					Status:  "warn",
					Message: fmt.Sprintf("references BootConfig %q which does not exist", jr.Spec.BootConfigRef),
				})
			}
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("jobrunner-ref/%s", name),
				Status:  "warn",
				Message: "has neither template nor bootConfigRef",
			})
		}
	}

	return items
}
