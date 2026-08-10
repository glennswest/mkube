package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger {
	logger, _ := zap.NewDevelopment()
	return logger.Sugar()
}

// withHealth wraps a mock handler so it also answers the /api/v1/health
// alive probe that gates client operations.
func withHealth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}
}

func TestEnsureZone_ExistingZone(t *testing.T) {
	zones := []Zone{{ID: "zone-123", Name: "gt.lo"}}
	srv := httptest.NewServer(withHealth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/zones" {
			_ = json.NewEncoder(w).Encode(zones)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	id, err := c.EnsureZone(context.Background(), srv.URL, "gt.lo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "zone-123" {
		t.Errorf("expected zone-123, got %s", id)
	}
}

func TestEnsureZone_CreatesZone(t *testing.T) {
	srv := httptest.NewServer(withHealth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/zones" {
			_ = json.NewEncoder(w).Encode([]Zone{})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/zones" {
			var req createZoneRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Zone{ID: "new-zone-456", Name: req.Name})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	id, err := c.EnsureZone(context.Background(), srv.URL, "gt.lo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "new-zone-456" {
		t.Errorf("expected new-zone-456, got %s", id)
	}
}

func TestRegisterHost(t *testing.T) {
	var receivedReq createRecordRequest
	srv := httptest.NewServer(withHealth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/zones/zone-123/records" {
			_ = json.NewDecoder(r.Body).Decode(&receivedReq)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Record{ID: "rec-1", Name: receivedReq.Name, Type: "A", Data: receivedReq.Data})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	err := c.RegisterHost(context.Background(), srv.URL, "zone-123", "myapp", "192.168.200.5", 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedReq.Name != "myapp" {
		t.Errorf("expected hostname 'myapp', got %q", receivedReq.Name)
	}
	if receivedReq.Data.Data != "192.168.200.5" {
		t.Errorf("expected IP '192.168.200.5', got %q", receivedReq.Data.Data)
	}
	if receivedReq.Data.Type != "A" {
		t.Errorf("expected record type 'A', got %q", receivedReq.Data.Type)
	}
	if receivedReq.TTL != 60 {
		t.Errorf("expected TTL 60, got %d", receivedReq.TTL)
	}
}

func TestDeregisterHost(t *testing.T) {
	deleted := make(map[string]bool)
	records := []Record{
		{ID: "rec-1", Name: "myapp", Type: "A", Data: RecordData{Type: "A", Data: "192.168.200.5"}},
		{ID: "rec-2", Name: "other", Type: "A", Data: RecordData{Type: "A", Data: "192.168.200.6"}},
		{ID: "rec-3", Name: "myapp", Type: "CNAME", Data: RecordData{Type: "CNAME", Data: "alias"}},
	}

	srv := httptest.NewServer(withHealth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/zones/zone-123/records" {
			_ = json.NewEncoder(w).Encode(records)
			return
		}
		if r.Method == http.MethodDelete {
			// Extract record ID from path
			deleted[r.URL.Path] = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	err := c.DeregisterHost(context.Background(), srv.URL, "zone-123", "myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should only delete rec-1 (name=myapp, type=A), not rec-2 or rec-3
	if !deleted["/api/v1/zones/zone-123/records/rec-1"] {
		t.Error("expected rec-1 to be deleted")
	}
	if deleted["/api/v1/zones/zone-123/records/rec-2"] {
		t.Error("rec-2 should not have been deleted")
	}
	if deleted["/api/v1/zones/zone-123/records/rec-3"] {
		t.Error("rec-3 (CNAME) should not have been deleted")
	}
}

func TestEndpointAlive_DownEndpointCachedSkip(t *testing.T) {
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			probes++
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	for i := 0; i < 3; i++ {
		if err := c.RegisterHost(context.Background(), srv.URL, "zone-1", "host", "1.2.3.4", 60); err == nil {
			t.Fatal("expected error for down endpoint")
		}
	}
	if probes != 1 {
		t.Errorf("expected 1 health probe within TTL, got %d", probes)
	}
}

func TestEnsureZone_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	_, err := c.EnsureZone(context.Background(), srv.URL, "gt.lo")
	if err == nil {
		t.Error("expected error for server error response")
	}
}

func TestRegisterHost_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(testLogger())
	err := c.RegisterHost(context.Background(), srv.URL, "zone-1", "host", "1.2.3.4", 60)
	if err == nil {
		t.Error("expected error for server error response")
	}
}
