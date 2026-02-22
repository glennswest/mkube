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

		// Register IPAM pool
		alloc.AddPool(netDef.Name, subnet, gateway)

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
	for _, name := range m.netOrder {
		ns := m.networks[name]
		if ns.def.DNS.Endpoint == "" || ns.def.DNS.Zone == "" {
			continue
		}
		zoneID, err := m.dns.EnsureZone(ctx, ns.def.DNS.Endpoint, ns.def.DNS.Zone)
		if err != nil {
			m.log.Warnw("failed to ensure DNS zone", "network", name, "zone", ns.def.DNS.Zone, "error", err)
			continue
		}
		ns.zoneID = zoneID
		m.log.Infow("DNS zone ready", "network", name, "zone", ns.def.DNS.Zone, "zoneID", zoneID)

		// Register static DNS records for infrastructure hosts (routers, gateways, etc.)
		// Fetch existing records once to avoid creating duplicates on restart.
		if len(ns.def.DNS.StaticRecords) > 0 {
			existing := make(map[string]string) // "name:ip" -> record ID
			if records, err := m.dns.ListRecords(ctx, ns.def.DNS.Endpoint, zoneID); err == nil {
				for _, r := range records {
					if r.Type == "A" {
						existing[r.Name+":"+r.Data.Data] = r.ID
					}
				}
			}
			for _, rec := range ns.def.DNS.StaticRecords {
				if _, found := existing[rec.Name+":"+rec.IP]; found {
					m.log.Debugw("static DNS record already exists", "name", rec.Name, "ip", rec.IP)
					continue
				}
				if err := m.dns.RegisterHost(ctx, ns.def.DNS.Endpoint, zoneID, rec.Name, rec.IP, 300); err != nil {
					m.log.Warnw("failed to register static DNS record", "network", name, "name", rec.Name, "ip", rec.IP, "error", err)
				} else {
					m.log.Infow("static DNS record registered", "network", name, "name", rec.Name, "ip", rec.IP)
				}
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
		m.log.Warnw("error removing veth", "name", vethName, "error", err)
	}

	// Clean up allocation
	if alloc, ok := m.allocs[vethName]; ok {
		m.ipam.Release(alloc.networkName, vethName)
		delete(m.allocs, vethName)
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
	ports, err := m.driver.ListPorts(ctx)
	if err != nil {
		return err
	}

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

		m.ipam.Record(poolName, p.Name, ip)
		m.allocs[p.Name] = &allocation{
			networkName: poolName,
			ip:          ip,
			hostname:    extractHostname(p.Name),
		}
		m.log.Debugw("synced existing allocation", "veth", p.Name, "ip", ip, "network", poolName)
	}

	total := len(m.ipam.AllAllocations())
	m.log.Infow("synced existing allocations", "count", total)
	return nil
}

// extractHostname derives a hostname from a veth name like "veth_ns_pod_0".
func extractHostname(vethName string) string {
	parts := strings.SplitN(vethName, "_", 4)
	if len(parts) >= 3 {
		return parts[2]
	}
	return vethName
}
