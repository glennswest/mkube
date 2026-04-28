package provider

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/safemap"
)

// PodWorker dispatches pod lifecycle operations as concurrent goroutines and
// remembers when each pod was last created so health checks can grant a
// startup grace period. The earlier serialized-queue implementation existed
// to protect the RouterOS REST API from concurrent session pile-up — with
// the native API in use, tag-multiplexing handles concurrent commands
// natively, so each lifecycle operation runs in its own goroutine.
//
// The Enqueue / IsPendingOrProcessing / CreatedAt method signatures are kept
// for compatibility with existing call sites; "Enqueue" no longer queues —
// it runs the function immediately in a goroutine, with a per-key in-flight
// guard to drop duplicates.
type PodWorker struct {
	inflight  *safemap.Map[string, bool]
	createdAt *safemap.Map[string, time.Time]
	logger    *zap.SugaredLogger
	ctx       context.Context
}

// NewPodWorker creates a PodWorker. Run must be called once with the
// long-lived application context before Enqueue is invoked, to wire up the
// context that goroutines will see.
func NewPodWorker(logger *zap.SugaredLogger) *PodWorker {
	return &PodWorker{
		inflight:  safemap.New[string, bool](),
		createdAt: safemap.New[string, time.Time](),
		logger:    logger,
		ctx:       context.Background(),
	}
}

// Run wires up the long-lived context and blocks until it is cancelled.
// Existing callers do `go pw.Run(ctx)`; that pattern stays unchanged so the
// goroutine sits idle until shutdown without any other side effects.
func (pw *PodWorker) Run(ctx context.Context) {
	pw.ctx = ctx
	pw.logger.Info("pod-worker: started (concurrent dispatch)")
	<-ctx.Done()
	pw.logger.Info("pod-worker: shutting down")
}

// Enqueue runs fn in its own goroutine if no other operation for the same
// key is already in flight. Duplicate enqueues for an in-flight key are
// dropped silently — reconcile will re-trigger on the next cycle if the work
// is still needed.
func (pw *PodWorker) Enqueue(key, reason string, fn func(ctx context.Context) error) {
	if !pw.inflight.SetIfAbsent(key, true) {
		pw.logger.Debugw("pod-worker: skipping duplicate", "pod", key, "reason", reason)
		return
	}

	go func() {
		defer pw.inflight.Delete(key)
		start := time.Now()
		pw.logger.Infow("pod-worker: starting", "pod", key, "reason", reason)
		err := fn(pw.ctx)
		elapsed := time.Since(start)
		if err != nil {
			pw.logger.Errorw("pod-worker: failed", "pod", key, "reason", reason,
				"error", err, "elapsed", elapsed)
			return
		}
		pw.logger.Infow("pod-worker: completed", "pod", key, "reason", reason,
			"elapsed", elapsed)
		pw.createdAt.Set(key, time.Now())
	}()
}

// IsPendingOrProcessing returns true if a goroutine for key is currently running.
func (pw *PodWorker) IsPendingOrProcessing(key string) bool {
	return pw.inflight.Has(key)
}

// QueueDepth reports the number of in-flight operations.
func (pw *PodWorker) QueueDepth() int {
	return pw.inflight.Len()
}

// CreatedAt returns when fn last completed successfully for key.
func (pw *PodWorker) CreatedAt(key string) (time.Time, bool) {
	return pw.createdAt.Get(key)
}
