package safemap

import (
	"sync"
	"testing"
)

func TestBasicCRUD(t *testing.T) {
	m := New[string, int]()

	// Set and Get
	m.Set("a", 1)
	v, ok := m.Get("a")
	if !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v; want 1, true", v, ok)
	}

	// Has
	if !m.Has("a") {
		t.Fatal("Has(a) = false; want true")
	}
	if m.Has("b") {
		t.Fatal("Has(b) = true; want false")
	}

	// Len
	if m.Len() != 1 {
		t.Fatalf("Len() = %d; want 1", m.Len())
	}

	// Delete
	m.Delete("a")
	_, ok = m.Get("a")
	if ok {
		t.Fatal("Get(a) after Delete should return false")
	}
	if m.Len() != 0 {
		t.Fatalf("Len() = %d after Delete; want 0", m.Len())
	}
}

func TestSnapshot(t *testing.T) {
	m := New[string, int]()
	m.Set("x", 10)
	m.Set("y", 20)

	snap := m.Snapshot()
	if len(snap) != 2 || snap["x"] != 10 || snap["y"] != 20 {
		t.Fatalf("Snapshot mismatch: %v", snap)
	}

	// Mutating the snapshot must not affect the original.
	snap["x"] = 999
	v, _ := m.Get("x")
	if v != 10 {
		t.Fatal("Snapshot mutation leaked into SafeMap")
	}
}

func TestKeysValues(t *testing.T) {
	m := New[string, int]()
	m.Set("a", 1)
	m.Set("b", 2)

	keys := m.Keys()
	if len(keys) != 2 {
		t.Fatalf("Keys() len = %d; want 2", len(keys))
	}
	vals := m.Values()
	if len(vals) != 2 {
		t.Fatalf("Values() len = %d; want 2", len(vals))
	}
}

func TestRange(t *testing.T) {
	m := New[string, int]()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("c", 3)

	sum := 0
	m.Range(func(_ string, v int) bool {
		sum += v
		return true
	})
	if sum != 6 {
		t.Fatalf("Range sum = %d; want 6", sum)
	}

	// Early exit
	count := 0
	m.Range(func(_ string, _ int) bool {
		count++
		return false // stop after first
	})
	if count != 1 {
		t.Fatalf("Range early exit count = %d; want 1", count)
	}
}

func TestSetIfAbsent(t *testing.T) {
	m := New[string, int]()

	if !m.SetIfAbsent("a", 1) {
		t.Fatal("SetIfAbsent on empty map should return true")
	}
	if m.SetIfAbsent("a", 2) {
		t.Fatal("SetIfAbsent on existing key should return false")
	}
	v, _ := m.Get("a")
	if v != 1 {
		t.Fatalf("Get(a) = %d; want 1 (not overwritten)", v)
	}
}

func TestGetAndDelete(t *testing.T) {
	m := New[string, int]()
	m.Set("a", 42)

	v, ok := m.GetAndDelete("a")
	if !ok || v != 42 {
		t.Fatalf("GetAndDelete(a) = %d, %v; want 42, true", v, ok)
	}
	if m.Has("a") {
		t.Fatal("key should be deleted after GetAndDelete")
	}

	_, ok = m.GetAndDelete("missing")
	if ok {
		t.Fatal("GetAndDelete on missing key should return false")
	}
}

func TestIncrement(t *testing.T) {
	m := New[string, int]()

	v := Increment(m, "counter", 1)
	if v != 1 {
		t.Fatalf("Increment from zero = %d; want 1", v)
	}
	v = Increment(m, "counter", 5)
	if v != 6 {
		t.Fatalf("Increment = %d; want 6", v)
	}
}

func TestConcurrent(t *testing.T) {
	m := New[int, int]()
	var wg sync.WaitGroup
	n := 100

	// Concurrent writers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Set(i, i*10)
		}(i)
	}

	// Concurrent readers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Get(i)
			m.Has(i)
			m.Len()
			m.Snapshot()
		}(i)
	}

	wg.Wait()

	if m.Len() != n {
		t.Fatalf("Len() = %d; want %d", m.Len(), n)
	}
}

func TestGetMissing(t *testing.T) {
	m := New[string, *int]()
	v, ok := m.Get("nope")
	if ok || v != nil {
		t.Fatalf("Get missing key = %v, %v; want nil, false", v, ok)
	}
}
