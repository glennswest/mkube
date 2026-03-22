package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	schedulerInterval   = 10 * time.Second
	provisioningTimeout = 10 * time.Minute
	heartbeatTimeout    = 90 * time.Second
	jobRetentionDefault = 7 * 24 * time.Hour // auto-delete completed jobs after 7 days
	cleanupInterval     = 60                  // run cleanup every 60 scheduler ticks (~10 min)
)

// schedulerDeferred collects NATS writes to perform outside the lock.
// The scheduler does all in-memory mutations under p.mu.Lock(), then
// releases the lock and flushes these writes. This prevents the write
// lock from being held during I/O, which was causing a deadlock:
// scheduler Lock() + slow NATS → blocks all API RLock() → healthz blocked → stormd kills mkube.
type schedulerDeferred struct {
	jobs       []*Job
	hrs        []*HostReservation
	bmhs       []bmhPersist
	jobDeletes []string // keys to delete from NATS (auto-cleanup)
}

type bmhPersist struct {
	key string
	bmh *BareMetalHost
}

func (sd *schedulerDeferred) flush(ctx context.Context, p *MicroKubeProvider) {
	for _, job := range sd.jobs {
		p.persistJob(ctx, job)
	}
	for _, hr := range sd.hrs {
		p.persistHostReservation(ctx, hr)
	}
	for _, bw := range sd.bmhs {
		if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
			parts := strings.SplitN(bw.key, "/", 2)
			if len(parts) == 2 {
				storeKey := parts[0] + "." + parts[1]
				if _, err := p.deps.Store.BareMetalHosts.PutJSON(ctx, storeKey, bw.bmh); err != nil {
					p.deps.Logger.Warnw("scheduler: deferred BMH persist failed", "key", bw.key, "error", err)
				}
			}
		}
	}
	for _, key := range sd.jobDeletes {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 && p.deps.Store != nil {
			storeKey := parts[0] + "." + parts[1]
			if p.deps.Store.Jobs != nil {
				_ = p.deps.Store.Jobs.Delete(ctx, storeKey)
			}
			if p.deps.Store.JobLogs != nil {
				_ = p.deps.Store.JobLogs.Delete(ctx, storeKey)
			}
		}
	}
}

// RunJobScheduler starts the job scheduling loop. It runs alongside the reconciler.
func (p *MicroKubeProvider) RunJobScheduler(ctx context.Context) {
	log := p.deps.Logger.Named("scheduler")
	log.Info("job scheduler starting")

	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("job scheduler stopping")
			return
		case <-ticker.C:
			p.mu.Lock()
			deferred := p.schedulerTick(ctx)
			p.mu.Unlock()
			deferred.flush(ctx, p)
		case <-p.kickScheduler:
			p.mu.Lock()
			deferred := p.schedulerTick(ctx)
			p.mu.Unlock()
			deferred.flush(ctx, p)
		}
	}
}

// schedulerTick performs one scheduling cycle. Must be called with p.mu held.
// Returns deferred NATS writes to flush outside the lock.
func (p *MicroKubeProvider) schedulerTick(ctx context.Context) schedulerDeferred {
	log := p.deps.Logger.Named("scheduler")
	var d schedulerDeferred

	// 1. Schedule pending jobs
	p.schedulePendingJobs(ctx, log, &d)

	// 2. Check provisioning timeouts
	p.checkProvisioningTimeouts(ctx, log, &d)

	// 3. Check running timeouts
	p.checkRunningTimeouts(ctx, log, &d)

	// 4. Check heartbeat timeouts
	p.checkHeartbeatTimeouts(ctx, log, &d)

	// 5. Handle idle runner power-off
	p.checkIdleRunners(ctx, log, &d)

	// 6. Power on hosts during scheduled work hours
	p.ensureScheduledHostsOnline(ctx, log, &d)

	// 7. Auto-cleanup old completed/failed jobs (runs every ~10min via counter)
	p.autoCleanupOldJobs(ctx, log, &d)

	return d
}

// deferBMHUpdate applies an in-memory mutation to a BMH (fast, under lock)
// and queues the NATS write for later flushing outside the lock.
func (p *MicroKubeProvider) deferBMHUpdate(d *schedulerDeferred, key string, mutate func(bmh *BareMetalHost)) error {
	bmh, ok := p.bareMetalHosts[key]
	if !ok {
		return fmt.Errorf("BareMetalHost %s not found", key)
	}
	mutate(bmh)
	d.bmhs = append(d.bmhs, bmhPersist{key: key, bmh: bmh})
	return nil
}

// schedulePendingJobs assigns pending jobs to available hosts.
func (p *MicroKubeProvider) schedulePendingJobs(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	// Collect pending jobs sorted by priority DESC, creation time ASC
	var pending []*Job
	for _, job := range p.jobs {
		if job.Status.Phase == "Pending" {
			pending = append(pending, job)
		}
	}
	if len(pending) == 0 {
		return
	}

	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Spec.Priority != pending[j].Spec.Priority {
			return pending[i].Spec.Priority > pending[j].Spec.Priority
		}
		return pending[i].CreationTimestamp.Before(&pending[j].CreationTimestamp)
	})

	for _, job := range pending {
		// Find matching JobRunner
		var runner *JobRunner
		for _, jr := range p.jobRunners {
			if jr.Spec.Pool == job.Spec.Pool && jr.Status.Phase == "Active" {
				runner = jr
				break
			}
		}
		if runner == nil {
			continue
		}

		// Check maxConcurrent (pool-wide)
		maxConc := runner.Spec.MaxConcurrent
		if maxConc > 0 {
			activeCount := 0
			for _, j := range p.jobs {
				if j.Spec.Pool == runner.Spec.Pool &&
					(j.Status.Phase == "Scheduling" || j.Status.Phase == "Provisioning" || j.Status.Phase == "Running") {
					activeCount++
				}
			}
			if activeCount >= maxConc {
				continue
			}
		}

		// Find available host (allows multiple jobs per host up to maxConcurrent)
		bmhName := p.findAvailableHost(job.Spec.Pool, maxConc, runner.Spec.AllowOverflow)
		if bmhName == "" {
			continue
		}

		// Schedule
		job.Status.Phase = "Scheduling"
		job.Status.RunnerRef = runner.Name
		job.Status.BMHRef = bmhName

		// Mark host reservation as active
		for _, hr := range p.hostReservations {
			if hr.Spec.BMHRef == bmhName {
				hr.Status.ActiveJob = jobKey(job)
				d.hrs = append(d.hrs, hr)
				break
			}
		}

		// Set BMH provisioning config and power on.
		// If the server is already online, skip boot config changes entirely —
		// overwriting template/bootConfigRef/image would trigger a PXE reboot
		// via the BMH operator. The running agent will pick up the new job
		// through its work poll loop.
		for bmhKey, bmh := range p.bareMetalHosts {
			if bmh.Name == bmhName {
				job.Status.HostIP = bmh.Spec.IP
				alreadyOnline := bmh.Spec.Online != nil && *bmh.Spec.Online
				if alreadyOnline {
					log.Infow("host already online, skipping boot config + power-on (agent will pick up job)",
						"bmh", bmhName, "job", jobKey(job))
				} else {
					runnerTemplate := runner.Spec.Template
					runnerBootConfigRef := runner.Spec.BootConfigRef
					runnerImage := runner.Spec.Image
					_ = p.deferBMHUpdate(d, bmhKey, func(b *BareMetalHost) {
						if runnerTemplate != "" {
							b.Spec.Template = runnerTemplate
						} else {
							b.Spec.BootConfigRef = runnerBootConfigRef
						}
						if runnerImage != "" {
							b.Spec.Image = runnerImage
						}
						online := true
						b.Spec.Online = &online
						// Clear manual-power annotation — scheduler takes power control
						delete(b.Annotations, "bmh.mkube.io/manual-power")
					})
				}
				break
			}
		}

		// Transition to Provisioning
		job.Status.Phase = "Provisioning"
		d.jobs = append(d.jobs, job)

		log.Infow("job scheduled",
			"job", jobKey(job),
			"bmh", bmhName,
			"pool", job.Spec.Pool,
			"priority", job.Spec.Priority,
		)
		p.appendRunnerEvent(runner.Name, fmt.Sprintf("Scheduled %s → %s (priority %d)", jobKey(job), bmhName, job.Spec.Priority))
	}
}

// findAvailableHost finds a BMH for the pool that is under the per-host concurrent limit.
func (p *MicroKubeProvider) findAvailableHost(pool string, maxConcurrent int, allowOverflow bool) string {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	// Count active jobs per BMH
	bmhActive := make(map[string]int)
	for _, j := range p.jobs {
		if j.Spec.Pool == pool &&
			(j.Status.Phase == "Scheduling" || j.Status.Phase == "Provisioning" || j.Status.Phase == "Running") {
			bmhActive[j.Status.BMHRef]++
		}
	}

	// First: reserved hosts for this pool under the limit
	for _, hr := range p.hostReservations {
		if hr.Spec.Pool == pool && hr.Status.Phase == "Active" {
			if bmhActive[hr.Spec.BMHRef] < maxConcurrent {
				return hr.Spec.BMHRef
			}
		}
	}

	// Overflow: unreserved BMHs under the limit
	if allowOverflow {
		reserved := make(map[string]bool)
		for _, hr := range p.hostReservations {
			reserved[hr.Spec.BMHRef] = true
		}
		for _, bmh := range p.bareMetalHosts {
			if !reserved[bmh.Name] && bmhActive[bmh.Name] < maxConcurrent {
				return bmh.Name
			}
		}
	}

	return ""
}

// releaseJobHostDeferred clears the host reservation for a completed/failed job.
// Only clears ActiveJob if no other active jobs remain on the host.
func (p *MicroKubeProvider) releaseJobHostDeferred(job *Job, d *schedulerDeferred) {
	if job.Status.BMHRef == "" {
		return
	}
	// Count remaining active jobs on this BMH (excluding the one being released)
	remaining := 0
	for _, j := range p.jobs {
		if j.Spec.Pool == job.Spec.Pool && j.Status.BMHRef == job.Status.BMHRef &&
			(j.Status.Phase == "Provisioning" || j.Status.Phase == "Running") &&
			jobKey(j) != jobKey(job) {
			remaining++
		}
	}
	for _, hr := range p.hostReservations {
		if hr.Spec.BMHRef == job.Status.BMHRef && hr.Spec.Pool == job.Spec.Pool {
			if remaining == 0 {
				hr.Status.ActiveJob = ""
			}
			d.hrs = append(d.hrs, hr)
			break
		}
	}
}

// checkProvisioningTimeouts fails jobs stuck in Provisioning too long.
func (p *MicroKubeProvider) checkProvisioningTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	for _, job := range p.jobs {
		if job.Status.Phase != "Provisioning" {
			continue
		}
		// Use creation time + scheduling as proxy if startedAt not set
		created := job.CreationTimestamp.Time
		if time.Since(created) > provisioningTimeout {
			job.Status.Phase = "Failed"
			job.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			job.Status.ErrorMessage = "provisioning timeout exceeded"
			p.releaseJobHostDeferred(job, d)
			d.jobs = append(d.jobs, job)
			log.Infow("job provisioning timeout", "job", jobKey(job))
			if job.Status.RunnerRef != "" {
				p.appendRunnerEvent(job.Status.RunnerRef, fmt.Sprintf("TIMEOUT provisioning %s on %s", jobKey(job), job.Status.BMHRef))
			}
		}
	}
}

// checkRunningTimeouts fails jobs exceeding their spec.timeout.
func (p *MicroKubeProvider) checkRunningTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	for _, job := range p.jobs {
		if job.Status.Phase != "Running" || job.Spec.Timeout <= 0 {
			continue
		}
		if job.Status.StartedAt == "" {
			continue
		}
		started, err := time.Parse(time.RFC3339, job.Status.StartedAt)
		if err != nil {
			continue
		}
		if time.Since(started) > time.Duration(job.Spec.Timeout)*time.Second {
			job.Status.Phase = "TimedOut"
			job.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			job.Status.ErrorMessage = fmt.Sprintf("exceeded timeout of %ds", job.Spec.Timeout)
			p.releaseJobHostDeferred(job, d)
			d.jobs = append(d.jobs, job)
			log.Infow("job timed out", "job", jobKey(job), "timeout", job.Spec.Timeout)
			if job.Status.RunnerRef != "" {
				p.appendRunnerEvent(job.Status.RunnerRef, fmt.Sprintf("TIMEOUT running %s after %ds", jobKey(job), job.Spec.Timeout))
			}
		}
	}
}

// checkHeartbeatTimeouts fails jobs with stale heartbeats.
func (p *MicroKubeProvider) checkHeartbeatTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	for _, job := range p.jobs {
		if job.Status.Phase != "Running" || job.Status.LastHeartbeat == "" {
			continue
		}
		lastHB, err := time.Parse(time.RFC3339, job.Status.LastHeartbeat)
		if err != nil {
			continue
		}
		if time.Since(lastHB) > heartbeatTimeout {
			job.Status.Phase = "Failed"
			job.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			job.Status.ErrorMessage = "heartbeat timeout — agent may have crashed"
			p.releaseJobHostDeferred(job, d)
			d.jobs = append(d.jobs, job)
			log.Infow("job heartbeat timeout", "job", jobKey(job))
			if job.Status.RunnerRef != "" {
				p.appendRunnerEvent(job.Status.RunnerRef, fmt.Sprintf("HEARTBEAT TIMEOUT %s on %s — agent may have crashed", jobKey(job), job.Status.BMHRef))
			}
		}
	}
}

// checkIdleRunners powers off idle hosts after the idle timeout expires.
// Hosts in pools with an active schedule are not powered off during scheduled hours.
func (p *MicroKubeProvider) checkIdleRunners(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	now := time.Now()
	for _, runner := range p.jobRunners {
		if runner.Spec.IdleTimeout <= 0 || runner.Spec.ReclaimPolicy == "Retain" {
			continue
		}

		// Skip power-off during active schedule hours
		if runner.Spec.Schedule != nil && runner.Spec.Schedule.IsActive(now) {
			continue
		}

		pool := runner.Spec.Pool

		// Check if any pending or active jobs for this pool
		hasWork := false
		var lastCompletion time.Time
		for _, job := range p.jobs {
			if job.Spec.Pool != pool {
				continue
			}
			switch job.Status.Phase {
			case "Pending", "Scheduling", "Provisioning", "Running":
				hasWork = true
			case "Completed", "Failed", "TimedOut", "Cancelled":
				if job.Status.CompletedAt != "" {
					if t, err := time.Parse(time.RFC3339, job.Status.CompletedAt); err == nil {
						if t.After(lastCompletion) {
							lastCompletion = t
						}
					}
				}
			}
		}

		if hasWork || lastCompletion.IsZero() {
			continue
		}

		if time.Since(lastCompletion) < time.Duration(runner.Spec.IdleTimeout)*time.Second {
			continue
		}

		// Power off all reserved hosts in this pool
		for _, hr := range p.hostReservations {
			if hr.Spec.Pool != pool || hr.Status.ActiveJob != "" {
				continue
			}
			for bmhKey, bmh := range p.bareMetalHosts {
				if bmh.Name == hr.Spec.BMHRef && bmh.Spec.Online != nil && *bmh.Spec.Online {
					// Skip hosts manually powered on by user
					if bmh.Annotations != nil && bmh.Annotations["bmh.mkube.io/manual-power"] != "" {
						continue
					}
					_ = p.deferBMHUpdate(d, bmhKey, func(b *BareMetalHost) {
						offline := false
						b.Spec.Online = &offline
					})
					log.Infow("powering off idle host",
						"bmh", bmh.Name,
						"pool", pool,
						"idleTimeout", runner.Spec.IdleTimeout,
					)
					p.appendRunnerEvent(runner.Name, fmt.Sprintf("Powering off idle host %s (timeout %ds)", bmh.Name, runner.Spec.IdleTimeout))
				}
			}
		}
	}
}

// ensureScheduledHostsOnline powers on reserved hosts during active schedule hours.
func (p *MicroKubeProvider) ensureScheduledHostsOnline(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	now := time.Now()
	for _, runner := range p.jobRunners {
		if runner.Spec.Schedule == nil || !runner.Spec.Schedule.IsActive(now) {
			continue
		}

		pool := runner.Spec.Pool

		for _, hr := range p.hostReservations {
			if hr.Spec.Pool != pool || hr.Status.ActiveJob != "" {
				continue
			}
			for bmhKey, bmh := range p.bareMetalHosts {
				if bmh.Name != hr.Spec.BMHRef {
					continue
				}
				if bmh.Spec.Online == nil || !*bmh.Spec.Online {
					_ = p.deferBMHUpdate(d, bmhKey, func(b *BareMetalHost) {
						online := true
						b.Spec.Online = &online
					})
					log.Infow("powering on host for scheduled work hours",
						"bmh", bmh.Name,
						"pool", pool,
						"schedule", runner.Spec.Schedule.FormatSummary(),
					)
					p.appendRunnerEvent(runner.Name, fmt.Sprintf("Powering on %s for scheduled work hours", bmh.Name))
				}
			}
		}
	}
}

// autoCleanupOldJobs removes completed/failed/timed-out/cancelled jobs older
// than the retention period. Runs on a tick counter to avoid doing NATS I/O
// every 10 seconds.
func (p *MicroKubeProvider) autoCleanupOldJobs(ctx context.Context, log interface{ Infow(string, ...interface{}) }, d *schedulerDeferred) {
	p.cleanupTickCounter++
	if p.cleanupTickCounter < cleanupInterval {
		return
	}
	p.cleanupTickCounter = 0

	cutoff := time.Now().Add(-jobRetentionDefault)
	var toDelete []string

	for key, job := range p.jobs {
		switch job.Status.Phase {
		case "Completed", "Failed", "TimedOut", "Cancelled":
		default:
			continue
		}

		if job.Status.CompletedAt != "" {
			if t, err := time.Parse(time.RFC3339, job.Status.CompletedAt); err == nil {
				if t.After(cutoff) {
					continue
				}
			}
		} else if !job.CreationTimestamp.IsZero() && job.CreationTimestamp.Time.After(cutoff) {
			continue
		}

		toDelete = append(toDelete, key)
	}

	if len(toDelete) == 0 {
		return
	}

	for _, key := range toDelete {
		p.deleteJobLogs(key)
		delete(p.jobs, key)
	}

	// Queue NATS deletes for deferred flush (outside lock)
	d.jobDeletes = append(d.jobDeletes, toDelete...)

	log.Infow("auto-cleaned old jobs",
		"count", len(toDelete),
		"retention", jobRetentionDefault.String(),
	)
}
