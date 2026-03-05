package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/glennswest/mkube/pkg/dns"
)

// seedDNSConfig populates a microdns instance with DHCP pools, reservations,
// and DNS forward zones derived from the Network CRD state. This replaces the
// old TOML-based config pipeline. The function retries until the microdns REST
// API becomes healthy (up to 30s), handling the chicken-and-egg of pod startup.
func (p *MicroKubeProvider) seedDNSConfig(ctx context.Context, net *Network) {
	log := p.deps.Logger.With("network", net.Name, "func", "seedDNSConfig")

	endpoint := net.Spec.DNS.Endpoint
	if endpoint == "" {
		endpoint = "http://" + net.Spec.DNS.Server + ":8080"
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()

	// Wait for microdns to become healthy (up to 30s)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			log.Warnw("microdns health timeout, skipping seed", "endpoint", endpoint)
			return
		}
		if err := dnsClient.HealthCheck(ctx, endpoint); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	log.Infow("microdns healthy, seeding DNS config", "endpoint", endpoint)

	// 1. Create DHCP pool from network's DHCP spec (if local — no serverNetwork)
	if net.Spec.DHCP.Enabled && net.Spec.DHCP.ServerNetwork == "" {
		p.seedDHCPPool(ctx, dnsClient, endpoint, net, net)
	}

	// 2. Relay topology: create pools for peer networks that relay to this one
	for _, peer := range p.networks {
		if peer.Name == net.Name {
			continue
		}
		if peer.Spec.DHCP.Enabled && peer.Spec.DHCP.ServerNetwork == net.Name {
			p.seedDHCPPool(ctx, dnsClient, endpoint, peer, net)
		}
	}

	// 3. Upsert all DHCP reservations (local + relayed networks)
	p.seedDHCPReservations(ctx, dnsClient, endpoint, net)

	// 4. Create DNS forward zones for all peer networks
	p.seedDNSForwarders(ctx, dnsClient, endpoint, net)

	log.Infow("DNS config seeded successfully", "endpoint", endpoint)
}

// seedDHCPPool creates a DHCP pool for a source network on the target microdns instance.
// Skips if a pool with matching subnet already exists.
func (p *MicroKubeProvider) seedDHCPPool(ctx context.Context, client *dns.Client, endpoint string, source, target *Network) {
	log := p.deps.Logger

	// Check if pool already exists
	existing, err := client.ListDHCPPools(ctx, endpoint)
	if err != nil {
		log.Warnw("failed to list DHCP pools", "endpoint", endpoint, "error", err)
		return
	}
	for _, pool := range existing {
		if pool.Subnet == source.Spec.CIDR {
			log.Debugw("DHCP pool already exists", "subnet", source.Spec.CIDR, "endpoint", endpoint)
			return
		}
	}

	leaseTime := source.Spec.DHCP.LeaseTime
	if leaseTime == 0 {
		leaseTime = 3600
	}

	pool := dns.DHCPPool{
		Name:          source.Name + "-pool",
		RangeStart:    source.Spec.DHCP.RangeStart,
		RangeEnd:      source.Spec.DHCP.RangeEnd,
		Subnet:        source.Spec.CIDR,
		Gateway:       source.Spec.Gateway,
		DNSServers:    []string{target.Spec.DNS.Server},
		Domain:        source.Spec.DNS.Zone,
		LeaseTimeSecs: leaseTime,
		NextServer:    source.Spec.DHCP.NextServer,
		BootFile:      source.Spec.DHCP.BootFile,
		BootFileEFI:   source.Spec.DHCP.BootFileEFI,
	}

	// Auto-derive iPXE boot URL
	if source.Spec.DHCP.BootFile != "" && source.Spec.DHCP.NextServer != "" {
		pool.IPXEBootURL = fmt.Sprintf("http://%s:8080/boot.ipxe", source.Spec.DHCP.NextServer)
	}

	if _, err := client.CreateDHCPPool(ctx, endpoint, pool); err != nil {
		log.Warnw("failed to create DHCP pool", "network", source.Name, "endpoint", endpoint, "error", err)
	}
}

// seedDHCPReservations upserts all DHCP reservations from the Network CRD
// (and any relayed networks) into the microdns instance.
func (p *MicroKubeProvider) seedDHCPReservations(ctx context.Context, client *dns.Client, endpoint string, net *Network) {
	log := p.deps.Logger

	// Local reservations
	for _, r := range net.Spec.DHCP.Reservations {
		res := networkReservationToDNS(r)
		if err := client.UpsertDHCPReservation(ctx, endpoint, res); err != nil {
			log.Warnw("failed to upsert DHCP reservation", "mac", r.MAC, "endpoint", endpoint, "error", err)
		}
	}

	// Reservations from networks that relay to this one
	for _, peer := range p.networks {
		if peer.Name == net.Name || !peer.Spec.DHCP.Enabled || peer.Spec.DHCP.ServerNetwork != net.Name {
			continue
		}
		for _, r := range peer.Spec.DHCP.Reservations {
			res := networkReservationToDNS(r)
			if err := client.UpsertDHCPReservation(ctx, endpoint, res); err != nil {
				log.Warnw("failed to upsert relayed DHCP reservation",
					"mac", r.MAC, "sourceNetwork", peer.Name, "endpoint", endpoint, "error", err)
			}
		}
	}
}

// networkReservationToDNS converts a Network CRD reservation to a DNS client reservation.
func networkReservationToDNS(r NetworkDHCPReservation) dns.DHCPReservation {
	return dns.DHCPReservation{
		MAC:         r.MAC,
		IP:          r.IP,
		Hostname:    r.Hostname,
		NextServer:  r.NextServer,
		BootFile:    r.BootFile,
		BootFileEFI: r.BootFileEFI,
	}
}

// seedDNSForwarders creates DNS forward zones for all peer networks.
func (p *MicroKubeProvider) seedDNSForwarders(ctx context.Context, client *dns.Client, endpoint string, net *Network) {
	log := p.deps.Logger

	for _, peer := range p.networks {
		if peer.Name == net.Name || peer.Spec.DNS.Zone == "" || peer.Spec.DNS.Server == "" {
			continue
		}
		fwd := dns.DNSForwarder{
			Zone:    peer.Spec.DNS.Zone,
			Servers: []string{peer.Spec.DNS.Server + ":53"},
		}
		if err := client.EnsureDNSForwarder(ctx, endpoint, fwd); err != nil {
			log.Warnw("failed to create DNS forwarder",
				"zone", peer.Spec.DNS.Zone, "endpoint", endpoint, "error", err)
		}
	}
}

// reconcileDNSConfig checks all managed networks with running DNS pods
// and re-seeds any that have empty DHCP pool databases (e.g. after microdns
// restart with a clean database). Also verifies forward zones match topology.
func (p *MicroKubeProvider) reconcileDNSConfig(ctx context.Context) {
	for _, net := range p.networks {
		if net.Spec.ExternalDNS || net.Spec.DNS.Zone == "" || net.Spec.DNS.Server == "" {
			continue
		}

		endpoint := net.Spec.DNS.Endpoint
		if endpoint == "" {
			endpoint = "http://" + net.Spec.DNS.Server + ":8080"
		}

		dnsClient := p.deps.NetworkMgr.DNSClient()

		// Quick health check — skip if microdns isn't up
		if err := dnsClient.HealthCheck(ctx, endpoint); err != nil {
			continue
		}

		// Check if DHCP pools exist — empty means needs re-seed
		needsSeed := false
		if net.Spec.DHCP.Enabled || p.networkHasDHCP(net.Name) {
			pools, err := dnsClient.ListDHCPPools(ctx, endpoint)
			if err == nil && len(pools) == 0 {
				p.deps.Logger.Infow("microdns has empty DHCP pools, re-seeding",
					"network", net.Name, "endpoint", endpoint)
				needsSeed = true
			}
		}

		if needsSeed {
			go p.seedDNSConfig(ctx, net)
			continue
		}

		// Verify forward zones match current topology
		forwarders, err := dnsClient.ListDNSForwarders(ctx, endpoint)
		if err != nil {
			continue
		}

		existingZones := make(map[string]bool, len(forwarders))
		for _, f := range forwarders {
			existingZones[f.Zone] = true
		}

		for _, peer := range p.networks {
			if peer.Name == net.Name || peer.Spec.DNS.Zone == "" || peer.Spec.DNS.Server == "" {
				continue
			}
			if !existingZones[peer.Spec.DNS.Zone] {
				fwd := dns.DNSForwarder{
					Zone:    peer.Spec.DNS.Zone,
					Servers: []string{peer.Spec.DNS.Server + ":53"},
				}
				if err := dnsClient.EnsureDNSForwarder(ctx, endpoint, fwd); err != nil {
					p.deps.Logger.Warnw("failed to add missing forwarder",
						"zone", peer.Spec.DNS.Zone, "endpoint", endpoint, "error", err)
				}
			}
		}
	}
}
