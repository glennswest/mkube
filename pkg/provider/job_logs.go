package provider

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

const jobLogMaxLines = 10000

// jobLogBuffer holds in-memory log lines for a single job.
type jobLogBuffer struct {
	mu    sync.Mutex
	lines []string
}

// jobLogStore manages in-memory log buffers for all jobs.
type jobLogStore struct {
	mu      sync.RWMutex
	buffers map[string]*jobLogBuffer // job key → buffer
}

func newJobLogStore() *jobLogStore {
	return &jobLogStore{
		buffers: make(map[string]*jobLogBuffer),
	}
}

func (s *jobLogStore) getOrCreate(key string) *jobLogBuffer {
	s.mu.RLock()
	buf, ok := s.buffers[key]
	s.mu.RUnlock()
	if ok {
		return buf
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check
	if buf, ok = s.buffers[key]; ok {
		return buf
	}
	buf = &jobLogBuffer{}
	s.buffers[key] = buf
	return buf
}

func (s *jobLogStore) get(key string) *jobLogBuffer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buffers[key]
}

func (s *jobLogStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buffers, key)
}

// append adds lines to the ring buffer, evicting oldest if over max.
func (b *jobLogBuffer) append(lines []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, lines...)
	if len(b.lines) > jobLogMaxLines {
		b.lines = b.lines[len(b.lines)-jobLogMaxLines:]
	}
}

// snapshot returns a copy of all lines.
func (b *jobLogBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// ─── Provider methods ───────────────────────────────────────────────────────

// appendJobLogs adds log lines to the in-memory buffer and persists to NATS.
// Also feeds the associated runner's log buffer if the job has a runnerRef.
func (p *MicroKubeProvider) appendJobLogs(ctx context.Context, jobKey string, lines []string) {
	buf := p.jobLogBuf.getOrCreate(jobKey)
	buf.append(lines)

	// Persist to NATS (store all lines as JSON array, overwriting)
	if p.deps.Store != nil && p.deps.Store.JobLogs != nil {
		natsKey := strings.ReplaceAll(jobKey, "/", ".")
		allLines := buf.snapshot()
		data, err := json.Marshal(allLines)
		if err == nil {
			_, _ = p.deps.Store.JobLogs.Put(ctx, natsKey, data)
		}
	}

	// Also feed runner log buffer
	if job, ok := p.jobs[jobKey]; ok && job.Status.RunnerRef != "" {
		p.appendRunnerLogs(job.Status.RunnerRef, lines)
	}
}

// getJobLogs returns log lines, falling back to NATS if not in memory.
func (p *MicroKubeProvider) getJobLogs(ctx context.Context, key string) []string {
	buf := p.jobLogBuf.get(key)
	if buf != nil {
		return buf.snapshot()
	}

	// Fall back to NATS
	if p.deps.Store != nil && p.deps.Store.JobLogs != nil {
		natsKey := strings.ReplaceAll(key, "/", ".")
		data, _, err := p.deps.Store.JobLogs.Get(ctx, natsKey)
		if err == nil {
			var lines []string
			if json.Unmarshal(data, &lines) == nil {
				return lines
			}
		}
	}

	return nil
}

// deleteJobLogs removes the log buffer for a job.
func (p *MicroKubeProvider) deleteJobLogs(key string) {
	p.jobLogBuf.delete(key)
}

// ─── Runner log methods ─────────────────────────────────────────────────────

// appendRunnerLogs adds log lines to the runner's in-memory log buffer.
func (p *MicroKubeProvider) appendRunnerLogs(runnerName string, lines []string) {
	buf := p.runnerLogBuf.getOrCreate(runnerName)
	buf.append(lines)
}

// appendRunnerEvent adds a single timestamped event line to the runner's log.
func (p *MicroKubeProvider) appendRunnerEvent(runnerName, msg string) {
	ts := time.Now().UTC().Format("15:04:05")
	p.appendRunnerLogs(runnerName, []string{"[" + ts + "] " + msg})
}

// getRunnerLogs returns log lines for a runner.
func (p *MicroKubeProvider) getRunnerLogs(runnerName string) []string {
	buf := p.runnerLogBuf.get(runnerName)
	if buf != nil {
		return buf.snapshot()
	}
	return nil
}
