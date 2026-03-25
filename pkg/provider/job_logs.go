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
	mu          sync.RWMutex
	buffers     map[string]*jobLogBuffer // job key → buffer
	subscribers map[string][]chan string  // job key → fan-out channels
}

func newJobLogStore() *jobLogStore {
	return &jobLogStore{
		buffers:     make(map[string]*jobLogBuffer),
		subscribers: make(map[string][]chan string),
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

// subscribe atomically snapshots the current buffer and registers a channel
// for new lines. The channel is buffered (1000) so the append path never blocks.
func (s *jobLogStore) subscribe(key string) ([]string, chan string) {
	ch := make(chan string, 1000)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Snapshot existing lines under the store lock so no lines are lost
	// between snapshot and subscribe.
	var backlog []string
	if buf, ok := s.buffers[key]; ok {
		buf.mu.Lock()
		backlog = make([]string, len(buf.lines))
		copy(backlog, buf.lines)
		buf.mu.Unlock()
	}

	s.subscribers[key] = append(s.subscribers[key], ch)
	return backlog, ch
}

// unsubscribe removes a channel from the subscriber list.
func (s *jobLogStore) unsubscribe(key string, ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subscribers[key]
	for i, c := range subs {
		if c == ch {
			s.subscribers[key] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(s.subscribers[key]) == 0 {
		delete(s.subscribers, key)
	}
}

// notifySubscribers sends lines to all subscriber channels for a job key.
// Non-blocking: drops lines if a subscriber channel is full.
func (s *jobLogStore) notifySubscribers(key string, lines []string) {
	s.mu.RLock()
	subs := s.subscribers[key]
	s.mu.RUnlock()

	for _, ch := range subs {
		for _, line := range lines {
			select {
			case ch <- line:
			default:
			}
		}
	}
}

// deleteSubscribers closes all subscriber channels for a job key and removes them.
func (s *jobLogStore) deleteSubscribers(key string) {
	s.mu.Lock()
	subs := s.subscribers[key]
	delete(s.subscribers, key)
	s.mu.Unlock()

	for _, ch := range subs {
		close(ch)
	}
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

	// Fan out to any follow subscribers
	p.jobLogBuf.notifySubscribers(jobKey, lines)

	// Persist to NATS (store all lines as JSON array, overwriting)
	if p.deps.Store != nil && p.deps.Store.JobLogs != nil {
		natsKey := strings.ReplaceAll(jobKey, "/", ".")
		allLines := buf.snapshot()
		data, err := json.Marshal(allLines)
		if err == nil {
			_, _ = p.deps.Store.JobLogs.Put(ctx, natsKey, data)
		}
	}

	// Note: build output is NOT fed to the runner log — runner log only
	// gets scheduler events (Scheduled, Started, FAILED, COMPLETED) via
	// appendRunnerEvent. Raw build output stays in per-job logs only.
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

// deleteJobLogs removes the log buffer for a job and closes any follow subscribers.
func (p *MicroKubeProvider) deleteJobLogs(key string) {
	p.jobLogBuf.deleteSubscribers(key)
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
