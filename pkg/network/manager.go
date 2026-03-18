package network

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/dns"
	"github.com/glennswest/mkube/pkg/network/ipam"
)

// networkState holds per-network config and cached zone ID.
type networkState struct {
	def     config.NetworkDef
	subnet  *net.IPNet
	gateway net.IP
	zoneID  string // cached MicroDNS zone UUID
}

// allocation tracks which network a veth belongs to.
type allocation struct {
	networkName string
	ip          net.IP
	hostname    string
}

// Manager handles IP address allocation (IPAM), veth interface creation,
// DNS registration, and bridge port management across multiple networks.
type Manager struct {
	networks map[string]*networkState // keyed by network name
	netOrder []string                 // ordered network names (first = default)
	driver   NetworkDriver
	dns      *dns.Client
	ipam     *ipam.Allocator
	state    *stateStore
	log      *zap.SugaredLogger

	mu     sync.Mutex
	allocs map[string]*allocation // veth name -> allocation info
}

// ManagerOpts are optional settings for NewManager.
type ManagerOpts struct {
	StatePath string // path for state persistence (empty = no persistence)
}

// NewManager initializes the network manager with multiple networks.
func NewManager(networks []config.NetworkDef, driver NetworkDriver, dnsClient *dns.Client, log *zap.SugaredLogger, opts ...ManagerOpts) (*Manager, error) {
	var o ManagerOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	alloc := ipam.NewAllocator()
	ss := newStateStore(o.StatePath)

	mgr := &Manager{
		networks: make(map[string]*networkState, len(networks)),
		driver:   driver,
		dns:      dnsClient,
		ipam:     alloc,
		state:    ss,
		log:      log,
		allocs:   make(map[string]*allocation),
	}

	for _, netDef := range networks {
		_, subnet, err := net.ParseCIDR(netDef.CIDR)
		if err != nil {
			return nil, fmt.Errorf("parsing CIDR %q for network %s: %w", netDef.CIDR, netDef.Name, err)
		}

		gateway := make(net.IP, len(subnet.IP))
		copy(gateway, subnet.IP)
		gateway[len(gateway)-1] = 1

		if netDef.Gateway != "" {
			gateway = net.ParseIP(netDef.Gateway)
			if gateway == nil {
				return nil, fmt.Errorf("invalid gateway IP %q for network %s", netDef.Gateway, netDef.Name)
			}
		}

		ns := &networkState{
			def:     netDef,
			subnet:  subnet,
			gateway: gateway,
		}

		mgr.networks[netDef.Name] = ns
		mgr.netOrder = append(mgr.netOrder, netDef.Name)

		// Register IPAM pool with optional allocation range
		var poolOpts []ipam.PoolOpts
		if netDef.IPAMStart != "" || netDef.IPAMEnd != "" {
			o := ipam.PoolOpts{}
			if netDef.IPAMStart != "" {
				o.AllocStart = net.ParseIP(netDef.IPAMStart)
				if o.AllocStart == nil {
					return nil, fmt.Errorf("invalid ipamStart %q for network %s", netDef.IPAMStart, netDef.Name)
				}
			}
			if netDef.IPAMEnd != "" {
				o.AllocEnd = net.ParseIP(netDef.IPAMEnd)
				if o.AllocEnd == nil {
					return nil, fmt.Errorf("invalid ipamEnd %q for network %s", netDef.IPAMEnd, netDef.Name)
				}
			}
			poolOpts = append(poolOpts, o)
		}
		alloc.AddPool(netDef.Name, subnet, gateway, poolOpts...)

		// Register logical switch in state
		ss.setSwitch(&LogicalSwitch{
			Name:    netDef.Name,
			Bridge:  netDef.Bridge,
			CIDR:    netDef.CIDR,
			Gateway: gateway.String(),
		})
	}

	// Try loading persisted state
	if err := ss.load(); err != nil {
		log.Debugw("no persisted network state, starting fresh", "error", err)
	}

	// Sync existing allocations from the network driver
	if err := mgr.syncExistingAllocations(context.Background()); err != nil {
		log.Warnw("failed to sync existing allocations", "error", err)
	}

	return mgr, nil
}

// InitDNSZones calls EnsureZone for each network that has DNS configured.
// Should be called at startup after NewManager.
func (m *Manager) InitDNSZones(ctx context.Context) {
	if m.dns == nil {
		return
	}
	// Enable batch mode to cache record lists while checking/registering per zone
	m.dns.BeginBatch()
	defer m.dns.EndBatch()

	for _, name := range m.netOrder {
		ns := m.networks[name]
		if ns.def.DNS.Endpoint == "" || ns.def.DNS.Zone == "" {
			continue
		}
		// Skip networks where DNS is managed externally (e.g. gw on pvex.gw.lo).
		// External DNS servers are not guaranteed reachable from this node and
		// would cause timeout delays during boot and reconcile.
		if ns.def.ExternalDNS {
			m.log.Debugw("skipping external DNS network", "network", name, "endpoint", ns.def.DNS.Endpoint)
			continue
		}
		zoneID, err := m.dns.EnsureZone(ctx, ns.def.DNS.Endpoint, ns.def.DNS.Zone)
		if err != nil {
			m.log.Warnw("failed to ensure DNS zone", "network", name, "zone", ns.def.DNS.Zone, "error", err)
			continue
		}
		ns.zoneID = zoneID
		m.log.Debugw("DNS zone ready", "network", name, "zone", ns.def.DNS.Zone, "zoneID", zoneID)

		// Fetch existing records once to avoid creating duplicates on restart,
		// and remove any duplicate A records that have accumulated.
		existing := make(map[string]string) // "name:ip" -> first record ID
		if records, err := m.dns.ListRecords(ctx, ns.def.DNS.Endpoint, zoneID); err == nil {
			for _, r := range records {
				if r.Type != "A" {
					continue
				}
				key := r.Name + ":" + r.Data.Data
				if _, seen := existing[key]; seen {
					// Duplicate — delete it
					if err := m.dns.DeleteRecord(ctx, ns.def.DNS.Endpoint, zoneID, r.ID); err != nil {
						m.log.Warnw("failed to delete duplicate DNS record", "network", name, "name", r.Name, "ip", r.Data.Data, "id", r.ID, "error", err)
					} else {
						m.log.Infow("deleted duplicate DNS record", "network", name, "name", r.Name, "ip", r.Data.Data, "id", r.ID)
					}
				} else {
					existing[key] = r.ID
				}
			}
		}

		// Register static DNS records for infrastructure hosts (routers, gateways, etc.)
		for _, rec := range ns.def.DNS.StaticRecords {
			if _, found := existing[rec.Name+":"+rec.IP]; found {
				continue
			}
			if err := m.dns.RegisterHost(ctx, ns.def.DNS.Endpoint, zoneID, rec.Name, rec.IP, 300); err != nil {
				m.log.Warnw("failed to register static DNS record", "network", name, "name", rec.Name, "ip", rec.IP, "error", err)
			} else {
				m.log.Infow("static DNS record registered", "network", name, "name", rec.Name, "ip", rec.IP)
			}
		}

		// DHCP reservation DNS records are managed by BMH sync + microdns REST API.
		// Static config reservations are obsolete — microdns is the source of truth.

		// Register infrastructure records (gateway + DNS server)
		for _, infra := range []struct{ name, ip string }{
			{"rose1", ns.def.Gateway},
			{"dns", ns.def.DNS.Server},
		} {
			if infra.ip == "" {
				continue
			}
			if _, found := existing[infra.name+":"+infra.ip]; found {
				continue
			}
			if err := m.dns.RegisterHost(ctx, ns.def.DNS.Endpoint, zoneID, infra.name, infra.ip, 300); err != nil {
				m.log.Warnw("failed to register infra DNS record", "network", name, "name", infra.name, "ip", infra.ip, "error", err)
			} else {
				m.log.Infow("infra DNS record registered", "network", name, "name", infra.name, "ip", infra.ip)
			}
		}
	}
}

// AllocateInterface creates a veth, assigns an IP from the specified network,
// registers a DNS A record, and adds it to the network's bridge.
// If networkName is empty, the first (default) network is used.
func (m *Manager) AllocateInterface(ctx context.Context, vethName, hostname, networkName, staticIP string) (ip string, gateway string, dnsServer string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, err := m.resolveNetwork(networkName)
	if err != nil {
		return "", "", "", err
	}

	// Allocate an IP via IPAM
	var allocatedIP net.IP
	if staticIP != "" {
		allocatedIP = net.ParseIP(staticIP)
		if allocatedIP == nil {
			return "", "", "", fmt.Errorf("invalid static IP %q", staticIP)
		}
		if err := m.ipam.AllocateStatic(ns.def.Name, vethName, allocatedIP); err != nil {
			return "", "", "", err
		}
	} else {
		allocatedIP, err = m.ipam.Allocate(ns.def.Name, vethName)
		if err != nil {
			return "", "", "", err
		}
	}

	ones, _ := ns.subnet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", allocatedIP.String(), ones)
	gw := ns.gateway.String()

	// Create port via network driver
	if err := m.driver.CreatePort(ctx, vethName, ipCIDR, gw); err != nil {
		m.ipam.Release(ns.def.Name, vethName)
		return "", "", "", fmt.Errorf("creating veth %s: %w", vethName, err)
	}

	// Attach to bridge
	if err := m.driver.AttachPort(ctx, ns.def.Bridge, vethName); err != nil {
		_ = m.driver.DeletePort(ctx, vethName)
		m.ipam.Release(ns.def.Name, vethName)
		return "", "", "", fmt.Errorf("adding %s to bridge %s: %w", vethName, ns.def.Bridge, err)
	}

	m.allocs[vethName] = &allocation{
		networkName: ns.def.Name,
		ip:          allocatedIP,
		hostname:    hostname,
	}

	// Record in state
	m.state.setPort(&LogicalPort{
		Name:     vethName,
		Switch:   ns.def.Name,
		Address:  ipCIDR,
		Gateway:  gw,
		Hostname: hostname,
		NodeName: m.driver.NodeName(),
	})
	if err := m.state.save(); err != nil {
		m.log.Warnw("failed to persist network state", "error", err)
	}

	// Register DNS A record
	if m.dns != nil && ns.zoneID != "" && hostname != "" {
		if regErr := m.dns.RegisterHost(ctx, ns.def.DNS.Endpoint, ns.zoneID, hostname, allocatedIP.String(), 60); regErr != nil {
			m.log.Warnw("failed to register DNS", "hostname", hostname, "ip", allocatedIP, "error", regErr)
		}
	}

	dnsServerIP := ns.def.DNS.Server
	m.log.Infow("interface allocated",
		"veth", vethName, "ip", ipCIDR, "bridge", ns.def.Bridge,
		"network", ns.def.Name, "dns", dnsServerIP)

	return ipCIDR, gw, dnsServerIP, nil
}

// ReleaseInterface removes the veth, deregisters DNS, and returns the IP.
func (m *Manager) ReleaseInterface(ctx context.Context, vethName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Deregister DNS
	if alloc, ok := m.allocs[vethName]; ok && m.dns != nil && alloc.hostname != "" {
		if ns, exists := m.networks[alloc.networkName]; exists && ns.zoneID != "" {
			if err := m.dns.DeregisterHost(ctx, ns.def.DNS.Endpoint, ns.zoneID, alloc.hostname); err != nil {
				m.log.Warnw("failed to deregister DNS", "hostname", alloc.hostname, "error", err)
			}
		}
	}

	if err := m.driver.DeletePort(ctx, vethName); err != nil {
		m.log.Warnw("error removing veth — keeping internal state for retry", "name", vethName, "error", err)
		return fmt.Errorf("delete veth %s: %w", vethName, err)
	}

	// Clean up allocation only after the physical veth is gone
	if alloc, ok := m.allocs[vethName]; ok {
		m.ipam.Release(alloc.networkName, vethName)
		delete(m.allocs, vethName)
	} else {
		// m.allocs may have been cleared (e.g. by a prior release or network
		// unregister) while the IPAM pool still holds the key. Fall back to
		// releasing from every pool so stale entries are always cleaned.
		m.ipam.ReleaseFromAll(vethName)
	}

	// Remove from state
	m.state.removePort(vethName)
	if err := m.state.save(); err != nil {
		m.log.Warnw("failed to persist network state", "error", err)
	}

	m.log.Infow("interface released", "veth", vethName)
	return nil
}

// DNSClient returns the underlying DNS client for direct zone operations.
func (m *Manager) DNSClient() *dns.Client {
	return m.dns
}

// GetPortInfo returns the IP (without CIDR mask) and network name for a veth allocation.
func (m *Manager) GetPortInfo(vethName string) (ip, networkName string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	alloc, exists := m.allocs[vethName]
	if !exists {
		return "", "", false
	}
	return alloc.ip.String(), alloc.networkName, true
}

// RegisterDNS registers an additional A record in the named network's DNS zone.
func (m *Manager) RegisterDNS(ctx context.Context, networkName, hostname, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, err := m.resolveNetwork(networkName)
	if err != nil {
		return err
	}
	if m.dns == nil || ns.zoneID == "" {
		return nil
	}
	return m.dns.RegisterHost(ctx, ns.def.DNS.Endpoint, ns.zoneID, hostname, ip, 60)
}

// CleanStaleDNS removes A records for hostname that don't match the given IP.
// Used during pod alias registration to clean up stale records from previous IPs.
func (m *Manager) CleanStaleDNS(ctx context.Context, networkName, hostname, currentIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, err := m.resolveNetwork(networkName)
	if err != nil {
		return err
	}
	if m.dns == nil || ns.zoneID == "" {
		return nil
	}
	return m.dns.CleanStaleRecords(ctx, ns.def.DNS.Endpoint, ns.zoneID, hostname, currentIP)
}

// DeregisterDNS removes the A record matching hostname+ip from the named network's DNS zone.
func (m *Manager) DeregisterDNS(ctx context.Context, networkName, hostname, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, err := m.resolveNetwork(networkName)
	if err != nil {
		return err
	}
	if m.dns == nil || ns.zoneID == "" {
		return nil
	}
	return m.dns.DeregisterHostByIP(ctx, ns.def.DNS.Endpoint, ns.zoneID, hostname, ip)
}

// GetAllocations returns a snapshot of current IP allocations across all networks.
func (m *Manager) GetAllocations() map[string]string {
	return m.ipam.AllAllocations()
}

// Networks returns the ordered list of network names.
func (m *Manager) Networks() []string {
	return m.netOrder
}

// NetworkDef returns the config.NetworkDef for the named network.
func (m *Manager) NetworkDef(name string) (config.NetworkDef, bool) {
	ns, ok := m.networks[name]
	if !ok {
		return config.NetworkDef{}, false
	}
	return ns.def, true
}

// NetworkZoneID returns the cached MicroDNS zone UUID for the named network.
func (m *Manager) NetworkZoneID(name string) (string, bool) {
	ns, ok := m.networks[name]
	if !ok {
		return "", false
	}
	return ns.zoneID, ns.zoneID != ""
}

// ListActualPorts delegates to the network driver to list all ports/veths.
func (m *Manager) ListActualPorts(ctx context.Context) ([]PortInfo, error) {
	return m.driver.ListPorts(ctx)
}

// resolveNetwork returns the networkState for the given name, or the default.
// Must be called with m.mu held.
func (m *Manager) resolveNetwork(name string) (*networkState, error) {
	if name == "" && len(m.netOrder) > 0 {
		return m.networks[m.netOrder[0]], nil
	}
	if ns, ok := m.networks[name]; ok {
		return ns, nil
	}
	return nil, fmt.Errorf("network %q not found", name)
}

// ─── Sync ───────────────────────────────────────────────────────────────────

func (m *Manager) syncExistingAllocations(ctx context.Context) error {
	// ListPorts does I/O — call without holding the lock
	ports, err := m.driver.ListPorts(ctx)
	if err != nil {
		return err
	}

	// Collect results, then write under lock
	type syncEntry struct {
		name     string
		poolName string
		ip       net.IP
	}
	var entries []syncEntry

	for _, p := range ports {
		if p.Address == "" {
			continue
		}
		ip, _, err := net.ParseCIDR(p.Address)
		if err != nil {
			ip = net.ParseIP(p.Address)
		}
		if ip == nil {
			continue
		}

		// Find which network this IP belongs to
		poolName := m.ipam.PoolForIP(ip)
		if poolName == "" {
			continue
		}

		entries = append(entries, syncEntry{name: p.Name, poolName: poolName, ip: ip})
	}

	// Write allocations under lock to prevent concurrent map access
	m.mu.Lock()
	for _, e := range entries {
		m.ipam.Record(e.poolName, e.name, e.ip)
		m.allocs[e.name] = &allocation{
			networkName: e.poolName,
			ip:          e.ip,
			hostname:    extractHostname(e.name),
		}
		m.log.Debugw("synced existing allocation", "veth", e.name, "ip", e.ip, "network", e.poolName)
	}
	m.mu.Unlock()

	total := len(m.ipam.AllAllocations())
	m.log.Debugw("synced existing allocations", "count", total)
	return nil
}

// RegisterNetwork dynamically adds a new network to the manager and IPAM
// allocator. This is used when Network CRDs are created via API (not present
// in the static config.yaml at boot time). It is idempotent — if the network
// already exists, it is a no-op.
func (m *Manager) RegisterNetwork(netDef config.NetworkDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Already registered — no-op
	if _, exists := m.networks[netDef.Name]; exists {
		return nil
	}

	_, subnet, err := net.ParseCIDR(netDef.CIDR)
	if err != nil {
		return fmt.Errorf("parsing CIDR %q for network %s: %w", netDef.CIDR, netDef.Name, err)
	}

	gateway := make(net.IP, len(subnet.IP))
	copy(gateway, subnet.IP)
	gateway[len(gateway)-1] = 1

	if netDef.Gateway != "" {
		gateway = net.ParseIP(netDef.Gateway)
		if gateway == nil {
			return fmt.Errorf("invalid gateway IP %q for network %s", netDef.Gateway, netDef.Name)
		}
	}

	ns := &networkState{
		def:     netDef,
		subnet:  subnet,
		gateway: gateway,
	}

	m.networks[netDef.Name] = ns
	m.netOrder = append(m.netOrder, netDef.Name)

	// Register IPAM pool
	var poolOpts []ipam.PoolOpts
	if netDef.IPAMStart != "" || netDef.IPAMEnd != "" {
		o := ipam.PoolOpts{}
		if netDef.IPAMStart != "" {
			o.AllocStart = net.ParseIP(netDef.IPAMStart)
			if o.AllocStart == nil {
				return fmt.Errorf("invalid ipamStart %q for network %s", netDef.IPAMStart, netDef.Name)
			}
		}
		if netDef.IPAMEnd != "" {
			o.AllocEnd = net.ParseIP(netDef.IPAMEnd)
			if o.AllocEnd == nil {
				return fmt.Errorf("invalid ipamEnd %q for network %s", netDef.IPAMEnd, netDef.Name)
			}
		}
		poolOpts = append(poolOpts, o)
	}
	m.ipam.AddPool(netDef.Name, subnet, gateway, poolOpts...)

	m.state.setSwitch(&LogicalSwitch{
		Name:    netDef.Name,
		Bridge:  netDef.Bridge,
		CIDR:    netDef.CIDR,
		Gateway: gateway.String(),
	})

	m.log.Infow("registered dynamic network", "name", netDef.Name, "cidr", netDef.CIDR, "bridge", netDef.Bridge)
	return nil
}

// UnregisterNetwork removes a dynamically registered network from the manager
// and IPAM allocator. This is the reverse of RegisterNetwork and is called
// when a Network CRD is deleted. It is a no-op if the network doesn't exist.
func (m *Manager) UnregisterNetwork(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.networks[name]; !exists {
		return
	}

	delete(m.networks, name)
	m.ipam.RemovePool(name)

	// Remove from ordered list
	for i, n := range m.netOrder {
		if n == name {
			m.netOrder = append(m.netOrder[:i], m.netOrder[i+1:]...)
			break
		}
	}

	// Remove all ports (veths) belonging to this network so IPAM entries
	// don't persist and block re-creation of the same network.
	m.state.removePortsBySwitch(name)
	m.state.removeSwitch(name)

	// Clean up m.allocs entries for this network so ReleaseInterface can
	// still reach the IPAM pool via the alloc's networkName.
	for k, alloc := range m.allocs {
		if alloc.networkName == name {
			delete(m.allocs, k)
		}
	}

	m.log.Infow("unregistered dynamic network", "name", name)
}

// ResyncAllocations re-queries all veths from the device and fills in any
// missing IPAM allocations. Idempotent — existing entries are overwritten
// with the same data. Called during reconcile to ensure IPAM tracks veths
// for pods that were tracked via the "already exists" path without calling
// AllocateInterface.
func (m *Manager) ResyncAllocations(ctx context.Context) error {
	return m.syncExistingAllocations(ctx)
}

// extractHostname derives a hostname from a veth name like "veth_ns_pod_0".
func extractHostname(vethName string) string {
	parts := strings.SplitN(vethName, "_", 4)
	if len(parts) >= 3 {
		return parts[2]
	}
	return vethName
}
