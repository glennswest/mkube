package provider

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/glennswest/mkube/pkg/dns"
)

// SmokeTestResult records the outcome of a microdns smoke test.
type SmokeTestResult struct {
	Pass      bool      `json:"pass"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Network   string    `json:"network"`
}

// smokeTestResults stores the latest smoke test result per network.
var smokeTestResults sync.Map // network name -> SmokeTestResult

// runDNSSmokeTest validates that a microdns instance is fully functional after
// seed. It checks DHCP pools/reservations via REST, then creates a canary DNS
// record and verifies it resolves over port 53. Runs as a goroutine ~10s after
// seedDNSConfig completes.
func (p *MicroKubeProvider) runDNSSmokeTest(ctx context.Context, net *Network) {
	log := p.deps.Logger.With("network", net.Name, "func", "smokeTest")

	// Wait for microdns to fully initialize (redb load, DHCP listener start)
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}

	endpoint := net.Spec.DNS.Endpoint
	if endpoint == "" {
		endpoint = "http://" + net.Spec.DNS.Server + ":8080"
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		storeSmokeResult(net.Name, false, "no DNS client configured")
		return
	}

	// 1. REST API health check
	if err := dnsClient.HealthCheck(ctx, endpoint); err != nil {
		msg := fmt.Sprintf("REST API not healthy: %v", err)
		log.Warnw("smoketest failed", "step", "health", "error", err)
		storeSmokeResult(net.Name, false, msg)
		return
	}

	// 2. DHCP pool check (if DHCP is enabled for this network or relayed to it)
	stNetsSnap := p.networks.Values()
	if net.Spec.DHCP.Enabled || p.networkHasDHCPFromSnapshot(net.Name, stNetsSnap) {
		pools, err := dnsClient.ListDHCPPools(ctx, endpoint)
		if err != nil {
			msg := fmt.Sprintf("cannot list DHCP pools: %v", err)
			log.Warnw("smoketest failed", "step", "dhcp-pools", "error", err)
			storeSmokeResult(net.Name, false, msg)
			return
		}
		if len(pools) == 0 {
			log.Warnw("smoketest failed", "step", "dhcp-pools", "reason", "no pools")
			storeSmokeResult(net.Name, false, "no DHCP pools configured")
			return
		}

		// Check reservations
		reservations, err := dnsClient.ListDHCPReservations(ctx, endpoint)
		if err != nil {
			log.Warnw("smoketest: cannot check reservations", "error", err)
			// Non-fatal — some networks have no reservations
		} else {
			log.Infow("smoketest: DHCP check passed",
				"pools", len(pools), "reservations", len(reservations))
		}
	}

	// 3. DNS write check — create canary A record
	zone := net.Spec.DNS.Zone
	zoneID, err := dnsClient.EnsureZone(ctx, endpoint, zone)
	if err != nil {
		msg := fmt.Sprintf("cannot get zone %s: %v", zone, err)
		log.Warnw("smoketest failed", "step", "zone-lookup", "error", err)
		storeSmokeResult(net.Name, false, msg)
		return
	}

	canaryName := "_smoketest"
	canaryIP := "127.0.0.99"

	if err := dnsClient.RegisterHost(ctx, endpoint, zoneID, canaryName, canaryIP, 30); err != nil {
		msg := fmt.Sprintf("cannot create canary record: %v", err)
		log.Warnw("smoketest failed", "step", "dns-write", "error", err)
		storeSmokeResult(net.Name, false, msg)
		return
	}

	// 4. DNS resolve check — query port 53 for the canary
	fqdn := canaryName + "." + zone
	resolved := probeDNSRecord(net.Spec.DNS.Server, fqdn, canaryIP, 5*time.Second)

	// 5. Cleanup — always delete the canary record
	cleanupCanary(ctx, dnsClient, endpoint, zoneID, canaryName)

	if !resolved {
		msg := fmt.Sprintf("canary %s did not resolve to %s on port 53", fqdn, canaryIP)
		log.Warnw("smoketest failed", "step", "dns-resolve", "fqdn", fqdn)
		storeSmokeResult(net.Name, false, msg)
		return
	}

	log.Infow("smoketest passed", "zone", zone)
	storeSmokeResult(net.Name, true, "all checks passed")
}

// runSmokeTestSync runs a smoke test synchronously and returns the result.
// Used by the on-demand API endpoint.
func (p *MicroKubeProvider) runSmokeTestSync(ctx context.Context, net *Network) SmokeTestResult {
	log := p.deps.Logger.With("network", net.Name, "func", "smokeTestSync")

	endpoint := net.Spec.DNS.Endpoint
	if endpoint == "" {
		endpoint = "http://" + net.Spec.DNS.Server + ":8080"
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		return SmokeTestResult{Pass: false, Message: "no DNS client configured", Timestamp: time.Now(), Network: net.Name}
	}

	// REST API health
	if err := dnsClient.HealthCheck(ctx, endpoint); err != nil {
		return SmokeTestResult{Pass: false, Message: fmt.Sprintf("REST API not healthy: %v", err), Timestamp: time.Now(), Network: net.Name}
	}

	// DHCP pools
	if net.Spec.DHCP.Enabled || p.networkHasDHCP(net.Name) {
		pools, err := dnsClient.ListDHCPPools(ctx, endpoint)
		if err != nil {
			return SmokeTestResult{Pass: false, Message: fmt.Sprintf("cannot list DHCP pools: %v", err), Timestamp: time.Now(), Network: net.Name}
		}
		if len(pools) == 0 {
			return SmokeTestResult{Pass: false, Message: "no DHCP pools configured", Timestamp: time.Now(), Network: net.Name}
		}
	}

	// DNS write + resolve
	zone := net.Spec.DNS.Zone
	zoneID, err := dnsClient.EnsureZone(ctx, endpoint, zone)
	if err != nil {
		return SmokeTestResult{Pass: false, Message: fmt.Sprintf("cannot get zone: %v", err), Timestamp: time.Now(), Network: net.Name}
	}

	canaryName := "_smoketest"
	canaryIP := "127.0.0.99"

	if err := dnsClient.RegisterHost(ctx, endpoint, zoneID, canaryName, canaryIP, 30); err != nil {
		return SmokeTestResult{Pass: false, Message: fmt.Sprintf("cannot create canary: %v", err), Timestamp: time.Now(), Network: net.Name}
	}

	fqdn := canaryName + "." + zone
	resolved := probeDNSRecord(net.Spec.DNS.Server, fqdn, canaryIP, 5*time.Second)
	cleanupCanary(ctx, dnsClient, endpoint, zoneID, canaryName)

	if !resolved {
		result := SmokeTestResult{Pass: false, Message: fmt.Sprintf("canary %s did not resolve to %s", fqdn, canaryIP), Timestamp: time.Now(), Network: net.Name}
		smokeTestResults.Store(net.Name, result)
		return result
	}

	log.Infow("on-demand smoketest passed", "zone", zone)
	result := SmokeTestResult{Pass: true, Message: "all checks passed", Timestamp: time.Now(), Network: net.Name}
	smokeTestResults.Store(net.Name, result)
	return result
}

// cleanupCanary removes the _smoketest canary record from the zone.
func cleanupCanary(ctx context.Context, dnsClient *dns.Client, endpoint, zoneID, canaryName string) {
	records, err := dnsClient.ListRecords(ctx, endpoint, zoneID)
	if err != nil {
		return
	}
	for _, r := range records {
		if r.Name == canaryName && r.Type == "A" {
			_ = dnsClient.DeleteRecord(ctx, endpoint, zoneID, r.ID)
		}
	}
}

func storeSmokeResult(network string, pass bool, message string) {
	smokeTestResults.Store(network, SmokeTestResult{
		Pass:      pass,
		Message:   message,
		Timestamp: time.Now(),
		Network:   network,
	})
}

// GetSmokeTestResults returns all stored smoke test results.
func GetSmokeTestResults() []SmokeTestResult {
	var results []SmokeTestResult
	smokeTestResults.Range(func(key, value interface{}) bool {
		if r, ok := value.(SmokeTestResult); ok {
			results = append(results, r)
		}
		return true
	})
	return results
}

// probeDNSRecord sends a DNS A query for fqdn to serverIP:53 and checks
// that the answer section contains an A record matching expectedIP.
func probeDNSRecord(serverIP, fqdn, expectedIP string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("udp", serverIP+":53", timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	query := buildDNSAQuery(fqdn)
	if _, err := conn.Write(query); err != nil {
		return false
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n < 12 {
		return false
	}

	return parseDNSAAnswer(buf[:n], expectedIP)
}

// buildDNSAQuery constructs a DNS query packet for an A record.
func buildDNSAQuery(fqdn string) []byte {
	// DNS Header: ID=0xABCD, QR=0, OPCODE=0, RD=1, QDCOUNT=1
	header := []byte{
		0xAB, 0xCD, // ID
		0x01, 0x00, // Flags: RD=1
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, // ANCOUNT=0
		0x00, 0x00, // NSCOUNT=0
		0x00, 0x00, // ARCOUNT=0
	}

	// Encode FQDN as DNS labels
	var question []byte
	name := strings.TrimSuffix(fqdn, ".")
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 {
			continue
		}
		question = append(question, byte(len(label)))
		question = append(question, []byte(label)...)
	}
	question = append(question, 0x00)       // root label
	question = append(question, 0x00, 0x01) // QTYPE=A
	question = append(question, 0x00, 0x01) // QCLASS=IN

	return append(header, question...)
}

// parseDNSAAnswer parses a DNS response and checks if any A record in the
// answer section matches expectedIP.
func parseDNSAAnswer(buf []byte, expectedIP string) bool {
	if len(buf) < 12 {
		return false
	}

	// Check response code (RCODE in lower 4 bits of byte 3)
	rcode := buf[3] & 0x0F
	if rcode != 0 {
		return false // NXDOMAIN, SERVFAIL, etc.
	}

	qdcount := binary.BigEndian.Uint16(buf[4:6])
	ancount := binary.BigEndian.Uint16(buf[6:8])
	if ancount == 0 {
		return false
	}

	// Skip header (12 bytes) and question section
	offset := 12
	for i := 0; i < int(qdcount); i++ {
		offset = skipDNSName(buf, offset)
		if offset < 0 || offset+4 > len(buf) {
			return false
		}
		offset += 4 // QTYPE + QCLASS
	}

	// Parse answer section
	expected := net.ParseIP(expectedIP).To4()
	if expected == nil {
		return false
	}

	for i := 0; i < int(ancount); i++ {
		offset = skipDNSName(buf, offset)
		if offset < 0 || offset+10 > len(buf) {
			return false
		}

		rtype := binary.BigEndian.Uint16(buf[offset : offset+2])
		// rclass := binary.BigEndian.Uint16(buf[offset+2 : offset+4])
		// ttl := binary.BigEndian.Uint32(buf[offset+4 : offset+8])
		rdlength := binary.BigEndian.Uint16(buf[offset+8 : offset+10])
		offset += 10

		if offset+int(rdlength) > len(buf) {
			return false
		}

		// A record: type 1, rdlength 4
		if rtype == 1 && rdlength == 4 {
			if buf[offset] == expected[0] &&
				buf[offset+1] == expected[1] &&
				buf[offset+2] == expected[2] &&
				buf[offset+3] == expected[3] {
				return true
			}
		}

		offset += int(rdlength)
	}

	return false
}

// skipDNSName skips a DNS name (handling compression pointers) and returns
// the offset after the name, or -1 on error.
func skipDNSName(buf []byte, offset int) int {
	if offset >= len(buf) {
		return -1
	}
	for {
		if offset >= len(buf) {
			return -1
		}
		length := int(buf[offset])
		if length == 0 {
			offset++
			break
		}
		// Compression pointer (top 2 bits set)
		if length&0xC0 == 0xC0 {
			offset += 2
			break // pointer — name ends here
		}
		offset += 1 + length
		if offset > len(buf) {
			return -1
		}
	}
	return offset
}
