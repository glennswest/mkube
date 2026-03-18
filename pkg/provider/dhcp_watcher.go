package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

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

// mkubeDHCPEvent is the typed event published by mkube for the bmh-operator.
// It enriches the raw microdns event with the resolved network name and type.
type mkubeDHCPEvent struct {
	Type        string `json:"type"`         // "LeaseCreated" or "LeaseReleased"
	Network     string `json:"network"`      // resolved network name (e.g. "g10")
	NetworkType string `json:"network_type"` // "ipmi", "data", etc.
	IPAddr      string `json:"ip_addr"`
	MACAddr     string `json:"mac_addr"`
	Hostname    string `json:"hostname"`
	Timestamp   string `json:"timestamp"`
}

// dhcpNetworkIndex is a precomputed lookup structure built from the network
// config so DHCP event publishing never needs hardcoded subnets or hostnames.
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

// rebuildDHCPIndex rebuilds the DHCP network index from both static config
// and Network CRDs. This ensures the index stays current when networks are
// added/removed/updated via the API.
func (p *MicroKubeProvider) rebuildDHCPIndex() {
	idx := &dhcpNetworkIndex{
		reservationsByIP: make(map[string]dhcpReservationEntry),
	}

	// Include static config networks
	for _, n := range p.deps.Config.Networks {
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

	// Include Network CRDs (these take precedence — overwrite static entries)
	for _, crd := range p.networks {
		if crd.Spec.CIDR != "" {
			_, subnet, err := net.ParseCIDR(crd.Spec.CIDR)
			if err == nil {
				// Avoid duplicate subnet entries
				found := false
				for _, s := range idx.subnets {
					if s.Network == crd.Name {
						found = true
						break
					}
				}
				if !found {
					idx.subnets = append(idx.subnets, dhcpSubnetEntry{
						Network: crd.Name,
						Subnet:  subnet,
					})
				}
			}
		}
		for _, r := range crd.Spec.DHCP.Reservations {
			if r.IP != "" && r.Hostname != "" {
				idx.reservationsByIP[r.IP] = dhcpReservationEntry{
					Network:  crd.Name,
					Hostname: r.Hostname,
				}
			}
		}
	}

	p.dhcpIndex = idx
	p.deps.Logger.Debugw("rebuilt DHCP index",
		"subnets", len(idx.subnets),
		"reservations", len(idx.reservationsByIP),
	)
}

// RunDHCPWatcher subscribes to NATS lease events for instant discovery and
// polls each DHCP-enabled microdns as a fallback consistency check.
func (p *MicroKubeProvider) RunDHCPWatcher(ctx context.Context) {
	// Rebuild index from CRDs (covers networks loaded from NATS)
	p.rebuildDHCPIndex()

	// Check if any network has DHCP enabled (static config or CRDs)
	hasAny := false
	for _, n := range p.deps.Config.Networks {
		if n.DNS.DHCP.Enabled {
			hasAny = true
			break
		}
	}
	if !hasAny {
		for _, n := range p.networks {
			if n.Spec.DHCP.Enabled {
				hasAny = true
				break
			}
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

		p.publishDHCPEvent(evt.Type, network, evt.IPAddr, evt.MACAddr, evt.Hostname)
	})
	if err != nil {
		log.Warnw("failed to subscribe to DHCP lease events", "error", err)
		return
	}

	log.Info("DHCP NATS subscription started on microdns.*.leases")
}

// publishDHCPEvent resolves the network and publishes a typed event on
// mkube.dhcp.{network}.lease for the bmh-operator (and any other consumer).
func (p *MicroKubeProvider) publishDHCPEvent(eventType, network, ip, mac, hostname string) {
	log := p.deps.Logger.Named("dhcp-watcher")

	if ip == "" || mac == "" {
		return
	}

	if p.dhcpIndex == nil {
		return
	}

	// Resolve hostname from reservation if available
	if entry, ok := p.dhcpIndex.reservationsByIP[ip]; ok {
		if network == "" {
			network = entry.Network
		}
		if hostname == "" {
			hostname = entry.Hostname
		}
	} else if network == "" {
		network = p.dhcpIndex.networkForIP(ip)
	}

	if network == "" {
		return
	}

	// Look up network type
	networkType := ""
	if net, ok := p.networks[network]; ok {
		networkType = string(net.Spec.Type)
	}

	evt := mkubeDHCPEvent{
		Type:        eventType,
		Network:     network,
		NetworkType: networkType,
		IPAddr:      ip,
		MACAddr:     mac,
		Hostname:    hostname,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(evt)
	if err != nil {
		log.Warnw("failed to marshal DHCP event", "error", err)
		return
	}

	subject := "mkube.dhcp." + network + ".lease"
	if p.deps.Store != nil {
		if err := p.deps.Store.Publish(subject, data); err != nil {
			log.Warnw("failed to publish DHCP event", "subject", subject, "error", err)
		} else {
			log.Infow("published DHCP event",
				"subject", subject,
				"network", network,
				"type", networkType,
				"ip", ip,
				"mac", mac,
				"hostname", hostname,
			)
		}
	}
}

// reconcileDHCPLeases polls every DHCP-enabled microdns instance for leases
// and publishes events for any discovered leases. This serves as a fallback
// consistency check for the event-driven NATS subscription.
func (p *MicroKubeProvider) reconcileDHCPLeases(ctx context.Context) {
	log := p.deps.Logger.Named("dhcp-watcher")

	// Poll static config networks
	for _, n := range p.deps.Config.Networks {
		if !n.DNS.DHCP.Enabled || n.DNS.Endpoint == "" {
			continue
		}
		p.pollDHCPLeases(ctx, log, n.Name, n.DNS.Endpoint)
	}

	// Poll Network CRD networks
	for _, n := range p.networks {
		if !n.Spec.DHCP.Enabled {
			continue
		}
		endpoint := p.networkDNSEndpoint(n)
		if endpoint == "" {
			continue
		}
		// Skip if already polled via static config
		alreadyPolled := false
		for _, sc := range p.deps.Config.Networks {
			if sc.Name == n.Name {
				alreadyPolled = true
				break
			}
		}
		if !alreadyPolled {
			p.pollDHCPLeases(ctx, log, n.Name, endpoint)
		}
	}
}

// pollDHCPLeases fetches active leases from a microdns instance and publishes events.
func (p *MicroKubeProvider) pollDHCPLeases(ctx context.Context, log *zap.SugaredLogger, networkName, endpoint string) {
	url := endpoint + "/api/v1/leases"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		log.Warnw("failed to create lease request", "network", networkName, "error", err)
		return
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		MaxConnsPerHost:   1,
		DisableKeepAlives: true,
	}}
	resp, err := client.Do(req)
	if err != nil {
		log.Debugw("DHCP lease fetch failed", "network", networkName, "error", err)
		return
	}

	var leases []dhcpLease
	if err := json.NewDecoder(resp.Body).Decode(&leases); err != nil {
		resp.Body.Close()
		log.Warnw("failed to decode leases", "network", networkName, "error", err)
		return
	}
	resp.Body.Close()

	for _, lease := range leases {
		p.publishDHCPEvent("LeaseCreated", networkName, lease.getIP(), lease.getMAC(), "")
	}
}
