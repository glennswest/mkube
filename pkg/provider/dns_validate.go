package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// DNSValidationReport is the response for GET /api/v1/dns/validate.
type DNSValidationReport struct {
	Timestamp string       `json:"timestamp"`
	Summary   CheckSummary `json:"summary"`
	Checks    []CheckItem  `json:"checks"`
}

func (p *MicroKubeProvider) handleDNSValidate(w http.ResponseWriter, r *http.Request) {
	report := p.runDNSValidation(r.Context())
	podWriteJSON(w, http.StatusOK, report)
}

func (p *MicroKubeProvider) runDNSValidation(ctx context.Context) DNSValidationReport {
	report := DNSValidationReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		report.Checks = append(report.Checks, CheckItem{
			Name:    "dns-client",
			Status:  "fail",
			Message: "no DNS client configured",
		})
		report.Summary.Fail = 1
		return report
	}

	// Collect all zone endpoints for cross-zone forwarding check later.
	type zoneInfo struct {
		name     string
		zone     string
		server   string
		endpoint string
	}
	var zones []zoneInfo

	for _, netName := range p.deps.NetworkMgr.Networks() {
		netDef, ok := p.deps.NetworkMgr.NetworkDef(netName)
		if !ok || netDef.DNS.Endpoint == "" || netDef.DNS.Zone == "" {
			continue
		}

		zones = append(zones, zoneInfo{
			name:     netName,
			zone:     netDef.DNS.Zone,
			server:   netDef.DNS.Server,
			endpoint: netDef.DNS.Endpoint,
		})

		// 1. Zone reachability
		zoneID, ok := p.deps.NetworkMgr.NetworkZoneID(netName)
		if !ok {
			report.Checks = append(report.Checks, CheckItem{
				Name:    fmt.Sprintf("zone/%s/reachable", netName),
				Status:  "fail",
				Message: "zone ID not cached (DNS may not be initialized)",
			})
			continue
		}

		records, err := dnsClient.ListRecords(ctx, netDef.DNS.Endpoint, zoneID)
		if err != nil {
			report.Checks = append(report.Checks, CheckItem{
				Name:    fmt.Sprintf("zone/%s/reachable", netName),
				Status:  "fail",
				Message: fmt.Sprintf("cannot list records: %v", err),
			})
			continue
		}

		report.Checks = append(report.Checks, CheckItem{
			Name:    fmt.Sprintf("zone/%s/reachable", netName),
			Status:  "pass",
			Message: fmt.Sprintf("%d records", len(records)),
		})

		// Build actual record lookup: hostname -> []ip
		actualRecords := make(map[string][]string)
		for _, r := range records {
			if r.Type == "A" {
				actualRecords[r.Name] = append(actualRecords[r.Name], r.Data.Data)
			}
		}

		// 2. DHCP reservation records
		for _, res := range netDef.DNS.DHCP.Reservations {
			if res.Hostname == "" || res.IP == "" {
				continue
			}
			checkName := fmt.Sprintf("zone/%s/reservation/%s", netName, res.Hostname)
			ips, exists := actualRecords[res.Hostname]
			if !exists {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "missing",
					Details: fmt.Sprintf("expected A=%s", res.IP),
				})
				continue
			}
			if containsStr(ips, res.IP) {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: fmt.Sprintf("A=%s", res.IP),
				})
			} else {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("wrong IP: have %v, want %s", ips, res.IP),
				})
			}
		}

		// 3. Static records
		for _, sr := range netDef.DNS.StaticRecords {
			checkName := fmt.Sprintf("zone/%s/static/%s", netName, sr.Name)
			ips, exists := actualRecords[sr.Name]
			if !exists {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "missing",
					Details: fmt.Sprintf("expected A=%s", sr.IP),
				})
				continue
			}
			if containsStr(ips, sr.IP) {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: fmt.Sprintf("A=%s", sr.IP),
				})
			} else {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("wrong IP: have %v, want %s", ips, sr.IP),
				})
			}
		}

		// 4. Infrastructure records (rose1 -> gateway, dns -> DNS server)
		infraRecords := []struct{ name, ip string }{
			{"rose1", netDef.Gateway},
			{"dns", netDef.DNS.Server},
		}
		for _, infra := range infraRecords {
			if infra.ip == "" {
				continue
			}
			checkName := fmt.Sprintf("zone/%s/infra/%s", netName, infra.name)
			ips, exists := actualRecords[infra.name]
			if !exists {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "missing",
					Details: fmt.Sprintf("expected A=%s", infra.ip),
				})
				continue
			}
			if containsStr(ips, infra.ip) {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: fmt.Sprintf("A=%s", infra.ip),
				})
			} else {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("wrong IP: have %v, want %s", ips, infra.ip),
				})
			}
		}

		// 5. Pod alias records — iterate tracked pods, rebuild expected aliases
		pods := make([]*corev1.Pod, 0, len(p.pods))
		for _, pod := range p.pods {
			pods = append(pods, pod)
		}
		expectedRecords := p.buildExpectedDNSRecords(pods, netName)
		for hostname, expected := range expectedRecords {
			checkName := fmt.Sprintf("zone/%s/pod/%s", netName, hostname)
			ips, exists := actualRecords[hostname]
			if !exists {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "missing",
					Details: fmt.Sprintf("expected A=%s", expected.ip),
				})
				continue
			}
			if containsStr(ips, expected.ip) {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: fmt.Sprintf("A=%s", expected.ip),
				})
			} else {
				report.Checks = append(report.Checks, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("wrong IP: have %v, want %s", ips, expected.ip),
				})
			}
		}

		// 6. Duplicate detection — flag hostnames with multiple A records
		for hostname, ips := range actualRecords {
			if len(ips) <= 1 {
				continue
			}
			// Check for truly duplicate IPs (same hostname, same IP registered twice)
			seen := make(map[string]int)
			for _, ip := range ips {
				seen[ip]++
			}
			for ip, count := range seen {
				if count > 1 {
					report.Checks = append(report.Checks, CheckItem{
						Name:    fmt.Sprintf("zone/%s/duplicate/%s", netName, hostname),
						Status:  "warn",
						Message: fmt.Sprintf("duplicate A record: %s appears %d times", ip, count),
					})
				}
			}
		}
	}

	// 7. Cross-zone forwarding — query each zone's recursor for hostnames in other zones
	if len(zones) > 1 {
		for _, src := range zones {
			if src.server == "" {
				continue
			}
			for _, dst := range zones {
				if dst.name == src.name {
					continue
				}
				// Pick a known hostname to resolve: "rose1.<dst.zone>"
				fqdn := "rose1." + dst.zone + "."
				checkName := fmt.Sprintf("forward/%s->%s/rose1", src.name, dst.name)

				resolver := &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						d := net.Dialer{Timeout: 3 * time.Second}
						return d.DialContext(ctx, "udp", src.server+":53")
					},
				}

				lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				addrs, err := resolver.LookupHost(lookupCtx, fqdn)
				cancel()

				if err != nil {
					report.Checks = append(report.Checks, CheckItem{
						Name:    checkName,
						Status:  "fail",
						Message: fmt.Sprintf("lookup failed: %v", err),
					})
				} else if len(addrs) > 0 {
					report.Checks = append(report.Checks, CheckItem{
						Name:    checkName,
						Status:  "pass",
						Message: fmt.Sprintf("resolved %s", addrs[0]),
					})
				} else {
					report.Checks = append(report.Checks, CheckItem{
						Name:    checkName,
						Status:  "fail",
						Message: "no addresses returned",
					})
				}
			}
		}
	}

	// Compute summary
	for _, item := range report.Checks {
		switch item.Status {
		case "pass":
			report.Summary.Pass++
		case "fail":
			report.Summary.Fail++
		case "warn":
			report.Summary.Warn++
		}
	}

	return report
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
