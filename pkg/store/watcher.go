package store

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// EventType represents the type of a watch event.
type EventType string

const (
	EventPut    EventType = "ADDED"
	EventUpdate EventType = "MODIFIED"
	EventDelete EventType = "DELETED"
)

// WatchEvent represents a single state change in a KV bucket.
type WatchEvent struct {
	Type     EventType
	Key      string
	Value    []byte
	Revision uint64
}

// Watch returns a channel of WatchEvents for keys matching the given filter.
// If filter is empty, all keys are watched. Pass sinceRevision > 0 to resume.
// The returned channel is closed when the context is cancelled.
func (b *Bucket) Watch(ctx context.Context, filter string, sinceRevision uint64) (<-chan WatchEvent, error) {
	opts := []jetstream.WatchOpt{
		jetstream.IgnoreDeletes(),
	}

	var watchKey string
	if filter == "" {
		watchKey = ">"
	} else {
		watchKey = filter + ".>"
	}

	// If sinceRevision is 0, include all existing values (initial state)
	if sinceRevision == 0 {
		opts = []jetstream.WatchOpt{}
	}

	watcher, err := b.kv.Watch(ctx, watchKey, opts...)
	if err != nil {
		return nil, fmt.Errorf("starting watch on %s: %w", watchKey, err)
	}

	ch := make(chan WatchEvent, 64)

	go func() {
		defer close(ch)
		defer func() { _ = watcher.Stop() }()

		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-watcher.Updates():
				if !ok {
					return
				}
				if entry == nil {
					// nil entry signals end of initial values
					continue
				}

				evt := WatchEvent{
					Key:      entry.Key(),
					Value:    entry.Value(),
					Revision: entry.Revision(),
				}

				switch entry.Operation() {
				case jetstream.KeyValuePut:
					if entry.Revision() == entry.Revision() {
						// We can't easily distinguish create vs update from NATS KV,
						// but for K8s compat we use revision == 1 as a heuristic.
						if entry.Revision() == 1 {
							evt.Type = EventPut
						} else {
							evt.Type = EventUpdate
						}
					}
				case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
					evt.Type = EventDelete
				}

				select {
				case ch <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// WatchAll returns a channel of all events (including deletes) for all keys.
func (b *Bucket) WatchAll(ctx context.Context) (<-chan WatchEvent, error) {
	watcher, err := b.kv.WatchAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting watch-all: %w", err)
	}

	ch := make(chan WatchEvent, 64)

	go func() {
		defer close(ch)
		defer func() { _ = watcher.Stop() }()

		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-watcher.Updates():
				if !ok {
					return
				}
				if entry == nil {
					continue
				}

				evt := WatchEvent{
					Key:      entry.Key(),
					Value:    entry.Value(),
					Revision: entry.Revision(),
				}

				switch entry.Operation() {
				case jetstream.KeyValuePut:
					if entry.Revision() == 1 {
						evt.Type = EventPut
					} else {
						evt.Type = EventUpdate
					}
				case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
					evt.Type = EventDelete
				}

				select {
				case ch <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}
