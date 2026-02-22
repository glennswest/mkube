package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/config"
)

// dhcpLease is the JSON structure from microdns DHCP lease API.
type dhcpLease struct {
	IP      string `json:"ip"`
	IPAddr  string `json:"ip_addr"`  // microdns field name
	MAC     string `json:"mac"`
	MACAddr string `json:"mac_addr"` // microdns field name
}

func (l dhcpLease) getIP() string {
	if l.IPAddr != "" {
		return l.IPAddr
	}
	return l.IP
}

func (l dhcpLease) getMAC() string {
	if l.MACAddr != "" {
		return l.MACAddr
	}
	return l.MAC
}

// microdnsLeaseEvent is the JSON structure published by microdns to NATS.
type microdnsLeaseEvent struct {
	Type       string `json:"type"`        // "LeaseCreated" or "LeaseReleased"
	InstanceID string `json:"instance_id"`
	IPAddr     string `json:"ip_addr"`
	MACAddr    string `json:"mac_addr"`
	Hostname   string `json:"hostname"`
	PoolID     string `json:"pool_id"`
	Timestamp  string `json:"timestamp"`
}

// dhcpNetworkIndex is a precomputed lookup structure built from the network
// config so processDHCPLease never needs hardcoded subnets or hostnames.
type dhcpNetworkIndex struct {
	// reservationsByIP maps an IP string to its reservation + network name.
	// Built from all networks' DHCP reservations.
	reservationsByIP map[string]dhcpReservationEntry

	// subnets maps parsed CIDRs to their network name so we can identify
	// which network an unreserved lease belongs to.
	subnets []dhcpSubnetEntry
}

type dhcpReservationEntry struct {
	Network  string
	Hostname string
}

type dhcpSubnetEntry struct {
	Network string
	Subnet  *net.IPNet
}

// buildDHCPIndex creates the lookup index from the live network config.
func buildDHCPIndex(networks []config.NetworkDef) *dhcpNetworkIndex {
	idx := &dhcpNetworkIndex{
		reservationsByIP: make(map[string]dhcpReservationEntry),
	}
	for _, n := range networks {
		if n.CIDR != "" {
			_, subnet, err := net.ParseCIDR(n.CIDR)
			if err == nil {
				idx.subnets = append(idx.subnets, dhcpSubnetEntry{
					Network: n.Name,
					Subnet:  subnet,
				})
			}
		}
		for _, r := range n.DNS.DHCP.Reservations {
			if r.IP != "" && r.Hostname != "" {
				idx.reservationsByIP[r.IP] = dhcpReservationEntry{
					Network:  n.Name,
					Hostname: r.Hostname,
				}
			}
		}
	}
	return idx
}

// networkForIP returns the network name that contains the given IP,
// or "" if no configured network matches.
func (idx *dhcpNetworkIndex) networkForIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	for _, s := range idx.subnets {
		if s.Subnet.Contains(parsed) {
			return s.Network
		}
	}
	return ""
}

// RunDHCPWatcher subscribes to NATS lease events for instant discovery and
// polls each DHCP-enabled microdns as a fallback consistency check.
func (p *MicroKubeProvider) RunDHCPWatcher(ctx context.Context) {
	// Check if any network has DHCP enabled
	hasAny := false
	for _, n := range p.deps.Config.Networks {
		if n.DNS.DHCP.Enabled {
			hasAny = true
			break
		}
	}
	if !hasAny {
		p.deps.Logger.Info("DHCP watcher disabled (no networks have DHCP enabled)")
		return
	}

	log := p.deps.Logger.Named("dhcp-watcher")

	// Start NATS subscription for instant lease events
	p.startDHCPSubscription(ctx)

	// Fallback poll interval — 5 minutes for consistency checks
	interval := 5 * time.Minute
	log.Infow("DHCP watcher starting", "poll_interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("DHCP watcher stopping")
			return
		case <-ticker.C:
			p.reconcileDHCPLeases(ctx)
		}
	}
}

// startDHCPSubscription subscribes to microdns NATS lease events.
// The wildcard subject microdns.*.leases catches events from every
// microdns instance regardless of network name.
func (p *MicroKubeProvider) startDHCPSubscription(ctx context.Context) {
	if p.deps.Store == nil {
		return
	}

	log := p.deps.Logger.Named("dhcp-watcher")

	_, err := p.deps.Store.Subscribe("microdns.*.leases", func(msg *nats.Msg) {
		var evt microdnsLeaseEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			log.Warnw("failed to unmarshal lease event", "error", err)
			return
		}

		if evt.Type != "LeaseCreated" {
			return
		}

		// Extract network name from subject: microdns.<network>.leases
		network := ""
		parts := strings.SplitN(msg.Subject, ".", 3)
		if len(parts) >= 2 {
			network = parts[1]
		}

		log.Infow("NATS lease event received",
			"type", evt.Type,
			"network", network,
			"ip", evt.IPAddr,
			"mac", evt.MACAddr,
			"hostname", evt.Hostname,
		)

		p.processDHCPLease(ctx, evt.IPAddr, evt.MACAddr, evt.Hostname)
	})
	if err != nil {
		log.Warnw("failed to subscribe to DHCP lease events", "error", err)
		return
	}

	log.Info("DHCP NATS subscription started on microdns.*.leases")
}

// processDHCPLease handles a single DHCP lease from any network.
// It looks up the IP in the reservation index to find the hostname and
// network, falling back to subnet matching for unreserved IPs.
func (p *MicroKubeProvider) processDHCPLease(ctx context.Context, ip, mac, eventHostname string) {
	cfg := p.deps.Config.BMH
	log := p.deps.Logger.Named("dhcp-watcher")

	if ip == "" || mac == "" {
		return
	}

	if p.dhcpIndex == nil {
		return
	}

	// Look up by reservation first (known servers)
	var network, hostname string
	if entry, ok := p.dhcpIndex.reservationsByIP[ip]; ok {
		network = entry.Network
		hostname = entry.Hostname
	} else {
		// Fall back to subnet match for unreserved leases
		network = p.dhcpIndex.networkForIP(ip)
		if network == "" {
			return
		}
		// Use the hostname from the DHCP event, or derive from IP
		hostname = eventHostname
		if hostname == "" {
			hostname = "host-" + strings.ReplaceAll(ip, ".", "-")
		}
	}

	key := network + "/" + hostname

	if _, exists := p.bareMetalHosts[key]; exists {
		return
	}

	log.Infow("auto-discovered host from DHCP",
		"name", hostname,
		"network", network,
		"ip", ip,
		"mac", mac,
	)

	bmh := &BareMetalHost{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              hostname,
			Namespace:         network,
			CreationTimestamp: metav1.Now(),
		},
		Spec: BMHSpec{
			Image: "localboot",
			BMC: BMCDetails{
				Address:  ip,
				Username: "ADMIN",
				Password: "ADMIN",
			},
			BootMACAddress: mac,
		},
		Status: BMHStatus{
			Phase: "Discovered",
			IP:    ip,
		},
	}

	p.bareMetalHosts[key] = bmh

	// Persist to NATS
	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		storeKey := network + "." + hostname
		if _, err := p.deps.Store.BareMetalHosts.PutJSON(ctx, storeKey, bmh); err != nil {
			log.Warnw("failed to persist discovered BMH", "name", hostname, "error", err)
		}
	}

	// Configure IPMI in pxemanager
	if cfg.PXEManagerURL != "" {
		if err := pxeConfigureIPMI(ctx, cfg.PXEManagerURL, hostname, ip, "ADMIN", "ADMIN"); err != nil {
			log.Warnw("failed to configure IPMI for discovered host", "name", hostname, "error", err)
		}
	}
}

// reconcileDHCPLeases polls every DHCP-enabled microdns instance for leases.
func (p *MicroKubeProvider) reconcileDHCPLeases(ctx context.Context) {
	log := p.deps.Logger.Named("dhcp-watcher")

	for _, n := range p.deps.Config.Networks {
		if !n.DNS.DHCP.Enabled || n.DNS.Endpoint == "" {
			continue
		}

		url := n.DNS.Endpoint + "/api/v1/leases"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			log.Warnw("failed to create lease request", "network", n.Name, "error", err)
			continue
		}

		resp, err := pxeHTTPClient.Do(req)
		if err != nil {
			log.Debugw("DHCP lease fetch failed", "network", n.Name, "error", err)
			continue
		}

		var leases []dhcpLease
		if err := json.NewDecoder(resp.Body).Decode(&leases); err != nil {
			resp.Body.Close()
			log.Warnw("failed to decode leases", "network", n.Name, "error", err)
			continue
		}
		resp.Body.Close()

		for _, lease := range leases {
			p.processDHCPLease(ctx, lease.getIP(), lease.getMAC(), "")
		}
	}
}
