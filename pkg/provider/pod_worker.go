package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/safemap"
)

// podWork is a unit of work for the PodWorker queue.
type podWork struct {
	key    string                           // "namespace/name" for dedup
	reason string                           // human-readable reason for logging
	fn     func(ctx context.Context) error  // the actual work to execute
}

// PodWorker serializes pod lifecycle operations (create, recreate, delete+create)
// in a single goroutine so the reconcile loop stays fast and the RouterOS REST API
// is not overwhelmed by concurrent heavy operations.
type PodWorker struct {
	queue      chan podWork
	pending    *safemap.Map[string, bool]
	processing atomic.Value // current key being processed (string)
	logger     *zap.SugaredLogger

	// createdAt tracks when each pod key was last successfully created by the worker.
	// Used by health checks to skip newly created pods that haven't finished starting.
	createdAt   *safemap.Map[string, time.Time]

	mu          sync.Mutex
	queuedItems []podWork // overflow when channel is full
}

const podWorkerQueueSize = 64

// NewPodWorker creates a PodWorker ready to accept work items.
func NewPodWorker(logger *zap.SugaredLogger) *PodWorker {
	pw := &PodWorker{
		queue:     make(chan podWork, podWorkerQueueSize),
		pending:   safemap.New[string, bool](),
		logger:    logger,
		createdAt: safemap.New[string, time.Time](),
	}
	pw.processing.Store("")
	return pw
}

// Enqueue adds a work item for the given pod key. Non-blocking.
// Duplicate keys (already pending or currently processing) are silently dropped.
// If the queue is full, the item is dropped — reconcile will re-enqueue next cycle.
func (pw *PodWorker) Enqueue(key, reason string, fn func(ctx context.Context) error) {
	// Dedup: skip if already pending or being processed right now
	if pw.IsPendingOrProcessing(key) {
		pw.logger.Debugw("pod-worker: skipping duplicate enqueue", "pod", key, "reason", reason)
		return
	}

	pw.pending.Set(key, true)

	item := podWork{key: key, reason: reason, fn: fn}
	select {
	case pw.queue <- item:
		pw.logger.Infow("pod-worker: enqueued", "pod", key, "reason", reason, "depth", len(pw.queue))
	default:
		// Queue full — drop. Reconcile will re-enqueue next cycle.
		pw.pending.Delete(key)
		pw.logger.Warnw("pod-worker: queue full, dropping", "pod", key, "reason", reason)
	}
}

// IsPendingOrProcessing returns true if the key is queued or currently being worked on.
func (pw *PodWorker) IsPendingOrProcessing(key string) bool {
	if pw.pending.Has(key) {
		return true
	}
	if cur, ok := pw.processing.Load().(string); ok && cur == key {
		return true
	}
	return false
}

// QueueDepth returns the number of items waiting in the queue.
func (pw *PodWorker) QueueDepth() int {
	return pw.pending.Len()
}

// CreatedAt returns when a pod was last successfully created by the worker.
// Returns zero time and false if the pod has no recorded creation.
func (pw *PodWorker) CreatedAt(key string) (time.Time, bool) {
	return pw.createdAt.Get(key)
}

// Run is the main worker loop. Call as a goroutine: go pw.Run(ctx).
// Processes one item at a time until ctx is cancelled.
func (pw *PodWorker) Run(ctx context.Context) {
	pw.logger.Info("pod-worker: started")
	for {
		select {
		case <-ctx.Done():
			pw.logger.Info("pod-worker: shutting down")
			return
		case item := <-pw.queue:
			pw.processItem(ctx, item)
		}
	}
}

func (pw *PodWorker) processItem(ctx context.Context, item podWork) {
	pw.processing.Store(item.key)
	pw.pending.Delete(item.key)

	start := time.Now()
	pw.logger.Infow("pod-worker: processing", "pod", item.key, "reason", item.reason)

	err := item.fn(ctx)
	elapsed := time.Since(start)

	if err != nil {
		pw.logger.Errorw("pod-worker: failed", "pod", item.key, "reason", item.reason,
			"error", err, "elapsed", elapsed)
	} else {
		pw.logger.Infow("pod-worker: completed", "pod", item.key, "reason", item.reason,
			"elapsed", elapsed)
		pw.createdAt.Set(item.key, time.Now())
	}

	pw.processing.Store("")
}
