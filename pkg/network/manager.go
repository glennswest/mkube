package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/dns"
	"github.com/glenneth/microkube/pkg/routeros"
)

// networkState holds per-network IPAM state and cached zone ID.
type networkState struct {
	def       config.NetworkDef
	subnet    *net.IPNet
	gateway   net.IP
	allocated map[string]net.IP // veth name -> allocated IP
	nextIP    uint32
	zoneID    string // cached MicroDNS zone UUID
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
	ros      *routeros.Client
	dns      *dns.Client
	log      *zap.SugaredLogger

	mu     sync.Mutex
	allocs map[string]*allocation // veth name -> allocation info
}

// NewManager initializes the network manager with multiple networks.
func NewManager(networks []config.NetworkDef, ros *routeros.Client, dnsClient *dns.Client, log *zap.SugaredLogger) (*Manager, error) {
	mgr := &Manager{
		networks: make(map[string]*networkState, len(networks)),
		ros:      ros,
		dns:      dnsClient,
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
			def:       netDef,
			subnet:    subnet,
			gateway:   gateway,
			allocated: make(map[string]net.IP),
			nextIP:    2,
		}

		mgr.networks[netDef.Name] = ns
		mgr.netOrder = append(mgr.netOrder, netDef.Name)
	}

	// Sync existing allocations from RouterOS
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
	}
}

// AllocateInterface creates a veth, assigns an IP from the specified network,
// registers a DNS A record, and adds it to the network's bridge.
// If networkName is empty, the first (default) network is used.
func (m *Manager) AllocateInterface(ctx context.Context, vethName, hostname, networkName string) (ip string, gateway string, dnsServer string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ns, err := m.resolveNetwork(networkName)
	if err != nil {
		return "", "", "", err
	}

	// Allocate an IP
	allocatedIP, err := m.allocateIP(ns)
	if err != nil {
		return "", "", "", err
	}

	ones, _ := ns.subnet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", allocatedIP.String(), ones)
	gw := ns.gateway.String()

	// Create veth on RouterOS
	if err := m.ros.CreateVeth(ctx, vethName, ipCIDR, gw); err != nil {
		return "", "", "", fmt.Errorf("creating veth %s: %w", vethName, err)
	}

	// Add to bridge
	if err := m.ros.AddBridgePort(ctx, ns.def.Bridge, vethName); err != nil {
		_ = m.ros.RemoveVeth(ctx, vethName)
		return "", "", "", fmt.Errorf("adding %s to bridge %s: %w", vethName, ns.def.Bridge, err)
	}

	ns.allocated[vethName] = allocatedIP
	m.allocs[vethName] = &allocation{
		networkName: ns.def.Name,
		ip:          allocatedIP,
		hostname:    hostname,
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

	if err := m.ros.RemoveVeth(ctx, vethName); err != nil {
		m.log.Warnw("error removing veth", "name", vethName, "error", err)
	}

	// Clean up allocation from the correct network
	if alloc, ok := m.allocs[vethName]; ok {
		if ns, exists := m.networks[alloc.networkName]; exists {
			delete(ns.allocated, vethName)
		}
		delete(m.allocs, vethName)
	}

	m.log.Infow("interface released", "veth", vethName)
	return nil
}

// DNSClient returns the underlying DNS client for direct zone operations.
func (m *Manager) DNSClient() *dns.Client {
	return m.dns
}

// GetAllocations returns a snapshot of current IP allocations across all networks.
func (m *Manager) GetAllocations() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]string)
	for _, ns := range m.networks {
		for name, ip := range ns.allocated {
			result[name] = ip.String()
		}
	}
	return result
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

// ─── IPAM ───────────────────────────────────────────────────────────────────

func (m *Manager) allocateIP(ns *networkState) (net.IP, error) {
	ones, bits := ns.subnet.Mask.Size()
	maxHosts := uint32(1<<(bits-ones)) - 2

	baseIP := ipToUint32(ns.subnet.IP)

	for attempts := uint32(0); attempts < maxHosts; attempts++ {
		candidate := baseIP + ns.nextIP
		candidateIP := uint32ToIP(candidate)

		taken := false
		for _, existing := range ns.allocated {
			if existing.Equal(candidateIP) {
				taken = true
				break
			}
		}

		ns.nextIP++
		if ns.nextIP > maxHosts {
			ns.nextIP = 2
		}

		if !taken && !candidateIP.Equal(ns.gateway) {
			return candidateIP, nil
		}
	}

	return nil, fmt.Errorf("IPAM: no available IPs in %s (all %d addresses allocated)", ns.subnet.String(), maxHosts)
}

func (m *Manager) syncExistingAllocations(ctx context.Context) error {
	veths, err := m.ros.ListVeths(ctx)
	if err != nil {
		return err
	}

	for _, v := range veths {
		if v.Address == "" {
			continue
		}
		ip, _, err := net.ParseCIDR(v.Address)
		if err != nil {
			ip = net.ParseIP(v.Address)
		}
		if ip == nil {
			continue
		}

		// Find which network this IP belongs to
		for _, ns := range m.networks {
			if ns.subnet.Contains(ip) {
				ns.allocated[v.Name] = ip
				m.allocs[v.Name] = &allocation{
					networkName: ns.def.Name,
					ip:          ip,
					hostname:    extractHostname(v.Name),
				}
				m.log.Debugw("synced existing allocation", "veth", v.Name, "ip", ip, "network", ns.def.Name)
				break
			}
		}
	}

	total := 0
	for _, ns := range m.networks {
		total += len(ns.allocated)
	}
	m.log.Infow("synced existing allocations", "count", total)
	return nil
}

// extractHostname attempts to derive a hostname from a veth name like "veth-myapp-0".
func extractHostname(vethName string) string {
	parts := strings.SplitN(vethName, "-", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return vethName
}

// ─── IP Helpers ─────────────────────────────────────────────────────────────

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return binary.BigEndian.Uint32(ip)
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
