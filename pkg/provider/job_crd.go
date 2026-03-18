package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/glennswest/mkube/pkg/store"
)

// ─── Types ──────────────────────────────────────────────────────────────────

// Job is a namespaced CRD representing a unit of work to execute on a bare metal host.
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              JobSpec   `json:"spec"`
	Status            JobStatus `json:"status,omitempty"`
}

// JobSpec defines the desired state of a Job.
type JobSpec struct {
	Pool      string            `json:"pool"`                // target pool
	Priority  int               `json:"priority,omitempty"`  // higher = first (default 0)
	Script    string            `json:"script"`              // bash script to execute
	Image     string            `json:"image,omitempty"`     // optional container image
	Env       map[string]string `json:"env,omitempty"`       // environment variables
	Timeout   int               `json:"timeout,omitempty"`   // max running seconds (0=no limit)
	Labels    map[string]string `json:"labels,omitempty"`    // constraint labels
	Artifacts []ArtifactSpec    `json:"artifacts,omitempty"` // files to collect after completion
}

// ArtifactSpec defines a file to collect from the host after job completion.
type ArtifactSpec struct {
	Path string `json:"path"` // path on host
	Name string `json:"name"` // artifact name
}

// JobStatus reports the observed state of a Job.
type JobStatus struct {
	Phase         string `json:"phase"`                   // Pending, Scheduling, Provisioning, Running, Completed, Failed, TimedOut, Cancelled
	BMHRef        string `json:"bmhRef,omitempty"`        // assigned BMH
	RunnerRef     string `json:"runnerRef,omitempty"`     // matched JobRunner
	HostIP        string `json:"hostIP,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	CompletedAt   string `json:"completedAt,omitempty"`
	ExitCode      *int   `json:"exitCode,omitempty"`
	ErrorMessage  string `json:"errorMessage,omitempty"`
	LogLines      int    `json:"logLines,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
}

// JobList is a list of Job objects.
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Job `json:"items"`
}

// DeepCopy returns a deep copy of the Job.
func (j *Job) DeepCopy() *Job {
	out := *j
	out.ObjectMeta = *j.ObjectMeta.DeepCopy()
	if j.Spec.Env != nil {
		out.Spec.Env = make(map[string]string, len(j.Spec.Env))
		for k, v := range j.Spec.Env {
			out.Spec.Env[k] = v
		}
	}
	if j.Spec.Labels != nil {
		out.Spec.Labels = make(map[string]string, len(j.Spec.Labels))
		for k, v := range j.Spec.Labels {
			out.Spec.Labels[k] = v
		}
	}
	if j.Spec.Artifacts != nil {
		out.Spec.Artifacts = make([]ArtifactSpec, len(j.Spec.Artifacts))
		copy(out.Spec.Artifacts, j.Spec.Artifacts)
	}
	if j.Status.ExitCode != nil {
		ec := *j.Status.ExitCode
		out.Status.ExitCode = &ec
	}
	return &out
}

// jobKey returns the memory map key for a Job.
func jobKey(j *Job) string {
	return j.Namespace + "/" + j.Name
}

// ─── Store Operations ────────────────────────────────────────────────────────

func (p *MicroKubeProvider) LoadJobsFromStore(ctx context.Context) {
	if p.deps.Store == nil || p.deps.Store.Jobs == nil {
		return
	}

	keys, err := p.deps.Store.Jobs.Keys(ctx, "")
	if err != nil {
		p.deps.Logger.Warnw("failed to list jobs from store", "error", err)
		return
	}

	for _, key := range keys {
		var job Job
		if _, err := p.deps.Store.Jobs.GetJSON(ctx, key, &job); err != nil {
			p.deps.Logger.Warnw("failed to read job from store", "key", key, "error", err)
			continue
		}
		p.jobs[job.Namespace+"/"+job.Name] = &job
	}

	if len(keys) > 0 {
		p.deps.Logger.Infow("loaded jobs from store", "count", len(keys))
	}
}

func (p *MicroKubeProvider) persistJob(ctx context.Context, job *Job) {
	if p.deps.Store != nil && p.deps.Store.Jobs != nil {
		key := job.Namespace + "." + job.Name
		if _, err := p.deps.Store.Jobs.PutJSON(ctx, key, job); err != nil {
			p.deps.Logger.Warnw("failed to persist Job", "name", job.Name, "error", err)
		}
	}
}

// ─── CRUD Handlers ──────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleListAllJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchJobs(w, r)
		return
	}

	items := make([]Job, 0, len(p.jobs))
	for _, job := range p.jobs {
		c := job.DeepCopy()
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
		items = append(items, *c)
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, JobList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "JobList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("watch") == "true" {
		p.handleWatchJobs(w, r)
		return
	}

	ns := r.PathValue("namespace")
	items := make([]Job, 0)
	for _, job := range p.jobs {
		if job.Namespace == ns {
			c := job.DeepCopy()
			c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
			items = append(items, *c)
		}
	}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobListToTable(items))
		return
	}

	podWriteJSON(w, http.StatusOK, JobList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "JobList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	job, ok := p.jobs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
		return
	}

	c := job.DeepCopy()
	c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobListToTable([]Job{*c}))
		return
	}

	podWriteJSON(w, http.StatusOK, c)
}

func (p *MicroKubeProvider) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	var job Job
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, fmt.Sprintf("invalid Job JSON: %v", err), http.StatusBadRequest)
		return
	}

	if job.Name == "" {
		http.Error(w, "Job name is required", http.StatusBadRequest)
		return
	}

	job.Namespace = ns
	key := ns + "/" + job.Name

	if _, exists := p.jobs[key]; exists {
		http.Error(w, fmt.Sprintf("Job %q already exists", job.Name), http.StatusConflict)
		return
	}

	if job.Spec.Pool == "" {
		http.Error(w, "spec.pool is required", http.StatusBadRequest)
		return
	}
	if job.Spec.Script == "" {
		http.Error(w, "spec.script is required", http.StatusBadRequest)
		return
	}

	// Verify a runner exists for this pool
	runnerFound := false
	for _, jr := range p.jobRunners {
		if jr.Spec.Pool == job.Spec.Pool {
			runnerFound = true
			break
		}
	}
	if !runnerFound {
		http.Error(w, fmt.Sprintf("no JobRunner found for pool %q", job.Spec.Pool), http.StatusBadRequest)
		return
	}

	job.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
	if job.CreationTimestamp.IsZero() {
		job.CreationTimestamp = metav1.Now()
	}
	if job.Status.Phase == "" {
		job.Status.Phase = "Pending"
	}

	p.persistJob(r.Context(), &job)
	p.jobs[key] = &job
	p.triggerScheduler()

	podWriteJSON(w, http.StatusCreated, &job)
}

func (p *MicroKubeProvider) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	old, ok := p.jobs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
		return
	}

	var job Job
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, fmt.Sprintf("invalid Job JSON: %v", err), http.StatusBadRequest)
		return
	}
	job.Name = name
	job.Namespace = ns
	job.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}

	if job.CreationTimestamp.IsZero() {
		job.CreationTimestamp = old.CreationTimestamp
	}
	if job.Status.Phase == "" {
		job.Status = old.Status
	}

	p.persistJob(r.Context(), &job)
	p.jobs[key] = &job
	p.triggerScheduler()

	podWriteJSON(w, http.StatusOK, &job)
}

func (p *MicroKubeProvider) handlePatchJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	existing, ok := p.jobs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
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
	merged.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
	merged.CreationTimestamp = existing.CreationTimestamp

	p.persistJob(r.Context(), merged)
	p.jobs[key] = merged
	p.triggerScheduler()

	podWriteJSON(w, http.StatusOK, merged)
}

func (p *MicroKubeProvider) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	job, ok := p.jobs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
		return
	}

	// If job is running, must cancel first
	if job.Status.Phase == "Running" || job.Status.Phase == "Provisioning" || job.Status.Phase == "Scheduling" {
		http.Error(w, fmt.Sprintf("Job %q is %s — cancel it first", name, job.Status.Phase), http.StatusConflict)
		return
	}

	// Clean up log buffer
	p.deleteJobLogs(key)

	if p.deps.Store != nil && p.deps.Store.Jobs != nil {
		storeKey := ns + "." + name
		if err := p.deps.Store.Jobs.Delete(r.Context(), storeKey); err != nil {
			http.Error(w, fmt.Sprintf("deleting Job from store: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Clean up job log from NATS
	if p.deps.Store != nil && p.deps.Store.JobLogs != nil {
		logKey := ns + "." + name
		_ = p.deps.Store.JobLogs.Delete(r.Context(), logKey)
	}

	delete(p.jobs, key)

	podWriteJSON(w, http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   "Success",
		Message:  fmt.Sprintf("Job %q deleted", name),
	})
}

// ─── Cancel ─────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	job, ok := p.jobs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
		return
	}

	if job.Status.Phase == "Completed" || job.Status.Phase == "Failed" || job.Status.Phase == "TimedOut" || job.Status.Phase == "Cancelled" {
		http.Error(w, fmt.Sprintf("Job %q is already %s", name, job.Status.Phase), http.StatusConflict)
		return
	}

	job.Status.Phase = "Cancelled"
	job.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	job.Status.ErrorMessage = "cancelled by user"

	// Release host reservation
	p.releaseJobHost(r.Context(), job)

	p.persistJob(r.Context(), job)

	podWriteJSON(w, http.StatusOK, job)
}

// releaseJobHost clears activeJob from the host reservation.
func (p *MicroKubeProvider) releaseJobHost(ctx context.Context, job *Job) {
	if job.Status.BMHRef == "" {
		return
	}
	for _, hr := range p.hostReservations {
		if hr.Spec.BMHRef == job.Status.BMHRef && hr.Status.ActiveJob == jobKey(job) {
			hr.Status.ActiveJob = ""
			p.persistHostReservation(ctx, hr)
			break
		}
	}
}

// ─── Job Queue (computed view) ──────────────────────────────────────────────

func (p *MicroKubeProvider) handleGetJobQueue(w http.ResponseWriter, r *http.Request) {
	var pending []Job
	for _, job := range p.jobs {
		if job.Status.Phase == "Pending" {
			c := job.DeepCopy()
			c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
			pending = append(pending, *c)
		}
	}

	// Sort by priority DESC, then creation time ASC
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Spec.Priority != pending[j].Spec.Priority {
			return pending[i].Spec.Priority > pending[j].Spec.Priority
		}
		return pending[i].CreationTimestamp.Before(&pending[j].CreationTimestamp)
	})

	if wantsTable(r) {
		podWriteJSON(w, http.StatusOK, jobQueueToTable(pending))
		return
	}

	podWriteJSON(w, http.StatusOK, JobList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "JobList"},
		Items:    pending,
	})
}

func jobQueueToTable(items []Job) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "#", Type: "integer"},
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Namespace", Type: "string"},
			{Name: "Pool", Type: "string"},
			{Name: "Priority", Type: "integer"},
			{Name: "Age", Type: "string"},
		},
	}

	for i := range items {
		job := &items[i]

		age := "<unknown>"
		if !job.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(job.CreationTimestamp.Time))
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              job.Name,
				"namespace":         job.Namespace,
				"creationTimestamp": job.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				i + 1,
				job.Name,
				job.Namespace,
				job.Spec.Pool,
				job.Spec.Priority,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Agent Endpoints ────────────────────────────────────────────────────────

// handleAgentWork returns the assigned job for the calling agent (source IP lookup).
func (p *MicroKubeProvider) handleAgentWork(w http.ResponseWriter, r *http.Request) {
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	// Find BMH by source IP
	var matchedBMH *BareMetalHost
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, fmt.Sprintf("no BMH found for IP %s", sourceIP), http.StatusNotFound)
		return
	}

	// Find job assigned to this BMH
	for _, job := range p.jobs {
		if job.Status.BMHRef == matchedBMH.Name &&
			(job.Status.Phase == "Provisioning" || job.Status.Phase == "Running") {
			c := job.DeepCopy()
			c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}

			// Transition from Provisioning to Running on first work pull
			if job.Status.Phase == "Provisioning" {
				job.Status.Phase = "Running"
				job.Status.StartedAt = time.Now().UTC().Format(time.RFC3339)
				job.Status.LastHeartbeat = job.Status.StartedAt
				p.persistJob(r.Context(), job)
				c.Status = job.Status
			}

			podWriteJSON(w, http.StatusOK, c)
			return
		}
	}

	// No job found — 204 No Content
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentHeartbeat updates the last heartbeat time for the job.
func (p *MicroKubeProvider) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	var matchedBMH *BareMetalHost
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, "no BMH found", http.StatusNotFound)
		return
	}

	for _, job := range p.jobs {
		if job.Status.BMHRef == matchedBMH.Name && job.Status.Phase == "Running" {
			job.Status.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
			p.persistJob(r.Context(), job)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	http.Error(w, "no running job found", http.StatusNotFound)
}

// handleAgentLogs accepts log lines from the agent.
func (p *MicroKubeProvider) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	var matchedBMH *BareMetalHost
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, "no BMH found", http.StatusNotFound)
		return
	}

	var matchedJob *Job
	for _, job := range p.jobs {
		if job.Status.BMHRef == matchedBMH.Name && job.Status.Phase == "Running" {
			matchedJob = job
			break
		}
	}

	if matchedJob == nil {
		http.Error(w, "no running job found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	key := jobKey(matchedJob)
	p.appendJobLogs(r.Context(), key, lines)
	matchedJob.Status.LogLines += len(lines)
	matchedJob.Status.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)

	w.WriteHeader(http.StatusOK)
}

// agentCompleteRequest is the JSON body for POST /api/v1/agent/complete.
type agentCompleteRequest struct {
	ExitCode     int    `json:"exitCode"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// handleAgentComplete marks the job as completed/failed.
func (p *MicroKubeProvider) handleAgentComplete(w http.ResponseWriter, r *http.Request) {
	sourceIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		sourceIP = r.RemoteAddr
	}

	var matchedBMH *BareMetalHost
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.IP == sourceIP {
			matchedBMH = bmh
			break
		}
	}

	if matchedBMH == nil {
		http.Error(w, "no BMH found", http.StatusNotFound)
		return
	}

	var matchedJob *Job
	for _, job := range p.jobs {
		if job.Status.BMHRef == matchedBMH.Name && job.Status.Phase == "Running" {
			matchedJob = job
			break
		}
	}

	if matchedJob == nil {
		http.Error(w, "no running job found", http.StatusNotFound)
		return
	}

	var req agentCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	matchedJob.Status.ExitCode = &req.ExitCode
	matchedJob.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	matchedJob.Status.ErrorMessage = req.ErrorMessage

	if req.ExitCode == 0 {
		matchedJob.Status.Phase = "Completed"
	} else {
		matchedJob.Status.Phase = "Failed"
	}

	// Release host reservation
	p.releaseJobHost(r.Context(), matchedJob)

	p.persistJob(r.Context(), matchedJob)
	p.triggerScheduler()

	p.deps.Logger.Infow("job completed",
		"job", jobKey(matchedJob),
		"phase", matchedJob.Status.Phase,
		"exitCode", req.ExitCode,
	)

	podWriteJSON(w, http.StatusOK, matchedJob)
}

// ─── Job Logs Endpoint ──────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleGetJobLogs(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	key := ns + "/" + name

	if _, ok := p.jobs[key]; !ok {
		http.Error(w, fmt.Sprintf("Job %q not found", name), http.StatusNotFound)
		return
	}

	lines := p.getJobLogs(r.Context(), key)

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

// ─── Watch ──────────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) handleWatchJobs(w http.ResponseWriter, r *http.Request) {
	if p.deps.Store == nil || p.deps.Store.Jobs == nil {
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
	snapshot := make([]*Job, 0, len(p.jobs))
	for _, job := range p.jobs {
		snapshot = append(snapshot, job.DeepCopy())
	}
	p.mu.RUnlock()

	for _, c := range snapshot {
		c.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
		if err := enc.Encode(K8sWatchEvent{Type: "ADDED", Object: c}); err != nil {
			return
		}
		flusher.Flush()
	}

	events, err := p.deps.Store.Jobs.WatchAll(ctx)
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
			var job Job
			if evt.Type == store.EventDelete {
				job = Job{
					TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Job"},
					ObjectMeta: metav1.ObjectMeta{Name: evt.Key},
				}
			} else {
				if err := json.Unmarshal(evt.Value, &job); err != nil {
					continue
				}
				job.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Job"}
			}
			if err := enc.Encode(K8sWatchEvent{Type: string(evt.Type), Object: &job}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ─── Table Format ───────────────────────────────────────────────────────────

func jobListToTable(items []Job) *metav1.Table {
	table := &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Table",
		},
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Namespace", Type: "string"},
			{Name: "Pool", Type: "string"},
			{Name: "Priority", Type: "integer"},
			{Name: "Status", Type: "string"},
			{Name: "Host", Type: "string"},
			{Name: "Duration", Type: "string"},
			{Name: "Exit", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	for i := range items {
		job := &items[i]

		age := "<unknown>"
		if !job.CreationTimestamp.IsZero() {
			age = formatAge(time.Since(job.CreationTimestamp.Time))
		}

		host := job.Status.BMHRef
		if host == "" {
			host = "-"
		}

		duration := "-"
		if job.Status.StartedAt != "" {
			start, err1 := time.Parse(time.RFC3339, job.Status.StartedAt)
			if err1 == nil {
				end := time.Now()
				if job.Status.CompletedAt != "" {
					if e, err2 := time.Parse(time.RFC3339, job.Status.CompletedAt); err2 == nil {
						end = e
					}
				}
				duration = formatAge(end.Sub(start))
			}
		}

		exitCode := "-"
		if job.Status.ExitCode != nil {
			exitCode = fmt.Sprintf("%d", *job.Status.ExitCode)
		}

		raw, _ := json.Marshal(map[string]interface{}{
			"kind":       "PartialObjectMetadata",
			"apiVersion": "meta.k8s.io/v1",
			"metadata": map[string]interface{}{
				"name":              job.Name,
				"namespace":         job.Namespace,
				"creationTimestamp": job.CreationTimestamp.Format(time.RFC3339),
			},
		})

		table.Rows = append(table.Rows, metav1.TableRow{
			Cells: []interface{}{
				job.Name,
				job.Namespace,
				job.Spec.Pool,
				job.Spec.Priority,
				job.Status.Phase,
				host,
				duration,
				exitCode,
				age,
			},
			Object: kruntime.RawExtension{Raw: raw},
		})
	}

	return table
}

// ─── Consistency ────────────────────────────────────────────────────────────

func (p *MicroKubeProvider) checkJobCRDs(ctx context.Context) []CheckItem {
	var items []CheckItem

	if p.deps.Store != nil && p.deps.Store.Jobs != nil {
		storeKeys, err := p.deps.Store.Jobs.Keys(ctx, "")
		if err == nil {
			storeSet := make(map[string]bool, len(storeKeys))
			for _, k := range storeKeys {
				storeSet[k] = true
			}

			for key, job := range p.jobs {
				storeKey := job.Namespace + "." + job.Name
				if storeSet[storeKey] {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("job/%s", key),
						Status:  "pass",
						Message: fmt.Sprintf("Job CRD synced with NATS (phase=%s)", job.Status.Phase),
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("job/%s", key),
						Status:  "fail",
						Message: "Job CRD in memory but not in NATS store",
					})
				}
				delete(storeSet, storeKey)
			}

			for storeKey := range storeSet {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("job/%s", storeKey),
					Status:  "warn",
					Message: "Job CRD in NATS but not in memory",
				})
			}
		}
	}

	return items
}
