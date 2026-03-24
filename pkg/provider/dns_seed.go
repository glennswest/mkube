package provider

import (
	"context"
	"fmt"
	"sort"
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

	log.Debugw("microdns healthy, seeding DNS config", "endpoint", endpoint)

	// 1. Create DHCP pool from network's DHCP spec (if local — no serverNetwork)
	if net.Spec.DHCP.Enabled && net.Spec.DHCP.ServerNetwork == "" {
		p.seedDHCPPool(ctx, dnsClient, endpoint, net, net)
	}

	// 2. Relay topology: create pools for peer networks that relay to this one
	for _, peer := range p.networks.Values() {
		if peer.Name == net.Name {
			continue
		}
		if peer.Spec.DHCP.Enabled && peer.Spec.DHCP.ServerNetwork == net.Name {
			p.seedDHCPPool(ctx, dnsClient, endpoint, peer, net)
		}
	}

	// 3. Upsert all DHCP reservations (local + relayed networks)
	p.seedDHCPReservations(ctx, dnsClient, endpoint, net)

	// 4. Create DNS A records from DHCP reservation hostnames.
	// BMCs and statically-addressed hosts don't DHCP, so their hostnames
	// are never auto-registered via lease. We create A records explicitly.
	p.seedReservationDNSRecords(ctx, dnsClient, endpoint, net)

	// 5. Create DNS forward zones for all peer networks
	p.seedDNSForwarders(ctx, dnsClient, endpoint, net)

	log.Debugw("DNS config seeded successfully", "endpoint", endpoint)

	// Run smoke test inline — seedDNSConfig is already called from a goroutine.
	// Using a background context so seed timeout doesn't kill the smoke test.
	smokeCtx, smokeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer smokeCancel()
	p.runDNSSmokeTest(smokeCtx, net)
}

// seedDHCPPool creates or updates a DHCP pool for a source network on the target microdns instance.
// If a pool with matching subnet already exists, updates it to ensure dns_servers and domain_search are set.
func (p *MicroKubeProvider) seedDHCPPool(ctx context.Context, client *dns.Client, endpoint string, source, target *Network) {
	log := p.deps.Logger

	leaseTime := source.Spec.DHCP.LeaseTime
	if leaseTime == 0 {
		leaseTime = 3600
	}

	// Build domain_search with ALL zones — local zone first, then peers.
	// This ensures clients can resolve hostnames across all networks without
	// scoped DNS issues (option 119 instead of option 15).
	domainSearch := []string{source.Spec.DNS.Zone}
	for _, net := range p.networks.Values() {
		if net.Spec.DNS.Zone != "" && net.Spec.DNS.Zone != source.Spec.DNS.Zone {
			domainSearch = append(domainSearch, net.Spec.DNS.Zone)
		}
	}
	sort.Strings(domainSearch[1:]) // keep local zone first, sort the rest

	// NTP servers: explicit config, or default to gateway (router typically serves NTP)
	ntpServers := source.Spec.DHCP.NTPServers
	if len(ntpServers) == 0 && source.Spec.Gateway != "" {
		ntpServers = []string{source.Spec.Gateway}
	}

	pool := dns.DHCPPool{
		Name:          source.Name + "-pool",
		RangeStart:    source.Spec.DHCP.RangeStart,
		RangeEnd:      source.Spec.DHCP.RangeEnd,
		Subnet:        source.Spec.CIDR,
		Gateway:       source.Spec.Gateway,
		DNSServers:    []string{target.Spec.DNS.Server},
		Domain:        source.Spec.DNS.Zone,
		DomainSearch:  domainSearch,
		LeaseTimeSecs: leaseTime,
		NextServer:    source.Spec.DHCP.NextServer,
		BootFile:      source.Spec.DHCP.BootFile,
		BootFileEFI:   source.Spec.DHCP.BootFileEFI,
		NTPServers:    ntpServers,
	}

	// For data networks: set default iSCSI root_path to baremetalservices.
	if source.Spec.Type == NetworkTypeData {
		cdrom, ok := p.iscsiCdroms.Get("baremetalservices")
		if ok && cdrom.Status.TargetIQN != "" {
			pool.RootPath = fmt.Sprintf("iscsi:%s::::%s", source.Spec.Gateway, cdrom.Status.TargetIQN)
			log.Infow("pool default root_path set", "network", source.Name, "root_path", pool.RootPath)
		}
	}

	// Check if pool already exists — update if so, create if not
	existing, err := client.ListDHCPPools(ctx, endpoint)
	if err != nil {
		log.Warnw("failed to list DHCP pools", "endpoint", endpoint, "error", err)
		return
	}
	for _, ep := range existing {
		if ep.Subnet == source.Spec.CIDR {
			pool.ID = ep.ID
			if _, err := client.UpdateDHCPPool(ctx, endpoint, ep.ID, pool); err != nil {
				log.Warnw("failed to update DHCP pool", "network", source.Name, "id", ep.ID, "error", err)
			} else {
				log.Infow("DHCP pool updated", "network", source.Name, "id", ep.ID, "dns", pool.DNSServers, "search", pool.DomainSearch)
			}
			return
		}
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
	for _, peer := range p.networks.Values() {
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
		Gateway:     r.Gateway,
		DNSServers:  r.DNSServers,
		Domain:      r.Domain,
		NextServer:  r.NextServer,
		BootFile:    r.BootFile,
		BootFileEFI: r.BootFileEFI,
		RootPath:    r.RootPath,
	}
}

// seedReservationDNSRecords creates DNS A records for all DHCP reservations
// that have a hostname. This ensures BMC addresses and statically-assigned hosts
// are resolvable via DNS, even though they never issue DHCP requests.
func (p *MicroKubeProvider) seedReservationDNSRecords(ctx context.Context, client *dns.Client, endpoint string, net *Network) {
	log := p.deps.Logger

	// Collect all reservations: local + relayed networks
	type resEntry struct {
		hostname string
		ip       string
		zone     string
	}
	var entries []resEntry

	for _, r := range net.Spec.DHCP.Reservations {
		if r.Hostname != "" && r.IP != "" {
			entries = append(entries, resEntry{r.Hostname, r.IP, net.Spec.DNS.Zone})
		}
	}

	for _, peer := range p.networks.Values() {
		if peer.Name == net.Name || !peer.Spec.DHCP.Enabled || peer.Spec.DHCP.ServerNetwork != net.Name {
			continue
		}
		for _, r := range peer.Spec.DHCP.Reservations {
			if r.Hostname != "" && r.IP != "" {
				entries = append(entries, resEntry{r.Hostname, r.IP, peer.Spec.DNS.Zone})
			}
		}
	}

	if len(entries) == 0 {
		return
	}

	// Resolve zone IDs via EnsureZone (creates zone if missing)
	zoneIDs := make(map[string]string)
	for _, e := range entries {
		if _, ok := zoneIDs[e.zone]; ok || e.zone == "" {
			continue
		}
		zoneID, err := client.EnsureZone(ctx, endpoint, e.zone)
		if err != nil {
			log.Warnw("failed to ensure zone for reservation DNS",
				"zone", e.zone, "error", err)
			continue
		}
		zoneIDs[e.zone] = zoneID
	}

	for _, e := range entries {
		zoneID, ok := zoneIDs[e.zone]
		if !ok {
			continue
		}
		// RegisterHost is idempotent — no-op if record already exists
		if err := client.RegisterHost(ctx, endpoint, zoneID, e.hostname, e.ip, 300); err != nil {
			log.Warnw("failed to create DNS A record from reservation",
				"hostname", e.hostname, "ip", e.ip, "zone", e.zone, "error", err)
		}
	}

	log.Debugw("seeded DNS A records from DHCP reservations",
		"endpoint", endpoint, "count", len(entries))
}

// seedDNSForwarders creates DNS forward zones for all peer networks.
func (p *MicroKubeProvider) seedDNSForwarders(ctx context.Context, client *dns.Client, endpoint string, net *Network) {
	log := p.deps.Logger

	for _, peer := range p.networks.Values() {
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
	rdcNetsSnap := p.networks.Values()
	for _, net := range rdcNetsSnap {
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

		// Check if DHCP pools need re-seed: empty pools, domain_search stale, or NTP missing
		needsSeed := false
		if net.Spec.DHCP.Enabled || p.networkHasDHCPFromSnapshot(net.Name, rdcNetsSnap) {
			pools, err := dnsClient.ListDHCPPools(ctx, endpoint)
			if err == nil && len(pools) == 0 {
				p.deps.Logger.Debugw("microdns has empty DHCP pools, re-seeding",
					"network", net.Name, "endpoint", endpoint)
				needsSeed = true
			}
			if !needsSeed && err == nil && len(pools) > 0 {
				// Check if domain_search is stale (fewer zones than expected)
				zoneCount := 0
				for _, n := range rdcNetsSnap {
					if n.Spec.DNS.Zone != "" {
						zoneCount++
					}
				}
				if len(pools[0].DomainSearch) < zoneCount {
					p.deps.Logger.Debugw("domain_search stale, re-seeding",
						"network", net.Name, "have", len(pools[0].DomainSearch), "want", zoneCount)
					needsSeed = true
				}
				// Check if NTP servers are missing (should have at least gateway)
				if !needsSeed && len(pools[0].NTPServers) == 0 {
					p.deps.Logger.Debugw("NTP servers missing from pool, re-seeding",
						"network", net.Name)
					needsSeed = true
				}
			}
		}

		// Check if reservation DNS A records are missing — re-seed if so.
		// Pools may exist (survived in redb) but A records may be lost if
		// the database path was wrong and records were in ephemeral storage.
		if !needsSeed {
			needsSeed = p.reservationDNSRecordsMissing(ctx, dnsClient, endpoint, net)
		}

		if needsSeed {
			// Guard with reseedRunning to prevent unbounded goroutine growth
			// when multiple networks need re-seeding concurrently.
			if p.reseedRunning.CompareAndSwap(false, true) {
				netCopy := net
				go func() {
					defer p.reseedRunning.Store(false)
					ctx2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					p.seedDNSConfig(ctx2, netCopy)
				}()
			}
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

		for _, peer := range p.networks.Values() {
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

// reservationDNSRecordsMissing checks if any DHCP reservations with hostnames
// are missing corresponding DNS A records. Returns true if re-seed is needed.
func (p *MicroKubeProvider) reservationDNSRecordsMissing(ctx context.Context, client *dns.Client, endpoint string, net *Network) bool {
	// Collect all reservations with hostnames (local + relayed)
	var expectedHostnames []string
	for _, r := range net.Spec.DHCP.Reservations {
		if r.Hostname != "" && r.IP != "" {
			expectedHostnames = append(expectedHostnames, r.Hostname)
		}
	}
	for _, peer := range p.networks.Values() {
		if peer.Name == net.Name || !peer.Spec.DHCP.Enabled || peer.Spec.DHCP.ServerNetwork != net.Name {
			continue
		}
		for _, r := range peer.Spec.DHCP.Reservations {
			if r.Hostname != "" && r.IP != "" {
				expectedHostnames = append(expectedHostnames, r.Hostname)
			}
		}
	}

	if len(expectedHostnames) == 0 {
		return false
	}

	// Get the zone ID
	zoneID, err := client.EnsureZone(ctx, endpoint, net.Spec.DNS.Zone)
	if err != nil || zoneID == "" {
		return false
	}

	// List existing records in the zone
	records, err := client.ListRecords(ctx, endpoint, zoneID)
	if err != nil {
		return false
	}

	existing := make(map[string]bool, len(records))
	for _, r := range records {
		existing[r.Name] = true
	}

	// If any reservation hostname is missing from DNS, needs re-seed
	for _, h := range expectedHostnames {
		if !existing[h] {
			p.deps.Logger.Debugw("reservation DNS A record missing, triggering re-seed",
				"network", net.Name, "hostname", h, "endpoint", endpoint)
			return true
		}
	}

	return false
}
