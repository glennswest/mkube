package provider

import (
	"context"
	"net"
	"time"
)

// RunSubnetScanner periodically probes DHCP reservation IPs for reachable
// IPMI cards. It complements RunDHCPWatcher: the watcher catches new DHCP
// events, while the scanner discovers devices that already have static IPs
// and never send DHCP requests.
func (p *MicroKubeProvider) RunSubnetScanner(ctx context.Context) {
	hasAny := false
	for _, n := range p.deps.Config.Networks {
		if n.DNS.DHCP.Enabled && len(n.DNS.DHCP.Reservations) > 0 {
			hasAny = true
			break
		}
	}
	if !hasAny {
		p.deps.Logger.Info("subnet scanner disabled (no networks with DHCP reservations)")
		return
	}

	log := p.deps.Logger.Named("subnet-scanner")
	interval := 5 * time.Minute
	log.Infow("subnet scanner starting", "interval", interval)

	// Run once immediately, then on ticker
	p.scanSubnets(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("subnet scanner stopping")
			return
		case <-ticker.C:
			p.scanSubnets(ctx)
		}
	}
}

// scanSubnets iterates all DHCP-enabled networks and probes each reservation IP.
func (p *MicroKubeProvider) scanSubnets(ctx context.Context) {
	log := p.deps.Logger.Named("subnet-scanner")

	for _, n := range p.deps.Config.Networks {
		if !n.DNS.DHCP.Enabled {
			continue
		}

		for _, r := range n.DNS.DHCP.Reservations {
			if r.IP == "" || r.Hostname == "" {
				continue
			}

			// Skip if BMH already exists for this host
			key := n.Name + "/" + r.Hostname
			if _, exists := p.bareMetalHosts[key]; exists {
				continue
			}

			if !probeIPMI(ctx, r.IP) {
				continue
			}

			log.Infow("subnet scan found reachable host",
				"hostname", r.Hostname,
				"network", n.Name,
				"ip", r.IP,
			)

			// Reuse the DHCP lease processor. MAC is empty — it will
			// be filled when the device eventually DHCPs or by the
			// BMH reconciler.
			p.processDHCPLease(ctx, r.IP, r.MAC, r.Hostname)
		}
	}
}

// probeIPMI checks if a BMC/iDRAC is reachable by trying its HTTPS
// management interface (TCP 443), then falling back to an RMCP ping
// on UDP 623. Most BMCs expose a web UI on 443; RMCP is UDP-only so
// a TCP probe on 623 won't work.
func probeIPMI(ctx context.Context, ip string) bool {
	d := net.Dialer{Timeout: 2 * time.Second}

	// Try HTTPS management port first (works for iDRAC, Supermicro, etc.)
	if conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443")); err == nil {
		conn.Close()
		return true
	}

	// Try HTTP management port
	if conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "80")); err == nil {
		conn.Close()
		return true
	}

	// Fall back to RMCP ping (UDP 623). Send an ASF Presence Ping and
	// check for any response.
	udpConn, err := d.DialContext(ctx, "udp", net.JoinHostPort(ip, "623"))
	if err != nil {
		return false
	}
	defer udpConn.Close()

	// ASF RMCP Presence Ping
	rmcpPing := []byte{0x06, 0x00, 0xff, 0x06, 0x00, 0x00, 0x11, 0xbe, 0x80, 0x00, 0x00, 0x00}
	_ = udpConn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := udpConn.Write(rmcpPing); err != nil {
		return false
	}

	buf := make([]byte, 64)
	n, err := udpConn.Read(buf)
	return err == nil && n > 0
}
