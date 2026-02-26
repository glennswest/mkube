package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
)

// Store provides persistent key-value storage backed by NATS JetStream.
type Store struct {
	conn     *nats.Conn
	js       jetstream.JetStream
	log      *zap.SugaredLogger
	replicas int

	Pods           *Bucket
	ConfigMaps     *Bucket
	Namespaces     *Bucket
	NodeStatus     *Bucket
	BareMetalHosts        *Bucket
	Deployments           *Bucket
	PersistentVolumeClaims *Bucket
	Networks               *Bucket
	Registries             *Bucket
}

// Bucket wraps a NATS JetStream KeyValue store with typed operations.
type Bucket struct {
	kv  jetstream.KeyValue
	log *zap.SugaredLogger
}

// New connects to NATS and initializes JetStream KV buckets.
func New(ctx context.Context, cfg config.NATSConfig, log *zap.SugaredLogger) (*Store, error) {
	url := cfg.URL
	if url == "" {
		url = nats.DefaultURL
	}

	s := &Store{log: log.Named("store"), replicas: cfg.Replicas}
	if s.replicas < 1 {
		s.replicas = 1
	}

	nc, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectBufSize(8*1024*1024),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warnw("NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			log.Infow("NATS reconnected, reinitializing KV buckets")
			if reinitErr := s.reinitBuckets(); reinitErr != nil {
				log.Errorw("failed to reinitialize KV buckets after reconnect", "error", reinitErr)
			} else {
				log.Infow("KV buckets reinitialized after reconnect")
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", url, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	s.conn = nc
	s.js = js

	if err := s.initAllBuckets(ctx); err != nil {
		nc.Close()
		return nil, err
	}

	s.log.Infow("store initialized", "url", url, "replicas", s.replicas)
	return s, nil
}

// NewFromConn creates a Store from an existing NATS connection (for testing).
func NewFromConn(ctx context.Context, nc *nats.Conn, replicas int, log *zap.SugaredLogger) (*Store, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	s := &Store{conn: nc, js: js, log: log.Named("store"), replicas: replicas}

	if err := s.initAllBuckets(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

// initAllBuckets creates all KV buckets. Used at startup and after reconnect.
func (s *Store) initAllBuckets(ctx context.Context) error {
	var err error
	s.Pods, err = s.initBucket(ctx, "PODS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.ConfigMaps, err = s.initBucket(ctx, "CONFIGMAPS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.Namespaces, err = s.initBucket(ctx, "NAMESPACES", s.replicas, 0)
	if err != nil {
		return err
	}
	s.NodeStatus, err = s.initBucket(ctx, "NODE_STATUS", s.replicas, 60*time.Second)
	if err != nil {
		return err
	}
	s.BareMetalHosts, err = s.initBucket(ctx, "BAREMETALHOSTS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.Deployments, err = s.initBucket(ctx, "DEPLOYMENTS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.PersistentVolumeClaims, err = s.initBucket(ctx, "PVCS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.Networks, err = s.initBucket(ctx, "NETWORKS", s.replicas, 0)
	if err != nil {
		return err
	}
	s.Registries, err = s.initBucket(ctx, "REGISTRIES", s.replicas, 0)
	if err != nil {
		return err
	}
	return nil
}

// reinitBuckets re-creates KV bucket handles after a NATS reconnection.
// The NATS server may have restarted with empty JetStream, so the old
// bucket handles would point to non-existent streams.
func (s *Store) reinitBuckets() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Re-create the JetStream context for the reconnected connection
	js, err := jetstream.New(s.conn)
	if err != nil {
		return fmt.Errorf("creating JetStream context: %w", err)
	}
	s.js = js

	return s.initAllBuckets(ctx)
}

func (s *Store) initBucket(ctx context.Context, name string, replicas int, ttl time.Duration) (*Bucket, error) {
	kvCfg := jetstream.KeyValueConfig{
		Bucket:   name,
		Replicas: replicas,
	}
	if ttl > 0 {
		kvCfg.TTL = ttl
	}

	kv, err := s.js.CreateOrUpdateKeyValue(ctx, kvCfg)
	if err != nil {
		return nil, fmt.Errorf("creating KV bucket %s: %w", name, err)
	}

	return &Bucket{kv: kv, log: s.log.Named(strings.ToLower(name))}, nil
}

// Subscribe creates a NATS core subscription on the given subject.
func (s *Store) Subscribe(subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	return s.conn.Subscribe(subject, handler)
}

// Close closes the NATS connection.
func (s *Store) Close() {
	if s.conn != nil {
		s.conn.Close()
	}
}

// Connected returns true if the NATS connection is active.
func (s *Store) Connected() bool {
	return s.conn != nil && s.conn.IsConnected()
}

// ─── Bucket Operations ──────────────────────────────────────────────────────

// Get retrieves a value by key. Returns the value, revision, and error.
func (b *Bucket) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	entry, err := b.kv.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return entry.Value(), entry.Revision(), nil
}

// Put stores a value at key. Returns the new revision.
func (b *Bucket) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	rev, err := b.kv.Put(ctx, key, value)
	if err != nil {
		return 0, err
	}
	return rev, nil
}

// Delete removes a key.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	return b.kv.Delete(ctx, key)
}

// Keys returns all keys in the bucket, optionally filtered by prefix.
func (b *Bucket) Keys(ctx context.Context, prefix string) ([]string, error) {
	keys, err := b.kv.Keys(ctx)
	if err != nil {
		// If no keys exist, NATS returns an error; treat as empty
		if err == jetstream.ErrNoKeysFound {
			return nil, nil
		}
		return nil, err
	}
	if prefix == "" {
		return keys, nil
	}
	filtered := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			filtered = append(filtered, k)
		}
	}
	return filtered, nil
}

// IsEmpty returns true if the bucket has no keys.
func (b *Bucket) IsEmpty(ctx context.Context) (bool, error) {
	keys, err := b.Keys(ctx, "")
	if err != nil {
		return false, err
	}
	return len(keys) == 0, nil
}

// PutJSON marshals v to JSON and stores it at key.
func (b *Bucket) PutJSON(ctx context.Context, key string, v interface{}) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("marshaling JSON: %w", err)
	}
	return b.Put(ctx, key, data)
}

// GetJSON retrieves a value and unmarshals it from JSON.
func (b *Bucket) GetJSON(ctx context.Context, key string, v interface{}) (uint64, error) {
	data, rev, err := b.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return 0, fmt.Errorf("unmarshaling JSON for key %s: %w", key, err)
	}
	return rev, nil
}

// ListJSON retrieves all values matching the prefix and unmarshals them.
func (b *Bucket) ListJSON(ctx context.Context, prefix string, factory func() interface{}) ([]interface{}, error) {
	keys, err := b.Keys(ctx, prefix)
	if err != nil {
		return nil, err
	}
	results := make([]interface{}, 0, len(keys))
	for _, key := range keys {
		v := factory()
		if _, err := b.GetJSON(ctx, key, v); err != nil {
			b.log.Warnw("skipping corrupt entry", "key", key, "error", err)
			continue
		}
		results = append(results, v)
	}
	return results, nil
}
