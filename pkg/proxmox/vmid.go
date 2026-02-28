package proxmox

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// VMIDAllocator manages VMID allocation within a configured range.
// It maintains a bidirectional name↔VMID mapping.
type VMIDAllocator struct {
	mu       sync.Mutex
	rangeMin int
	rangeMax int
	used     map[int]string // vmid → container name
	names    map[string]int // container name → vmid
}

// NewVMIDAllocator creates a VMID allocator for the given range (e.g. "200-299").
func NewVMIDAllocator(rangeSpec string) (*VMIDAllocator, error) {
	parts := strings.SplitN(rangeSpec, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid VMID range %q: expected format 'MIN-MAX'", rangeSpec)
	}

	min, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("invalid VMID range min %q: %w", parts[0], err)
	}

	max, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid VMID range max %q: %w", parts[1], err)
	}

	if min > max {
		return nil, fmt.Errorf("invalid VMID range: min %d > max %d", min, max)
	}

	return &VMIDAllocator{
		rangeMin: min,
		rangeMax: max,
		used:     make(map[int]string),
		names:    make(map[string]int),
	}, nil
}

// Allocate returns the next available VMID in the range for the given name.
// If the name already has a VMID, it returns the existing one.
func (a *VMIDAllocator) Allocate(name string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Return existing allocation
	if vmid, ok := a.names[name]; ok {
		return vmid, nil
	}

	// Find lowest available in range
	for vmid := a.rangeMin; vmid <= a.rangeMax; vmid++ {
		if _, used := a.used[vmid]; !used {
			a.used[vmid] = name
			a.names[name] = vmid
			return vmid, nil
		}
	}

	return 0, fmt.Errorf("no available VMIDs in range %d-%d", a.rangeMin, a.rangeMax)
}

// Release frees a VMID allocation by name.
func (a *VMIDAllocator) Release(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if vmid, ok := a.names[name]; ok {
		delete(a.used, vmid)
		delete(a.names, name)
	}
}

// MarkUsed records an existing VMID→name mapping (from discovery).
func (a *VMIDAllocator) MarkUsed(vmid int, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.used[vmid] = name
	if name != "" {
		a.names[name] = vmid
	}
}

// Lookup returns the VMID for a container name, or 0 if not found.
func (a *VMIDAllocator) Lookup(name string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	vmid, ok := a.names[name]
	return vmid, ok
}

// LookupName returns the container name for a VMID, or empty if not found.
func (a *VMIDAllocator) LookupName(vmid int) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	name, ok := a.used[vmid]
	return name, ok
}

// InRange returns true if the given VMID is within the allocator's range.
func (a *VMIDAllocator) InRange(vmid int) bool {
	return vmid >= a.rangeMin && vmid <= a.rangeMax
}
