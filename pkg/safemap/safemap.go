// Package safemap provides a generic concurrent-safe map with per-instance locking.
package safemap

import "sync"

// Map is a generic concurrent-safe map. Each instance has its own RWMutex,
// enabling fine-grained locking when replacing a single global mutex that
// guards multiple unrelated maps.
type Map[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// New creates a new empty Map.
func New[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{m: make(map[K]V)}
}

// Get returns the value for key and whether it was found.
func (m *Map[K, V]) Get(key K) (V, bool) {
	m.mu.RLock()
	v, ok := m.m[key]
	m.mu.RUnlock()
	return v, ok
}

// Set stores a key-value pair.
func (m *Map[K, V]) Set(key K, val V) {
	m.mu.Lock()
	m.m[key] = val
	m.mu.Unlock()
}

// Delete removes a key.
func (m *Map[K, V]) Delete(key K) {
	m.mu.Lock()
	delete(m.m, key)
	m.mu.Unlock()
}

// Len returns the number of entries.
func (m *Map[K, V]) Len() int {
	m.mu.RLock()
	n := len(m.m)
	m.mu.RUnlock()
	return n
}

// Has returns true if the key exists.
func (m *Map[K, V]) Has(key K) bool {
	m.mu.RLock()
	_, ok := m.m[key]
	m.mu.RUnlock()
	return ok
}

// Snapshot returns a shallow copy of the entire map. The caller owns the
// returned map and can iterate it without holding any lock.
func (m *Map[K, V]) Snapshot() map[K]V {
	m.mu.RLock()
	cp := make(map[K]V, len(m.m))
	for k, v := range m.m {
		cp[k] = v
	}
	m.mu.RUnlock()
	return cp
}

// Values returns a snapshot of all values.
func (m *Map[K, V]) Values() []V {
	m.mu.RLock()
	vals := make([]V, 0, len(m.m))
	for _, v := range m.m {
		vals = append(vals, v)
	}
	m.mu.RUnlock()
	return vals
}

// Keys returns a snapshot of all keys.
func (m *Map[K, V]) Keys() []K {
	m.mu.RLock()
	keys := make([]K, 0, len(m.m))
	for k := range m.m {
		keys = append(keys, k)
	}
	m.mu.RUnlock()
	return keys
}

// Range calls fn for each key-value pair under a read lock.
// If fn returns false, iteration stops.
func (m *Map[K, V]) Range(fn func(K, V) bool) {
	m.mu.RLock()
	for k, v := range m.m {
		if !fn(k, v) {
			break
		}
	}
	m.mu.RUnlock()
}

// SetIfAbsent stores the key-value pair only if the key does not already
// exist. Returns true if the value was set.
func (m *Map[K, V]) SetIfAbsent(key K, val V) bool {
	m.mu.Lock()
	if _, ok := m.m[key]; ok {
		m.mu.Unlock()
		return false
	}
	m.m[key] = val
	m.mu.Unlock()
	return true
}

// GetAndDelete removes a key and returns its previous value.
func (m *Map[K, V]) GetAndDelete(key K) (V, bool) {
	m.mu.Lock()
	v, ok := m.m[key]
	if ok {
		delete(m.m, key)
	}
	m.mu.Unlock()
	return v, ok
}

// Increment atomically adds delta to an integer-valued entry, returning the
// new value. If the key does not exist, it is created with value delta.
// This is a convenience for counter maps (e.g. failure counts).
func Increment[K comparable](m *Map[K, int], key K, delta int) int {
	m.mu.Lock()
	m.m[key] += delta
	v := m.m[key]
	m.mu.Unlock()
	return v
}
