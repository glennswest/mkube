package provider

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const (
	schedulerInterval      = 10 * time.Second
	provisioningTimeout    = 10 * time.Minute
	heartbeatTimeout       = 90 * time.Second
)

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
			p.schedulerTick(ctx)
			p.mu.Unlock()
		case <-p.kickScheduler:
			p.mu.Lock()
			p.schedulerTick(ctx)
			p.mu.Unlock()
		}
	}
}

// schedulerTick performs one scheduling cycle. Must be called with p.mu held.
func (p *MicroKubeProvider) schedulerTick(ctx context.Context) {
	log := p.deps.Logger.Named("scheduler")

	// 1. Schedule pending jobs
	p.schedulePendingJobs(ctx, log)

	// 2. Check provisioning timeouts
	p.checkProvisioningTimeouts(ctx, log)

	// 3. Check running timeouts
	p.checkRunningTimeouts(ctx, log)

	// 4. Check heartbeat timeouts
	p.checkHeartbeatTimeouts(ctx, log)

	// 5. Handle idle runner power-off
	p.checkIdleRunners(ctx, log)

	// 6. Power on hosts during scheduled work hours
	p.ensureScheduledHostsOnline(ctx, log)
}

// schedulePendingJobs assigns pending jobs to available hosts.
func (p *MicroKubeProvider) schedulePendingJobs(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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

		// Check maxConcurrent
		if runner.Spec.MaxConcurrent > 0 {
			activeCount := 0
			for _, j := range p.jobs {
				if j.Spec.Pool == runner.Spec.Pool &&
					(j.Status.Phase == "Scheduling" || j.Status.Phase == "Provisioning" || j.Status.Phase == "Running") {
					activeCount++
				}
			}
			if activeCount >= runner.Spec.MaxConcurrent {
				continue
			}
		}

		// Find available host
		bmhName := p.findAvailableHost(job.Spec.Pool, runner.Spec.AllowOverflow)
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
				p.persistHostReservation(ctx, hr)
				break
			}
		}

		// Set BMH provisioning config and power on
		for bmhKey, bmh := range p.bareMetalHosts {
			if bmh.Name == bmhName {
				job.Status.HostIP = bmh.Spec.IP
				runnerTemplate := runner.Spec.Template
				runnerBootConfigRef := runner.Spec.BootConfigRef
				runnerImage := runner.Spec.Image
				_ = p.updateBMHFields(ctx, bmhKey, func(b *BareMetalHost) {
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
				})
				break
			}
		}

		// Transition to Provisioning
		job.Status.Phase = "Provisioning"
		p.persistJob(ctx, job)

		log.Infow("job scheduled",
			"job", jobKey(job),
			"bmh", bmhName,
			"pool", job.Spec.Pool,
			"priority", job.Spec.Priority,
		)
	}
}

// findAvailableHost finds a free BMH for the pool.
func (p *MicroKubeProvider) findAvailableHost(pool string, allowOverflow bool) string {
	// First: reserved hosts for this pool with no active job
	for _, hr := range p.hostReservations {
		if hr.Spec.Pool == pool && hr.Status.ActiveJob == "" && hr.Status.Phase == "Active" {
			return hr.Spec.BMHRef
		}
	}

	// Overflow: unreserved BMHs
	if allowOverflow {
		reserved := make(map[string]bool)
		for _, hr := range p.hostReservations {
			reserved[hr.Spec.BMHRef] = true
		}
		for _, bmh := range p.bareMetalHosts {
			if !reserved[bmh.Name] {
				return bmh.Name
			}
		}
	}

	return ""
}

// checkProvisioningTimeouts fails jobs stuck in Provisioning too long.
func (p *MicroKubeProvider) checkProvisioningTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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
			p.releaseJobHost(ctx, job)
			p.persistJob(ctx, job)
			log.Infow("job provisioning timeout", "job", jobKey(job))
		}
	}
}

// checkRunningTimeouts fails jobs exceeding their spec.timeout.
func (p *MicroKubeProvider) checkRunningTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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
			p.releaseJobHost(ctx, job)
			p.persistJob(ctx, job)
			log.Infow("job timed out", "job", jobKey(job), "timeout", job.Spec.Timeout)
		}
	}
}

// checkHeartbeatTimeouts fails jobs with stale heartbeats.
func (p *MicroKubeProvider) checkHeartbeatTimeouts(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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
			p.releaseJobHost(ctx, job)
			p.persistJob(ctx, job)
			log.Infow("job heartbeat timeout", "job", jobKey(job))
		}
	}
}

// checkIdleRunners powers off idle hosts after the idle timeout expires.
// Hosts in pools with an active schedule are not powered off during scheduled hours.
func (p *MicroKubeProvider) checkIdleRunners(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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
					_ = p.updateBMHFields(ctx, bmhKey, func(b *BareMetalHost) {
						offline := false
						b.Spec.Online = &offline
					})
					log.Infow("powering off idle host",
						"bmh", bmh.Name,
						"pool", pool,
						"idleTimeout", runner.Spec.IdleTimeout,
					)
				}
			}
		}
	}
}

// ensureScheduledHostsOnline powers on reserved hosts during active schedule hours.
func (p *MicroKubeProvider) ensureScheduledHostsOnline(ctx context.Context, log interface{ Infow(string, ...interface{}) }) {
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
					_ = p.updateBMHFields(ctx, bmhKey, func(b *BareMetalHost) {
						online := true
						b.Spec.Online = &online
					})
					log.Infow("powering on host for scheduled work hours",
						"bmh", bmh.Name,
						"pool", pool,
						"schedule", runner.Spec.Schedule.FormatSummary(),
					)
				}
			}
		}
	}
}
