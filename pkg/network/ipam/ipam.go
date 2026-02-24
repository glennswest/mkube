package ipam

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

// Pool tracks IP allocation state for a single subnet.
type Pool struct {
	Subnet     *net.IPNet
	Gateway    net.IP
	Allocated  map[string]net.IP // key (e.g. veth name) -> allocated IP
	NextIP     uint32            // offset from network base
	AllocStart uint32            // first allocatable offset (from network base)
	AllocEnd   uint32            // last allocatable offset (from network base)
}

// PoolOpts are optional parameters for AddPool.
type PoolOpts struct {
	AllocStart net.IP // first allocatable IP (nil = .2)
	AllocEnd   net.IP // last allocatable IP (nil = max usable host)
}

// Allocator manages IP allocation across multiple named pools.
type Allocator struct {
	mu    sync.Mutex
	pools map[string]*Pool // keyed by pool/network name
}

// NewAllocator returns an empty Allocator.
func NewAllocator() *Allocator {
	return &Allocator{
		pools: make(map[string]*Pool),
	}
}

// AddPool registers a subnet for allocation. By default, allocation starts
// at offset 2 (skipping .0 network and .1 gateway) and ends at the last
// usable host address. Use PoolOpts to restrict the allocation range and
// avoid collisions with static server IPs or DHCP reservations.
func (a *Allocator) AddPool(name string, subnet *net.IPNet, gateway net.IP, opts ...PoolOpts) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ones, bits := subnet.Mask.Size()
	maxHosts := uint32(1<<(bits-ones)) - 2

	start := uint32(2)
	end := maxHosts
	baseIP := IPToUint32(subnet.IP)

	if len(opts) > 0 {
		if opts[0].AllocStart != nil {
			start = IPToUint32(opts[0].AllocStart) - baseIP
		}
		if opts[0].AllocEnd != nil {
			end = IPToUint32(opts[0].AllocEnd) - baseIP
		}
	}

	a.pools[name] = &Pool{
		Subnet:     subnet,
		Gateway:    gateway,
		Allocated:  make(map[string]net.IP),
		NextIP:     start,
		AllocStart: start,
		AllocEnd:   end,
	}
}

// Allocate picks the next free IP in the named pool and records it under key.
func (a *Allocator) Allocate(poolName, key string) (net.IP, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return nil, fmt.Errorf("IPAM pool %q not found", poolName)
	}
	return allocateFromPool(pool, key)
}

// AllocateStatic reserves a specific IP in the named pool.
// Returns error if IP is outside subnet, is the gateway, or already taken.
func (a *Allocator) AllocateStatic(poolName, key string, ip net.IP) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return fmt.Errorf("IPAM pool %q not found", poolName)
	}
	if !pool.Subnet.Contains(ip) {
		return fmt.Errorf("IP %s not in subnet %s", ip, pool.Subnet)
	}
	if ip.Equal(pool.Gateway) {
		return fmt.Errorf("IP %s is the gateway", ip)
	}
	for k, existing := range pool.Allocated {
		if existing.Equal(ip) {
			return fmt.Errorf("IP %s already allocated to %s", ip, k)
		}
	}
	pool.Allocated[key] = ip
	return nil
}

// Release frees the IP held by key in the named pool.
func (a *Allocator) Release(poolName, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return
	}
	delete(pool.Allocated, key)
}

// Record marks an IP as already allocated (used during sync).
func (a *Allocator) Record(poolName, key string, ip net.IP) {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return
	}
	pool.Allocated[key] = ip
}

// Get returns the IP allocated for key in poolName, or nil.
func (a *Allocator) Get(poolName, key string) net.IP {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return nil
	}
	return pool.Allocated[key]
}

// PoolAllocations returns a snapshot of allocations for a pool.
func (a *Allocator) PoolAllocations(poolName string) map[string]net.IP {
	a.mu.Lock()
	defer a.mu.Unlock()

	pool, ok := a.pools[poolName]
	if !ok {
		return nil
	}
	out := make(map[string]net.IP, len(pool.Allocated))
	for k, v := range pool.Allocated {
		out[k] = v
	}
	return out
}

// AllAllocations returns all allocations across all pools as key -> IP string.
func (a *Allocator) AllAllocations() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[string]string)
	for _, pool := range a.pools {
		for k, ip := range pool.Allocated {
			out[k] = ip.String()
		}
	}
	return out
}

// PoolForIP returns the pool name that contains ip, or "" if none.
func (a *Allocator) PoolForIP(ip net.IP) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	for name, pool := range a.pools {
		if pool.Subnet.Contains(ip) {
			return name
		}
	}
	return ""
}

// allocateFromPool picks the next free IP from a single pool, constrained
// to the [AllocStart, AllocEnd] range.
func allocateFromPool(pool *Pool, key string) (net.IP, error) {
	rangeSize := pool.AllocEnd - pool.AllocStart + 1
	baseIP := IPToUint32(pool.Subnet.IP)

	for attempts := uint32(0); attempts < rangeSize; attempts++ {
		candidate := baseIP + pool.NextIP
		candidateIP := Uint32ToIP(candidate)

		taken := false
		for _, existing := range pool.Allocated {
			if existing.Equal(candidateIP) {
				taken = true
				break
			}
		}

		pool.NextIP++
		if pool.NextIP > pool.AllocEnd {
			pool.NextIP = pool.AllocStart
		}

		if !taken && !candidateIP.Equal(pool.Gateway) {
			pool.Allocated[key] = candidateIP
			return candidateIP, nil
		}
	}

	return nil, fmt.Errorf("IPAM: no available IPs in %s range %s-%s (all %d addresses allocated)",
		pool.Subnet.String(),
		Uint32ToIP(baseIP+pool.AllocStart).String(),
		Uint32ToIP(baseIP+pool.AllocEnd).String(),
		rangeSize)
}

// MaxUsableIP returns the highest usable host IP in a subnet (broadcast - 1).
func MaxUsableIP(subnet *net.IPNet) net.IP {
	ones, bits := subnet.Mask.Size()
	baseIP := IPToUint32(subnet.IP)
	broadcast := baseIP | uint32((1<<(bits-ones))-1)
	return Uint32ToIP(broadcast - 1)
}

// DNSServerIP returns the IP address a MicroDNS instance should use on a
// subnet. It is MaxUsableIP - 2, leaving the top two addresses free for
// routers or other infrastructure.
func DNSServerIP(subnet *net.IPNet) net.IP {
	ones, bits := subnet.Mask.Size()
	baseIP := IPToUint32(subnet.IP)
	broadcast := baseIP | uint32((1<<(bits-ones))-1)
	return Uint32ToIP(broadcast - 3)
}

// IPToUint32 converts a net.IP (IPv4) to a uint32.
func IPToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return binary.BigEndian.Uint32(ip)
}

// Uint32ToIP converts a uint32 to a net.IP (IPv4).
func Uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
