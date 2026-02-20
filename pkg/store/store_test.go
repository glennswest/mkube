package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

func startTestServer(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random port
		NoLog:     true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}

	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}

	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		ns.Shutdown()
		t.Fatal(err)
	}

	return ns, nc
}

func testStore(t *testing.T) (*Store, func()) {
	t.Helper()
	ns, nc := startTestServer(t)

	log, _ := zap.NewDevelopment()
	s, err := NewFromConn(context.Background(), nc, 1, log.Sugar())
	if err != nil {
		nc.Close()
		ns.Shutdown()
		t.Fatal(err)
	}

	return s, func() {
		s.Close()
		ns.Shutdown()
	}
}

func TestBucketCRUD(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	// Put
	rev, err := s.Pods.Put(ctx, "default.nginx", []byte(`{"name":"nginx"}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rev == 0 {
		t.Fatal("expected non-zero revision")
	}

	// Get
	val, gotRev, err := s.Pods.Get(ctx, "default.nginx")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(val) != `{"name":"nginx"}` {
		t.Fatalf("unexpected value: %s", val)
	}
	if gotRev != rev {
		t.Fatalf("revision mismatch: got %d, want %d", gotRev, rev)
	}

	// Keys
	keys, err := s.Pods.Keys(ctx, "")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "default.nginx" {
		t.Fatalf("unexpected keys: %v", keys)
	}

	// Keys with prefix
	_, _ = s.Pods.Put(ctx, "kube.redis", []byte(`{}`))
	keys, err = s.Pods.Keys(ctx, "default.")
	if err != nil {
		t.Fatalf("Keys prefix: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key with prefix 'default.', got %d", len(keys))
	}

	// Delete
	if err := s.Pods.Delete(ctx, "default.nginx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err = s.Pods.Get(ctx, "default.nginx")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestBucketJSON(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	type TestPod struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Image     string `json:"image"`
	}

	pod := TestPod{Name: "nginx", Namespace: "default", Image: "nginx:latest"}
	rev, err := s.Pods.PutJSON(ctx, "default.nginx", &pod)
	if err != nil {
		t.Fatalf("PutJSON: %v", err)
	}
	if rev == 0 {
		t.Fatal("expected non-zero revision")
	}

	var got TestPod
	gotRev, err := s.Pods.GetJSON(ctx, "default.nginx", &got)
	if err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if got.Name != "nginx" || got.Image != "nginx:latest" {
		t.Fatalf("unexpected pod: %+v", got)
	}
	if gotRev != rev {
		t.Fatalf("revision mismatch: got %d, want %d", gotRev, rev)
	}
}

func TestBucketIsEmpty(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	empty, err := s.Pods.IsEmpty(ctx)
	if err != nil {
		t.Fatalf("IsEmpty: %v", err)
	}
	if !empty {
		t.Fatal("expected empty bucket")
	}

	_, _ = s.Pods.Put(ctx, "default.nginx", []byte("{}"))

	empty, err = s.Pods.IsEmpty(ctx)
	if err != nil {
		t.Fatalf("IsEmpty after put: %v", err)
	}
	if empty {
		t.Fatal("expected non-empty bucket")
	}
}

func TestWatchAll(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := s.Pods.WatchAll(ctx)
	if err != nil {
		t.Fatalf("WatchAll: %v", err)
	}

	// Give the watcher time to start
	time.Sleep(200 * time.Millisecond)

	// Put a key
	_, err = s.Pods.Put(ctx, "default.nginx", []byte(`{"name":"nginx"}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Receive event
	select {
	case evt := <-ch:
		if evt.Key != "default.nginx" {
			t.Fatalf("unexpected key: %s", evt.Key)
		}
		if evt.Type != EventPut {
			t.Fatalf("expected ADDED, got %s", evt.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for watch event")
	}

	// Update same key
	_, err = s.Pods.Put(ctx, "default.nginx", []byte(`{"name":"nginx","v":2}`))
	if err != nil {
		t.Fatalf("Put update: %v", err)
	}

	select {
	case evt := <-ch:
		if evt.Type != EventUpdate {
			t.Fatalf("expected MODIFIED, got %s", evt.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for update event")
	}

	// Delete
	err = s.Pods.Delete(ctx, "default.nginx")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	select {
	case evt := <-ch:
		if evt.Type != EventDelete {
			t.Fatalf("expected DELETED, got %s", evt.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for delete event")
	}
}

func TestImportExport(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	// Manually put some data
	type SimplePod struct {
		Name string `json:"name"`
	}
	_, _ = s.Pods.PutJSON(ctx, "infra.nats", &SimplePod{Name: "nats"})
	_, _ = s.Pods.PutJSON(ctx, "dns.mdns-gw", &SimplePod{Name: "mdns-gw"})

	// Export
	exported, err := s.ExportYAML(ctx)
	if err != nil {
		t.Fatalf("ExportYAML: %v", err)
	}
	if len(exported) == 0 {
		t.Fatal("expected non-empty export")
	}

	t.Logf("exported:\n%s", string(exported))
}

func TestMultipleBuckets(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	// Verify all buckets are independent
	_, _ = s.Pods.Put(ctx, "test.key", []byte("pod"))
	_, _ = s.ConfigMaps.Put(ctx, "test.key", []byte("configmap"))
	_, _ = s.Namespaces.Put(ctx, "test", []byte("namespace"))

	val, _, _ := s.Pods.Get(ctx, "test.key")
	if string(val) != "pod" {
		t.Fatalf("pods bucket: unexpected value: %s", val)
	}

	val, _, _ = s.ConfigMaps.Get(ctx, "test.key")
	if string(val) != "configmap" {
		t.Fatalf("configmaps bucket: unexpected value: %s", val)
	}

	val, _, _ = s.Namespaces.Get(ctx, "test")
	if string(val) != "namespace" {
		t.Fatalf("namespaces bucket: unexpected value: %s", val)
	}
}

func TestListJSON(t *testing.T) {
	s, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()

	type Item struct {
		Name string `json:"name"`
	}

	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("ns.item-%d", i)
		_, _ = s.Pods.PutJSON(ctx, key, &Item{Name: fmt.Sprintf("item-%d", i)})
	}

	items, err := s.Pods.ListJSON(ctx, "ns.", func() interface{} { return &Item{} })
	if err != nil {
		t.Fatalf("ListJSON: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("expected 5 items, got %d", len(items))
	}

	for _, item := range items {
		i := item.(*Item)
		data, _ := json.Marshal(i)
		t.Logf("item: %s", data)
	}
}
