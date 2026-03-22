package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/glennswest/mkube/pkg/network"
)

// ConsistencyReport is the top-level response for GET /api/v1/consistency.
type ConsistencyReport struct {
	Timestamp string            `json:"timestamp"`
	Summary   CheckSummary      `json:"summary"`
	Checks    ConsistencyChecks `json:"checks"`
}

// CheckSummary counts results by status.
type CheckSummary struct {
	Pass int `json:"pass"`
	Fail int `json:"fail"`
	Warn int `json:"warn"`
}

// ConsistencyChecks groups the check categories.
type ConsistencyChecks struct {
	Containers  []CheckItem `json:"containers"`
	DNS         []CheckItem `json:"dns"`
	Manifest    []CheckItem `json:"manifest"`
	IPAM        []CheckItem `json:"ipam"`
	Network     []CheckItem `json:"network,omitempty"`
	Deployments []CheckItem `json:"deployments,omitempty"`
	PVCs        []CheckItem `json:"pvcs,omitempty"`
	Networks    []CheckItem `json:"networks,omitempty"`
	BMHs        []CheckItem `json:"bmhs,omitempty"`
	Registries  []CheckItem `json:"registries,omitempty"`
	ISCSICdroms      []CheckItem `json:"iscsiCdroms,omitempty"`
	ISCSIDisks       []CheckItem `json:"iscsiDisks,omitempty"`
	BootConfigs      []CheckItem `json:"bootConfigs,omitempty"`
	HostReservations []CheckItem `json:"hostReservations,omitempty"`
	JobRunners       []CheckItem `json:"jobRunners,omitempty"`
	Jobs             []CheckItem `json:"jobs,omitempty"`
	StoragePools     []CheckItem `json:"storagePools,omitempty"`
	MicroDNS         []CheckItem `json:"microDNS,omitempty"`
	SmokeTests       []CheckItem `json:"smokeTests,omitempty"`
	PodLiveness      []CheckItem `json:"podLiveness,omitempty"`
}

// CheckItem is a single check result.
type CheckItem struct {
	Name    string `json:"name"`
	Status  string `json:"status"`  // "pass", "fail", "warn"
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

func (p *MicroKubeProvider) handleConsistency(w http.ResponseWriter, r *http.Request) {
	report := p.runConsistencyChecks(r.Context())
	podWriteJSON(w, http.StatusOK, report)
}

// handleConsistencyRepair cleans up orphaned IPAM entries where the veth
// no longer exists on the device.
func (p *MicroKubeProvider) handleConsistencyRepair(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ipamAllocs := p.deps.NetworkMgr.GetAllocations()
	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("listing ports: %v", err), http.StatusInternalServerError)
		return
	}

	actualMap := make(map[string]bool, len(actualPorts))
	for _, port := range actualPorts {
		actualMap[port.Name] = true
	}

	var released []string
	for veth := range ipamAllocs {
		if !actualMap[veth] {
			if err := p.deps.NetworkMgr.ReleaseInterface(ctx, veth); err != nil {
				p.deps.Logger.Warnw("repair: failed to release orphan", "veth", veth, "error", err)
			} else {
				released = append(released, veth)
			}
		}
	}

	podWriteJSON(w, http.StatusOK, map[string]interface{}{
		"released": released,
		"count":    len(released),
	})
}

func (p *MicroKubeProvider) runConsistencyChecks(ctx context.Context) ConsistencyReport {
	report := ConsistencyReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	report.Checks.Containers = p.checkContainers(ctx)
	report.Checks.DNS = p.checkDNS(ctx)
	report.Checks.Manifest = p.checkManifest()
	report.Checks.IPAM = p.checkIPAM(ctx)
	report.Checks.Network = p.checkNetworkHealth(ctx)
	report.Checks.Deployments = p.checkDeployments()
	report.Checks.PVCs = p.checkPVCs(ctx)
	report.Checks.Networks = p.checkNetworkCRDs(ctx)
	report.Checks.BMHs = p.checkBMHs()
	report.Checks.Registries = p.checkRegistryCRDs(ctx)
	report.Checks.ISCSICdroms = p.checkISCSICdromCRDs(ctx)
	report.Checks.ISCSIDisks = p.checkISCSIDiskCRDs(ctx)
	report.Checks.BootConfigs = p.checkBootConfigCRDs(ctx)
	report.Checks.HostReservations = p.checkHostReservationCRDs(ctx)
	report.Checks.JobRunners = p.checkJobRunnerCRDs(ctx)
	report.Checks.Jobs = p.checkJobCRDs(ctx)
	report.Checks.StoragePools = p.checkStoragePoolCRDs(ctx)
	report.Checks.MicroDNS = p.checkMicroDNSServices(ctx)
	report.Checks.SmokeTests = p.checkSmokeTests()
	report.Checks.PodLiveness = p.checkPodLiveness(ctx)

	for _, items := range [][]CheckItem{
		report.Checks.Containers,
		report.Checks.DNS,
		report.Checks.Manifest,
		report.Checks.IPAM,
		report.Checks.Network,
		report.Checks.Deployments,
		report.Checks.PVCs,
		report.Checks.Networks,
		report.Checks.BMHs,
		report.Checks.Registries,
		report.Checks.ISCSICdroms,
		report.Checks.ISCSIDisks,
		report.Checks.BootConfigs,
		report.Checks.HostReservations,
		report.Checks.JobRunners,
		report.Checks.Jobs,
		report.Checks.StoragePools,
		report.Checks.MicroDNS,
		report.Checks.SmokeTests,
		report.Checks.PodLiveness,
	} {
		for _, item := range items {
			switch item.Status {
			case "pass":
				report.Summary.Pass++
			case "fail":
				report.Summary.Fail++
			case "warn":
				report.Summary.Warn++
			}
		}
	}

	return report
}

// checkContainers verifies each manifest container exists and is running,
// and that its veth exists.
func (p *MicroKubeProvider) checkContainers(ctx context.Context) []CheckItem {
	var items []CheckItem

	manifestPath := p.deps.Config.Lifecycle.BootManifestPath
	if manifestPath == "" {
		return []CheckItem{{
			Name:    "boot-manifest",
			Status:  "warn",
			Message: "no boot manifest path configured",
		}}
	}

	pods, _, err := loadManifests(manifestPath)
	if err != nil {
		return []CheckItem{{
			Name:    "boot-manifest",
			Status:  "fail",
			Message: fmt.Sprintf("failed to load manifest: %v", err),
		}}
	}

	for _, pod := range pods {
		for i, c := range pod.Spec.Containers {
			name := sanitizeName(pod, c.Name)
			checkName := fmt.Sprintf("container/%s/%s", pod.Name, c.Name)

			ct, err := p.deps.Runtime.GetContainer(ctx, name)
			if err != nil {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "container not found",
					Details: fmt.Sprintf("expected RouterOS container %q", name),
				})
			} else if ct.Status != "running" {
				detail := fmt.Sprintf("id=%s", ct.ID)
				if ct.Comment != "" {
					detail = fmt.Sprintf("id=%s comment=%q", ct.ID, ct.Comment)
				}
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("container exists but status is %q", ct.Status),
					Details: detail,
				})
			} else {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: "running",
					Details: fmt.Sprintf("id=%s", ct.ID),
				})
			}

			// Check veth exists
			vethName := vethName(pod, i)
			if _, _, ok := p.deps.NetworkMgr.GetPortInfo(vethName); !ok {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("veth/%s", vethName),
					Status:  "fail",
					Message: "veth not found in allocations",
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("veth/%s", vethName),
					Status:  "pass",
					Message: "veth allocated",
				})
			}
		}
	}

	return items
}

// checkDNS verifies DNS records match expected state from all sources:
// boot manifest, NATS store, static records, DHCP reservations, and
// infrastructure records.
func (p *MicroKubeProvider) checkDNS(ctx context.Context) []CheckItem {
	var items []CheckItem

	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		return []CheckItem{{
			Name:    "dns-client",
			Status:  "warn",
			Message: "no DNS client configured",
		}}
	}

	// Use ALL desired pods (tracked + NATS + boot-order), not just boot manifest
	allPods := p.allDesiredPods(ctx)

	// Check each network that has DNS configured
	for _, netName := range p.deps.NetworkMgr.Networks() {
		netDef, ok := p.deps.NetworkMgr.NetworkDef(netName)
		if !ok || netDef.DNS.Endpoint == "" || netDef.DNS.Zone == "" {
			continue
		}

		// DNS port 53 liveness check — verify the resolver is actually
		// answering queries, not just that the container is running.
		if netDef.DNS.Server != "" && !netDef.ExternalDNS {
			if probeDNSPort(netDef.DNS.Server, netDef.DNS.Zone, 3*time.Second) {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("dns-liveness/%s", netName),
					Status:  "pass",
					Message: fmt.Sprintf("DNS port 53 responding on %s", netDef.DNS.Server),
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("dns-liveness/%s", netName),
					Status:  "fail",
					Message: fmt.Sprintf("DNS port 53 NOT responding on %s", netDef.DNS.Server),
					Details: "recursor may have crashed — container restart needed",
				})
			}
		}

		zoneID, ok := p.deps.NetworkMgr.NetworkZoneID(netName)
		if !ok {
			status := "warn"
			msg := "zone ID not cached (DNS may not be initialized)"
			if netDef.ExternalDNS {
				status = "pass"
				msg = "external DNS (zone init deferred)"
			}
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns-zone/%s", netName),
				Status:  status,
				Message: msg,
			})
			continue
		}

		records, err := dnsClient.ListRecords(ctx, netDef.DNS.Endpoint, zoneID)
		if err != nil {
			status := "fail"
			if netDef.ExternalDNS {
				status = "pass"
			}
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns-zone/%s", netName),
				Status:  status,
				Message: fmt.Sprintf("DNS endpoint unreachable: %v", err),
			})
			continue
		}

		// Build a lookup of actual records: hostname -> []ip
		actualRecords := make(map[string][]string)
		for _, r := range records {
			if r.Type == "A" {
				actualRecords[r.Name] = append(actualRecords[r.Name], r.Data.Data)
			}
		}

		// Build expected records from all desired pods
		expectedRecords := p.buildExpectedDNSRecords(allPods, netName)

		// Add static records from config
		for _, rec := range netDef.DNS.StaticRecords {
			if rec.Name != "" && rec.IP != "" {
				expectedRecords[rec.Name] = expectedDNS{ip: rec.IP}
			}
		}

		// BMH DNS records (data + IPMI hostnames) are included via buildExpectedDNSRecords.

		// Add infrastructure records (gateway + DNS server)
		if netDef.Gateway != "" {
			expectedRecords["rose1"] = expectedDNS{ip: netDef.Gateway}
		}
		if netDef.DNS.Server != "" {
			expectedRecords["dns"] = expectedDNS{ip: netDef.DNS.Server}
		}

		// Check expected vs actual
		for hostname, expected := range expectedRecords {
			checkName := fmt.Sprintf("dns/%s/%s", netName, hostname)
			actuals, exists := actualRecords[hostname]
			if !exists {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "DNS record missing",
					Details: fmt.Sprintf("expected A record -> %s", expected.ip),
				})
				continue
			}

			found := false
			for _, a := range actuals {
				if a == expected.ip {
					found = true
					break
				}
			}
			if !found {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "DNS record has wrong IP",
					Details: fmt.Sprintf("expected=%s actual=%v", expected.ip, actuals),
				})
			} else {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: "record correct",
					Details: fmt.Sprintf("ip=%s", expected.ip),
				})
			}
			delete(actualRecords, hostname)
		}

		// Any remaining actual records are stale — flag for cleanup
		for hostname, ips := range actualRecords {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns/%s/%s", netName, hostname),
				Status:  "warn",
				Message: "stale DNS record",
				Details: fmt.Sprintf("ips=%v", ips),
			})
		}
	}

	return items
}

type expectedDNS struct {
	ip string
}

// buildExpectedDNSRecords constructs the set of DNS hostnames and IPs expected
// from the boot manifest for a given network.
func (p *MicroKubeProvider) buildExpectedDNSRecords(pods []*corev1.Pod, networkName string) map[string]expectedDNS {
	expected := make(map[string]expectedDNS)

	// Add BMH data network DNS records (server hostnames from DHCP reservations)
	for _, bmh := range p.bareMetalHosts {
		if bmh.Spec.Network == networkName && bmh.Spec.IP != "" && bmh.Spec.Hostname != "" {
			expected[bmh.Spec.Hostname] = expectedDNS{ip: bmh.Spec.IP}
		}
		// Add BMH IPMI network DNS records
		if bmh.Spec.BMC.Network == networkName && bmh.Spec.BMC.Address != "" && bmh.Spec.BMC.Hostname != "" {
			expected[bmh.Spec.BMC.Hostname] = expectedDNS{ip: bmh.Spec.BMC.Address}
		}
	}

	for _, pod := range pods {
		podNetwork := pod.Annotations[annotationNetwork]
		if podNetwork == "" {
			// Default network — only match if this is the first network
			nets := p.deps.NetworkMgr.Networks()
			if len(nets) > 0 {
				podNetwork = nets[0]
			}
		}
		if podNetwork != networkName {
			continue
		}

		staticIP := pod.Annotations[annotationStaticIP]

		for i, c := range pod.Spec.Containers {
			veth := vethName(pod, i)
			ip, _, ok := p.deps.NetworkMgr.GetPortInfo(veth)
			if !ok {
				continue
			}
			if staticIP != "" {
				ip = staticIP
			}

			// Container hostname: container.pod
			containerHostname := c.Name + "." + pod.Name
			expected[containerHostname] = expectedDNS{ip: ip}
		}

		// Pod-level alias: podName -> first container's IP
		if len(pod.Spec.Containers) > 0 {
			firstContainer := pod.Spec.Containers[0].Name
			veth := vethName(pod, 0)
			if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(veth); ok {
				resolvedIP := ip
				if staticIP != "" {
					resolvedIP = staticIP
				}
				expected[pod.Name] = expectedDNS{ip: resolvedIP}

				// Custom aliases
				if ann := pod.Annotations[annotationAliases]; ann != "" {
					aliases := parseAliases(ann, firstContainer)
					for _, a := range aliases {
						// Find the IP for the alias's target container
						for ci, c := range pod.Spec.Containers {
							if c.Name == a.containerName {
								aVeth := vethName(pod, ci)
								if aIP, _, ok := p.deps.NetworkMgr.GetPortInfo(aVeth); ok {
									aliasIP := aIP
									if staticIP != "" {
										aliasIP = staticIP
									}
									expected[a.hostname] = expectedDNS{ip: aliasIP}
								}
								break
							}
						}
					}
				}
			}
		}
	}

	return expected
}

// checkManifest compares boot manifest pods against tracked pods.
func (p *MicroKubeProvider) checkManifest() []CheckItem {
	var items []CheckItem

	manifestPath := p.deps.Config.Lifecycle.BootManifestPath
	if manifestPath == "" {
		return []CheckItem{{
			Name:    "boot-manifest",
			Status:  "warn",
			Message: "no boot manifest path configured",
		}}
	}

	pods, _, err := loadManifests(manifestPath)
	if err != nil {
		return []CheckItem{{
			Name:    "manifest-load",
			Status:  "fail",
			Message: fmt.Sprintf("failed to load manifest: %v", err),
		}}
	}

	manifestSet := make(map[string]bool, len(pods))
	for _, pod := range pods {
		key := podKey(pod)
		manifestSet[key] = true

		if _, exists := p.pods[key]; exists {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("manifest/%s", key),
				Status:  "pass",
				Message: "manifest pod is tracked",
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("manifest/%s", key),
				Status:  "fail",
				Message: "manifest pod not tracked by provider",
			})
		}
	}

	// Tracked pods not in manifest — these come from NATS (oc apply),
	// which is the normal deployment path. Not a warning.
	for key := range p.pods {
		if !manifestSet[key] {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("manifest/%s", key),
				Status:  "pass",
				Message: "NATS-sourced pod (not in boot manifest)",
			})
		}
	}

	return items
}

// checkIPAM cross-references IPAM allocations against actual veths.
func (p *MicroKubeProvider) checkIPAM(ctx context.Context) []CheckItem {
	var items []CheckItem

	ipamAllocs := p.deps.NetworkMgr.GetAllocations() // veth -> ip

	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		return []CheckItem{{
			Name:    "ipam-ports",
			Status:  "fail",
			Message: fmt.Sprintf("failed to list actual ports: %v", err),
		}}
	}

	// Build lookup: veth name -> actual PortInfo
	actualMap := make(map[string]network.PortInfo, len(actualPorts))
	for _, port := range actualPorts {
		actualMap[port.Name] = port
	}

	// Check each IPAM allocation has a matching actual port
	for veth, ipamIP := range ipamAllocs {
		checkName := fmt.Sprintf("ipam/%s", veth)
		actual, exists := actualMap[veth]
		if !exists {
			items = append(items, CheckItem{
				Name:    checkName,
				Status:  "warn",
				Message: "IPAM entry exists but veth not found on device (orphan)",
				Details: fmt.Sprintf("ipam_ip=%s", ipamIP),
			})
			continue
		}

		// Compare IPs (IPAM returns bare IP, actual may have CIDR)
		actualIP := strings.Split(actual.Address, "/")[0]
		if actualIP == ipamIP {
			items = append(items, CheckItem{
				Name:    checkName,
				Status:  "pass",
				Message: "IPAM matches actual",
				Details: fmt.Sprintf("ip=%s", ipamIP),
			})
		} else {
			items = append(items, CheckItem{
				Name:    checkName,
				Status:  "fail",
				Message: "IP mismatch between IPAM and actual veth",
				Details: fmt.Sprintf("ipam=%s actual=%s", ipamIP, actualIP),
			})
		}
		delete(actualMap, veth)
	}

	// Check for veths that exist on device but not in IPAM (only veth-* ports)
	for name, port := range actualMap {
		if !strings.HasPrefix(name, "veth_") {
			continue
		}
		items = append(items, CheckItem{
			Name:    fmt.Sprintf("ipam/%s", name),
			Status:  "fail",
			Message: "veth exists on device but not in IPAM",
			Details: fmt.Sprintf("actual_ip=%s", port.Address),
		})
	}

	// Verify static IP annotations match actual allocations
	manifestPath := p.deps.Config.Lifecycle.BootManifestPath
	if manifestPath != "" {
		pods, _, err := loadManifests(manifestPath)
		if err == nil {
			for _, pod := range pods {
				staticIP := pod.Annotations[annotationStaticIP]
				if staticIP == "" {
					continue
				}
				for i := range pod.Spec.Containers {
					veth := vethName(pod, i)
					if ip, _, ok := p.deps.NetworkMgr.GetPortInfo(veth); ok {
						if ip == staticIP {
							items = append(items, CheckItem{
								Name:    fmt.Sprintf("static-ip/%s/%s", pod.Name, veth),
								Status:  "pass",
								Message: "static IP matches allocation",
								Details: fmt.Sprintf("ip=%s", staticIP),
							})
						} else {
							items = append(items, CheckItem{
								Name:    fmt.Sprintf("static-ip/%s/%s", pod.Name, veth),
								Status:  "fail",
								Message: "static IP mismatch",
								Details: fmt.Sprintf("expected=%s actual=%s", staticIP, ip),
							})
						}
					}
				}
			}
		}
	}

	return items
}

// probeDNSPort sends a minimal DNS query (SOA for the zone) to the server's
// port 53 over UDP and checks for any response. This catches the case where
// the microdns container is running (REST API up) but the recursor/auth
// listener on port 53 has crashed or failed to start.
func probeDNSPort(serverIP, zone string, timeout time.Duration) bool {
	// Build a minimal DNS query for SOA of the zone.
	// DNS wire format: header (12 bytes) + question section.
	// This is simpler and avoids pulling in a DNS library.
	conn, err := net.DialTimeout("udp", serverIP+":53", timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Encode a minimal DNS query packet
	query := buildDNSQuery(zone)
	if _, err := conn.Write(query); err != nil {
		return false
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	// Any response at all (even NXDOMAIN/SERVFAIL) means port 53 is alive
	return err == nil && n > 0
}

// buildDNSQuery constructs a minimal DNS query packet for the SOA record of a zone.
func buildDNSQuery(zone string) []byte {
	// DNS Header: ID=0x1234, QR=0, OPCODE=0, RD=1, QDCOUNT=1
	header := []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // Flags: RD=1
		0x00, 0x01, // QDCOUNT=1
		0x00, 0x00, // ANCOUNT=0
		0x00, 0x00, // NSCOUNT=0
		0x00, 0x00, // ARCOUNT=0
	}

	// Question: encode zone name as DNS labels
	var question []byte
	for _, label := range strings.Split(strings.TrimSuffix(zone, "."), ".") {
		if len(label) == 0 {
			continue
		}
		question = append(question, byte(len(label)))
		question = append(question, []byte(label)...)
	}
	question = append(question, 0x00)       // root label
	question = append(question, 0x00, 0x06) // QTYPE=SOA
	question = append(question, 0x00, 0x01) // QCLASS=IN

	return append(header, question...)
}

// checkPVCs verifies PVC consistency between memory, NATS, and on-disk state.
func (p *MicroKubeProvider) checkPVCs(ctx context.Context) []CheckItem {
	var items []CheckItem

	// Build set of PVCs actively used by running pods
	activePVCs := make(map[string]bool) // pvc key -> true
	for _, pod := range p.pods {
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				activePVCs[pod.Namespace+"/"+v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}

	// Check each PVC in memory has a matching NATS entry and disk directory
	for key, pvc := range p.pvcs {
		// 1. NATS sync check
		if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
			storeKey := pvc.Namespace + "." + pvc.Name
			_, _, err := p.deps.Store.PersistentVolumeClaims.Get(ctx, storeKey)
			if err != nil {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("pvc/%s", key),
					Status:  "fail",
					Message: "PVC in memory but not in NATS store",
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("pvc/%s", key),
					Status:  "pass",
					Message: "PVC synced with NATS store",
				})
			}
		}

		// 2. Disk directory check
		hostPath := p.pvcHostPath(pvc)
		exists, err := p.deps.Runtime.FileExists(ctx, hostPath)
		if err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("pvc-disk/%s", key),
				Status:  "warn",
				Message: fmt.Sprintf("cannot check PVC directory: %v", err),
				Details: fmt.Sprintf("path=%s", hostPath),
			})
		} else if !exists {
			// Directory missing — auto-create (self-healing)
			if mkErr := p.deps.Runtime.EnsureDirectory(ctx, hostPath); mkErr != nil {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("pvc-disk/%s", key),
					Status:  "fail",
					Message: "PVC directory missing and auto-create failed",
					Details: fmt.Sprintf("path=%s error=%v", hostPath, mkErr),
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("pvc-disk/%s", key),
					Status:  "warn",
					Message: "PVC directory was missing, auto-created",
					Details: fmt.Sprintf("path=%s", hostPath),
				})
			}
		} else {
			// Directory exists — check if it has data (for active PVCs)
			if activePVCs[key] {
				entries, listErr := p.deps.Runtime.ListDirectory(ctx, hostPath)
				if listErr != nil {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("pvc-disk/%s", key),
						Status:  "warn",
						Message: fmt.Sprintf("PVC directory exists but cannot list: %v", listErr),
						Details: fmt.Sprintf("path=%s", hostPath),
					})
				} else if len(entries) == 0 {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("pvc-disk/%s", key),
						Status:  "warn",
						Message: "PVC directory exists but is empty (data may be lost or first boot)",
						Details: fmt.Sprintf("path=%s", hostPath),
					})
				} else {
					items = append(items, CheckItem{
						Name:    fmt.Sprintf("pvc-disk/%s", key),
						Status:  "pass",
						Message: fmt.Sprintf("PVC directory exists with %d entries", len(entries)),
						Details: fmt.Sprintf("path=%s", hostPath),
					})
				}
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("pvc-disk/%s", key),
					Status:  "pass",
					Message: "PVC directory exists on disk",
					Details: fmt.Sprintf("path=%s", hostPath),
				})
			}
		}
	}

	// Check pods referencing PVCs have valid PVC objects
	for _, pod := range p.pods {
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			pvcKey := pod.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
			checkName := fmt.Sprintf("pvc-ref/%s/%s", podKey(pod), v.Name)
			if _, ok := p.pvcs[pvcKey]; ok {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: fmt.Sprintf("PVC %s exists", v.PersistentVolumeClaim.ClaimName),
				})
			} else {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("pod references PVC %s which does not exist", v.PersistentVolumeClaim.ClaimName),
				})
			}
		}
	}

	return items
}

// checkDeployments verifies each deployment has the correct number of running pods.
func (p *MicroKubeProvider) checkDeployments() []CheckItem {
	var items []CheckItem
	for key, deploy := range p.deployments {
		replicas := deploy.Spec.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		ownedPods := p.deploymentPods(deploy)
		actual := int32(len(ownedPods))

		if actual == replicas {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("deploy/%s", key),
				Status:  "pass",
				Message: fmt.Sprintf("deployment has %d/%d pods", actual, replicas),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("deploy/%s", key),
				Status:  "fail",
				Message: fmt.Sprintf("deployment has %d/%d pods", actual, replicas),
				Details: fmt.Sprintf("expected=%d actual=%d", replicas, actual),
			})
		}
	}
	return items
}

// CheckConsistencyAsync runs a consistency check in the background after
// container operations. It detects and cleans up orphaned veths and IPAM entries.
// The delay gives the system time to settle after the triggering operation.
func (p *MicroKubeProvider) CheckConsistencyAsync(reason string) {
	// Only allow one consistency check goroutine at a time.
	// If one is already running/pending, skip this invocation.
	if !p.consistencyRunning.CompareAndSwap(false, true) {
		p.deps.Logger.Debugw("consistency check already running, skipping", "trigger", reason)
		return
	}
	go func() {
		defer p.consistencyRunning.Store(false)
		time.Sleep(5 * time.Second) // let the operation settle

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		p.deps.Logger.Infow("running async consistency check", "trigger", reason)

		// Re-sync IPAM allocations from device before orphan detection.
		// Ensures veths added by other reconcile cycles are registered
		// in IPAM before we decide what's orphaned.
		if err := p.deps.NetworkMgr.ResyncAllocations(ctx); err != nil {
			p.deps.Logger.Warnw("IPAM re-sync failed", "error", err)
		}

		var cleaned int

		n, err := p.cleanOrphanedContainers(ctx)
		if err != nil {
			p.deps.Logger.Warnw("orphaned container check failed", "error", err)
		}
		cleaned += n

		n, err = p.cleanOrphanedVeths(ctx)
		if err != nil {
			p.deps.Logger.Warnw("orphaned veth check failed", "error", err)
		}
		cleaned += n

		n, err = p.cleanOrphanedIPAM(ctx)
		if err != nil {
			p.deps.Logger.Warnw("orphaned IPAM check failed", "error", err)
		}
		cleaned += n

		n, err = p.repairNetworkHealth(ctx)
		if err != nil {
			p.deps.Logger.Warnw("network health repair failed", "error", err)
		}
		cleaned += n

		n, err = p.cleanStaleDNSRecords(ctx)
		if err != nil {
			p.deps.Logger.Warnw("stale DNS cleanup failed", "error", err)
		}
		cleaned += n

		n = p.repairDNSLiveness(ctx)
		cleaned += n

		if cleaned > 0 {
			p.deps.Logger.Infow("consistency check cleaned up resources", "trigger", reason, "cleaned", cleaned)
		} else {
			p.deps.Logger.Debugw("consistency check passed", "trigger", reason)
		}
	}()
}

// cleanOrphanedVeths finds veths on the device that have no corresponding
// desired pod and removes them. Only runs when NATS is connected so we
// have the full desired state.
func (p *MicroKubeProvider) cleanOrphanedVeths(ctx context.Context) (int, error) {
	// Don't remove veths until we have the full desired state from NATS.
	if p.deps.Store == nil || !p.deps.Store.Connected() {
		return 0, nil
	}

	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing actual ports: %w", err)
	}

	// Build set of veths from ALL desired sources (tracked + NATS + boot-order)
	expectedVeths := make(map[string]bool)

	// Snapshot tracked pods under lock (called from background goroutine)
	p.mu.RLock()
	for _, pod := range p.pods {
		for i := range pod.Spec.Containers {
			expectedVeths[vethName(pod, i)] = true
		}
	}
	p.mu.RUnlock()

	storePods, _ := p.loadFromStore(ctx)
	for _, pod := range storePods {
		for i := range pod.Spec.Containers {
			expectedVeths[vethName(pod, i)] = true
		}
	}

	if p.deps.Config.Lifecycle.BootManifestPath != "" {
		bootPods, _, err := loadManifests(p.deps.Config.Lifecycle.BootManifestPath)
		if err == nil {
			for _, pod := range bootPods {
				for i := range pod.Spec.Containers {
					expectedVeths[vethName(pod, i)] = true
				}
			}
		}
	}

	cleaned := 0
	for _, port := range actualPorts {
		if !strings.HasPrefix(port.Name, "veth_") {
			continue
		}
		if expectedVeths[port.Name] {
			continue
		}

		// This veth has no desired pod — it's orphaned
		p.deps.Logger.Infow("removing orphaned veth", "name", port.Name, "address", port.Address)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, port.Name); err != nil {
			p.deps.Logger.Warnw("failed to release orphaned veth", "name", port.Name, "error", err)
		} else {
			cleaned++
		}
	}

	return cleaned, nil
}

// cleanOrphanedContainers finds RouterOS containers that follow the mkube
// naming convention (namespace_pod_container) but are not owned by any
// desired pod (tracked, in NATS store, or in boot-order manifest).
// Only runs when we have the full desired state (NATS connected) to avoid
// incorrectly killing containers whose pods haven't loaded yet.
func (p *MicroKubeProvider) cleanOrphanedContainers(ctx context.Context) (int, error) {
	// Don't remove containers until we have the full desired state from NATS.
	// Without NATS, we only know about boot-order pods — NATS-sourced pods
	// would be incorrectly flagged as orphaned and killed.
	if p.deps.Store == nil || !p.deps.Store.Connected() {
		return 0, nil
	}

	containers, err := p.deps.Runtime.ListContainers(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing containers: %w", err)
	}

	// Build the full set of expected container names from ALL sources:
	// 1. Currently tracked pods
	// 2. NATS store (pods that may not be tracked yet)
	// 3. Boot-order manifest
	expectedContainers := make(map[string]bool)

	// Source 1: tracked pods (snapshot under lock — called from background goroutine)
	p.mu.RLock()
	for _, pod := range p.pods {
		for _, c := range pod.Spec.Containers {
			expectedContainers[sanitizeName(pod, c.Name)] = true
		}
	}
	p.mu.RUnlock()

	// Source 2: NATS store
	storePods, _ := p.loadFromStore(ctx)
	for _, pod := range storePods {
		for _, c := range pod.Spec.Containers {
			expectedContainers[sanitizeName(pod, c.Name)] = true
		}
	}

	// Source 3: boot-order manifest
	if p.deps.Config.Lifecycle.BootManifestPath != "" {
		bootPods, _, err := loadManifests(p.deps.Config.Lifecycle.BootManifestPath)
		if err == nil {
			for _, pod := range bootPods {
				for _, c := range pod.Spec.Containers {
					expectedContainers[sanitizeName(pod, c.Name)] = true
				}
			}
		}
	}

	// Source 4: deployment-expected containers (reads p.deployments, needs lock)
	p.mu.RLock()
	for name := range p.deploymentExpectedContainers() {
		expectedContainers[name] = true
	}
	p.mu.RUnlock()

	cleaned := 0
	for _, ct := range containers {
		// Only consider containers following the mkube naming convention
		// (contains underscore separators like "default_nginx_web").
		// Containers without underscores are not managed by mkube (e.g. kube.gt.lo).
		if !strings.Contains(ct.Name, "_") {
			continue
		}
		if expectedContainers[ct.Name] {
			continue
		}

		p.deps.Logger.Infow("removing orphaned container",
			"name", ct.Name, "id", ct.ID, "status", ct.Status, "interface", ct.Interface)
		p.stopAndRemoveContainer(ctx, ct.Name, ct.ID)
		_ = p.deps.Runtime.RemoveMountsByList(ctx, ct.Name)
		cleaned++
	}

	return cleaned, nil
}

// cleanOrphanedIPAM removes IPAM allocations for veths that no longer exist
// on the device.
func (p *MicroKubeProvider) cleanOrphanedIPAM(ctx context.Context) (int, error) {
	ipamAllocs := p.deps.NetworkMgr.GetAllocations()
	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing actual ports: %w", err)
	}

	actualMap := make(map[string]bool, len(actualPorts))
	for _, port := range actualPorts {
		actualMap[port.Name] = true
	}

	cleaned := 0
	for veth := range ipamAllocs {
		if actualMap[veth] {
			continue
		}
		p.deps.Logger.Infow("releasing orphaned IPAM entry", "veth", veth)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, veth); err != nil {
			p.deps.Logger.Warnw("failed to release orphaned IPAM", "veth", veth, "error", err)
		} else {
			cleaned++
		}
	}

	return cleaned, nil
}

// cleanStaleDNSRecords removes A records with wrong IPs for expected hostnames
// (e.g. old IPs from pod recreations). Records with hostnames not in the
// expected set are left untouched — they may be user-created via REST API.
func (p *MicroKubeProvider) cleanStaleDNSRecords(ctx context.Context) (int, error) {
	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		return 0, nil
	}

	// Enable batch mode to cache record lists across zone iterations
	dnsClient.BeginBatch()
	defer dnsClient.EndBatch()

	// Snapshot tracked pods under lock (called from background goroutine)
	p.mu.RLock()
	allPods := p.allDesiredPods(ctx)
	p.mu.RUnlock()
	cleaned := 0

	for _, netName := range p.deps.NetworkMgr.Networks() {
		netDef, ok := p.deps.NetworkMgr.NetworkDef(netName)
		if !ok || netDef.DNS.Endpoint == "" || netDef.DNS.Zone == "" {
			continue
		}

		zoneID, ok := p.deps.NetworkMgr.NetworkZoneID(netName)
		if !ok {
			continue
		}

		records, err := dnsClient.ListRecords(ctx, netDef.DNS.Endpoint, zoneID)
		if err != nil {
			continue
		}

		// Build expected: hostname -> expected IP (reads p.bareMetalHosts, needs lock)
		p.mu.RLock()
		expected := p.buildExpectedDNSRecords(allPods, netName)
		p.mu.RUnlock()
		for _, rec := range netDef.DNS.StaticRecords {
			if rec.Name != "" && rec.IP != "" {
				expected[rec.Name] = expectedDNS{ip: rec.IP}
			}
		}
		if netDef.Gateway != "" {
			expected["rose1"] = expectedDNS{ip: netDef.Gateway}
		}
		if netDef.DNS.Server != "" {
			expected["dns"] = expectedDNS{ip: netDef.DNS.Server}
		}

		for _, r := range records {
			if r.Type != "A" {
				continue
			}

			exp, isExpected := expected[r.Name]
			if !isExpected {
				// Hostname not in mkube's expected set — leave it alone.
				// User-created records (via REST API or mk apply) are not
				// managed by mkube and must not be deleted.
				continue
			} else if r.Data.Data != exp.ip {
				// Hostname expected but this record has wrong IP — stale from old allocation
				p.deps.Logger.Infow("deleting stale DNS record (wrong IP)",
					"network", netName, "hostname", r.Name, "stale_ip", r.Data.Data, "expected_ip", exp.ip, "id", r.ID)
				if err := dnsClient.DeleteRecord(ctx, netDef.DNS.Endpoint, zoneID, r.ID); err != nil {
					p.deps.Logger.Warnw("failed to delete stale DNS record",
						"hostname", r.Name, "ip", r.Data.Data, "error", err)
				} else {
					cleaned++
				}
			}
		}
	}

	return cleaned, nil
}

// networkHealthThreshold is the number of consecutive failures before a pod's
// network is considered broken and the pod is recreated.
const networkHealthThreshold = 3

// checkNetworkHealth produces read-only CheckItems for the /api/v1/consistency
// endpoint. It verifies each tracked pod has a veth with a valid IP and that
// static-IP annotations match the actual allocation.
func (p *MicroKubeProvider) checkNetworkHealth(ctx context.Context) []CheckItem {
	var items []CheckItem

	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		return []CheckItem{{
			Name:    "network-ports",
			Status:  "fail",
			Message: fmt.Sprintf("failed to list actual ports: %v", err),
		}}
	}

	actualMap := make(map[string]network.PortInfo, len(actualPorts))
	for _, port := range actualPorts {
		actualMap[port.Name] = port
	}

	// Collect all desired pods from all sources
	allPods := p.allDesiredPods(ctx)

	for _, pod := range allPods {
		key := podKey(pod)
		staticIP := pod.Annotations[annotationStaticIP]

		for i := range pod.Spec.Containers {
			veth := vethName(pod, i)
			checkName := fmt.Sprintf("network/%s/%s", key, veth)

			actual, exists := actualMap[veth]
			if !exists {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "veth missing on device",
				})
				continue
			}

			actualIP := strings.Split(actual.Address, "/")[0]
			if actualIP == "" {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "veth has no IP address",
					Details: fmt.Sprintf("veth=%s", veth),
				})
				continue
			}

			if staticIP != "" && actualIP != staticIP {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: "static IP mismatch",
					Details: fmt.Sprintf("expected=%s actual=%s", staticIP, actualIP),
				})
				continue
			}

			failCount := p.networkFailures[key]
			if failCount > 0 {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "warn",
					Message: fmt.Sprintf("recovering (failures=%d/%d)", failCount, networkHealthThreshold),
					Details: fmt.Sprintf("ip=%s", actualIP),
				})
			} else {
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "pass",
					Message: "network healthy",
					Details: fmt.Sprintf("ip=%s", actualIP),
				})
			}
		}
	}

	return items
}

// repairDNSLiveness checks that DNS port 53 is responding on each managed
// network's DNS server. If the port is dead (recursor crashed while container
// is still running), the DNS pod is restarted via delete+create.
// DNS pods are restarted one at a time with liveness verification between
// each to prevent simultaneous DNS outages across all networks.
func (p *MicroKubeProvider) repairDNSLiveness(ctx context.Context) int {
	log := p.deps.Logger

	// Collect all dead DNS pods first
	type deadDNS struct {
		netName string
		server  string
		zone    string
		pod     *corev1.Pod
	}
	var deadList []deadDNS

	for _, netName := range p.deps.NetworkMgr.Networks() {
		netDef, ok := p.deps.NetworkMgr.NetworkDef(netName)
		if !ok || netDef.DNS.Server == "" || netDef.DNS.Zone == "" || netDef.ExternalDNS {
			continue
		}

		if probeDNSPort(netDef.DNS.Server, netDef.DNS.Zone, 3*time.Second) {
			continue
		}

		podKey := netName + "/dns"
		p.mu.RLock()
		pod, exists := p.pods[podKey]
		p.mu.RUnlock()
		if !exists {
			log.Warnw("DNS pod not tracked, cannot restart", "pod", podKey)
			continue
		}

		deadList = append(deadList, deadDNS{
			netName: netName,
			server:  netDef.DNS.Server,
			zone:    netDef.DNS.Zone,
			pod:     pod,
		})
	}

	if len(deadList) == 0 {
		return 0
	}

	log.Errorw("DNS port 53 dead, restarting DNS pods one at a time",
		"count", len(deadList))

	repaired := 0
	for i, dead := range deadList {
		podKey := dead.netName + "/dns"
		log.Infow("restarting dead DNS pod",
			"pod", podKey, "server", dead.server,
			"index", i+1, "total", len(deadList))

		// Restart: delete and recreate (hold lock for map writes in DeletePod/CreatePod)
		p.mu.Lock()
		if err := p.DeletePod(ctx, dead.pod); err != nil {
			p.mu.Unlock()
			log.Errorw("failed to delete dead DNS pod", "pod", podKey, "error", err)
			continue
		}
		p.mu.Unlock()

		// Re-read from NATS store to get clean spec
		if p.deps.Store == nil {
			log.Errorw("no store, cannot recreate DNS pod", "pod", podKey)
			continue
		}

		storeKey := dead.netName + ".dns"
		var storePod corev1.Pod
		if _, err := p.deps.Store.Pods.GetJSON(ctx, storeKey, &storePod); err != nil {
			log.Errorw("DNS pod not in store, cannot recreate", "pod", podKey, "error", err)
			continue
		}

		p.mu.Lock()
		if err := p.CreatePod(ctx, &storePod); err != nil {
			p.mu.Unlock()
			log.Errorw("failed to recreate DNS pod", "pod", podKey, "error", err)
			continue
		}
		p.mu.Unlock()

		// Wait for port 53 to come alive before restarting the next DNS pod
		alive := false
		for attempt := 0; attempt < 15; attempt++ {
			time.Sleep(3 * time.Second)
			if probeDNSPort(dead.server, dead.zone, 3*time.Second) {
				alive = true
				break
			}
			log.Infow("waiting for DNS port 53 to come alive",
				"pod", podKey, "attempt", attempt+1)
		}

		if alive {
			log.Infow("DNS pod restarted and port 53 alive", "pod", podKey)
			repaired++

			// Re-seed DHCP pools/reservations/forwarders — the database
			// may be empty after restart (redb ephemeral).
			p.mu.RLock()
			_, netOK := p.networks[dead.netName]
			p.mu.RUnlock()
			if netOK {
				p.triggerNetworkReseed(dead.netName)
			}
		} else {
			log.Errorw("DNS pod restarted but port 53 still dead, halting DNS repair",
				"pod", podKey)
			break // Don't restart more DNS pods if this one failed
		}
	}

	return repaired
}

// repairNetworkHealth checks all desired pods for broken networking and
// triggers a full delete+create cycle after networkHealthThreshold consecutive
// failures. Returns the number of pods repaired.
func (p *MicroKubeProvider) repairNetworkHealth(ctx context.Context) (int, error) {
	// Don't repair until we have the full desired state from NATS.
	if p.deps.Store == nil || !p.deps.Store.Connected() {
		return 0, nil
	}

	actualPorts, err := p.deps.NetworkMgr.ListActualPorts(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing actual ports: %w", err)
	}

	actualMap := make(map[string]network.PortInfo, len(actualPorts))
	for _, port := range actualPorts {
		actualMap[port.Name] = port
	}

	// Snapshot pods and redeploying state under lock (background goroutine)
	p.mu.RLock()
	allPods := p.allDesiredPods(ctx)
	redeployingSnap := make(map[string]bool, len(p.redeploying))
	for k, v := range p.redeploying {
		redeployingSnap[k] = v
	}
	p.mu.RUnlock()
	repaired := 0

	for _, pod := range allPods {
		key := podKey(pod)

		// Skip pods currently being redeployed
		if redeployingSnap[key] {
			continue
		}

		staticIP := pod.Annotations[annotationStaticIP]
		broken := false

		for i := range pod.Spec.Containers {
			veth := vethName(pod, i)
			actual, exists := actualMap[veth]

			if !exists {
				broken = true
				break
			}

			actualIP := strings.Split(actual.Address, "/")[0]
			if actualIP == "" {
				broken = true
				break
			}

			if staticIP != "" && actualIP != staticIP {
				broken = true
				break
			}
		}

		if broken {
			p.mu.Lock()
			p.networkFailures[key]++
			failCount := p.networkFailures[key]
			p.mu.Unlock()
			p.deps.Logger.Warnw("container has broken network",
				"pod", key, "failures", failCount, "threshold", networkHealthThreshold)

			if failCount >= networkHealthThreshold {
				p.deps.Logger.Infow("container network broken beyond threshold, triggering recreate",
					"pod", key, "failures", failCount)

				p.mu.Lock()
				if err := p.DeletePod(ctx, pod); err != nil {
					p.mu.Unlock()
					p.deps.Logger.Errorw("failed to delete pod for network repair",
						"pod", key, "error", err)
					continue
				}
				if err := p.CreatePod(ctx, pod); err != nil {
					p.mu.Unlock()
					p.deps.Logger.Errorw("failed to recreate pod for network repair",
						"pod", key, "error", err)
					continue
				}
				delete(p.networkFailures, key)
				p.mu.Unlock()
				repaired++
			}
		} else {
			// Network is healthy — reset failure counter
			p.mu.Lock()
			delete(p.networkFailures, key)
			p.mu.Unlock()
		}
	}

	return repaired, nil
}

// allDesiredPods collects pods from all sources: tracked, NATS store, and boot-order.
func (p *MicroKubeProvider) allDesiredPods(ctx context.Context) []*corev1.Pod {
	seen := make(map[string]bool)
	var result []*corev1.Pod

	// Source 1: tracked pods
	for key, pod := range p.pods {
		if !seen[key] {
			seen[key] = true
			result = append(result, pod)
		}
	}

	// Source 2: NATS store
	if p.deps.Store != nil && p.deps.Store.Connected() {
		storePods, _ := p.loadFromStore(ctx)
		for _, pod := range storePods {
			key := podKey(pod)
			if !seen[key] {
				seen[key] = true
				result = append(result, pod)
			}
		}
	}

	// Source 3: boot-order manifest
	if p.deps.Config.Lifecycle.BootManifestPath != "" {
		bootPods, _, err := loadManifests(p.deps.Config.Lifecycle.BootManifestPath)
		if err == nil {
			for _, pod := range bootPods {
				key := podKey(pod)
				if !seen[key] {
					seen[key] = true
					result = append(result, pod)
				}
			}
		}
	}

	return result
}

// checkBMHs verifies BareMetalHost objects for duplicates and NATS sync.
func (p *MicroKubeProvider) checkBMHs() []CheckItem {
	var items []CheckItem

	// Per-BMH checks: each BMH is a physical server
	bootMACs := make(map[string][]string) // MAC -> list of "ns/name"
	bmcMACs := make(map[string][]string)
	hostnames := make(map[string][]string) // hostname -> list of "ns/name"

	for key, bmh := range p.bareMetalHosts {
		if mac := strings.ToUpper(bmh.Spec.BootMACAddress); mac != "" && mac != "00:00:00:00:00:00" {
			bootMACs[mac] = append(bootMACs[mac], key)
		}
		if mac := strings.ToUpper(bmh.Spec.BMC.MAC); mac != "" {
			bmcMACs[mac] = append(bmcMACs[mac], key)
		}
		hostnames[bmh.Name] = append(hostnames[bmh.Name], key)

		// Validate data network reference
		if net := bmh.Spec.Network; net != "" {
			if _, ok := p.networks[net]; !ok {
				items = append(items, CheckItem{
					Name:    "bmh/" + bmh.Name + "/network",
					Status:  "fail",
					Message: fmt.Sprintf("data network %q not found", net),
				})
			} else {
				// Check DHCP reservation exists in the network
				found := false
				if n, ok := p.networks[net]; ok && bmh.Spec.BootMACAddress != "" {
					for _, r := range n.Spec.DHCP.Reservations {
						if strings.EqualFold(r.MAC, bmh.Spec.BootMACAddress) {
							found = true
							break
						}
					}
				}
				if found {
					items = append(items, CheckItem{
						Name:    "bmh/" + bmh.Name + "/data-reservation",
						Status:  "pass",
						Message: fmt.Sprintf("DHCP reservation in %s", net),
						Details: fmt.Sprintf("mac=%s ip=%s", bmh.Spec.BootMACAddress, bmh.Spec.IP),
					})
				} else if bmh.Spec.BootMACAddress != "" && bmh.Spec.BootMACAddress != "00:00:00:00:00:00" {
					items = append(items, CheckItem{
						Name:    "bmh/" + bmh.Name + "/data-reservation",
						Status:  "warn",
						Message: fmt.Sprintf("no DHCP reservation in %s for boot MAC %s", net, bmh.Spec.BootMACAddress),
					})
				}
			}
		}

		// Validate IPMI network reference
		if net := bmh.Spec.BMC.Network; net != "" {
			if _, ok := p.networks[net]; !ok {
				items = append(items, CheckItem{
					Name:    "bmh/" + bmh.Name + "/bmc-network",
					Status:  "fail",
					Message: fmt.Sprintf("BMC network %q not found", net),
				})
			} else {
				found := false
				if n, ok := p.networks[net]; ok && bmh.Spec.BMC.MAC != "" {
					for _, r := range n.Spec.DHCP.Reservations {
						if strings.EqualFold(r.MAC, bmh.Spec.BMC.MAC) {
							found = true
							break
						}
					}
				}
				if found {
					items = append(items, CheckItem{
						Name:    "bmh/" + bmh.Name + "/bmc-reservation",
						Status:  "pass",
						Message: fmt.Sprintf("IPMI reservation in %s", net),
						Details: fmt.Sprintf("mac=%s ip=%s", bmh.Spec.BMC.MAC, bmh.Spec.BMC.Address),
					})
				} else if bmh.Spec.BMC.MAC != "" {
					items = append(items, CheckItem{
						Name:    "bmh/" + bmh.Name + "/bmc-reservation",
						Status:  "warn",
						Message: fmt.Sprintf("no DHCP reservation in %s for BMC MAC %s", net, bmh.Spec.BMC.MAC),
					})
				}
			}
		}
	}

	// Duplicate detection: same physical server should not have multiple BMH objects
	for mac, owners := range bootMACs {
		if len(owners) > 1 {
			items = append(items, CheckItem{
				Name:    "bmh-dup-boot-mac/" + mac,
				Status:  "fail",
				Message: "duplicate boot MAC — same server registered twice",
				Details: fmt.Sprintf("shared by: %s", strings.Join(owners, ", ")),
			})
		}
	}
	for mac, owners := range bmcMACs {
		if len(owners) > 1 {
			items = append(items, CheckItem{
				Name:    "bmh-dup-bmc-mac/" + mac,
				Status:  "fail",
				Message: "duplicate BMC MAC — same server registered twice",
				Details: fmt.Sprintf("shared by: %s", strings.Join(owners, ", ")),
			})
		}
	}
	for name, owners := range hostnames {
		if len(owners) > 1 {
			items = append(items, CheckItem{
				Name:    "bmh-dup-name/" + name,
				Status:  "fail",
				Message: "duplicate hostname — same server in multiple namespaces",
				Details: fmt.Sprintf("found in: %s", strings.Join(owners, ", ")),
			})
		}
	}

	// Verify memory ↔ NATS sync
	if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
		natsKeys, _ := p.deps.Store.BareMetalHosts.Keys(context.Background(), "")
		natsSet := make(map[string]bool, len(natsKeys))
		for _, k := range natsKeys {
			natsSet[k] = true
		}
		for key := range p.bareMetalHosts {
			storeKey := strings.Replace(key, "/", ".", 1)
			if !natsSet[storeKey] {
				items = append(items, CheckItem{
					Name:    "bmh-nats/" + key,
					Status:  "warn",
					Message: "BMH in memory but not in NATS",
				})
			}
		}
	}

	return items
}

// checkMicroDNSServices verifies all microdns services (REST API, DHCP pools,
// reservations, DNS forwarders) for each managed network.
func (p *MicroKubeProvider) checkMicroDNSServices(ctx context.Context) []CheckItem {
	var items []CheckItem
	dnsClient := p.deps.NetworkMgr.DNSClient()
	if dnsClient == nil {
		return nil
	}

	for _, net := range p.networks {
		if net.Spec.ExternalDNS || net.Spec.DNS.Zone == "" || net.Spec.DNS.Server == "" {
			continue
		}

		endpoint := net.Spec.DNS.Endpoint
		if endpoint == "" {
			endpoint = "http://" + net.Spec.DNS.Server + ":8080"
		}

		// 1. REST API health
		if err := dnsClient.HealthCheck(ctx, endpoint); err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("microdns-api/%s", net.Name),
				Status:  "fail",
				Message: fmt.Sprintf("REST API unreachable: %v", err),
			})
			// Skip further checks — API is down
			continue
		}
		items = append(items, CheckItem{
			Name:    fmt.Sprintf("microdns-api/%s", net.Name),
			Status:  "pass",
			Message: "REST API healthy",
			Details: endpoint,
		})

		// 2. DHCP pools (if DHCP is enabled for this network or relayed to it)
		if net.Spec.DHCP.Enabled || p.networkHasDHCP(net.Name) {
			pools, err := dnsClient.ListDHCPPools(ctx, endpoint)
			if err != nil {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("microdns-dhcp-pools/%s", net.Name),
					Status:  "warn",
					Message: fmt.Sprintf("cannot check DHCP pools: %v", err),
				})
			} else if len(pools) == 0 {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("microdns-dhcp-pools/%s", net.Name),
					Status:  "fail",
					Message: "no DHCP pools configured (database may be empty after restart)",
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("microdns-dhcp-pools/%s", net.Name),
					Status:  "pass",
					Message: fmt.Sprintf("%d DHCP pool(s) configured", len(pools)),
				})
			}

			// 3. DHCP reservations — verify count matches Network CRD
			reservations, err := dnsClient.ListDHCPReservations(ctx, endpoint)
			if err != nil {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("microdns-dhcp-reservations/%s", net.Name),
					Status:  "warn",
					Message: fmt.Sprintf("cannot check DHCP reservations: %v", err),
				})
			} else {
				// Count expected reservations from this network + relayed peers
				expected := len(net.Spec.DHCP.Reservations)
				for _, peer := range p.networks {
					if peer.Name != net.Name && peer.Spec.DHCP.Enabled && peer.Spec.DHCP.ServerNetwork == net.Name {
						expected += len(peer.Spec.DHCP.Reservations)
					}
				}

				status := "pass"
				msg := fmt.Sprintf("%d reservation(s) active", len(reservations))
				if expected > 0 && len(reservations) < expected {
					status = "warn"
					msg = fmt.Sprintf("%d/%d reservations (some missing)", len(reservations), expected)
				}
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("microdns-dhcp-reservations/%s", net.Name),
					Status:  status,
					Message: msg,
				})
			}
		}

		// 4. DNS forwarders match network topology
		forwarders, err := dnsClient.ListDNSForwarders(ctx, endpoint)
		if err != nil {
			continue
		}

		existingZones := make(map[string]bool, len(forwarders))
		for _, f := range forwarders {
			existingZones[f.Zone] = true
		}

		var missing []string
		for _, peer := range p.networks {
			if peer.Name == net.Name || peer.Spec.DNS.Zone == "" || peer.Spec.DNS.Server == "" {
				continue
			}
			if !existingZones[peer.Spec.DNS.Zone] {
				missing = append(missing, peer.Spec.DNS.Zone)
			}
		}

		if len(missing) > 0 {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("microdns-forwarders/%s", net.Name),
				Status:  "fail",
				Message: fmt.Sprintf("missing forwarders: %s", strings.Join(missing, ", ")),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("microdns-forwarders/%s", net.Name),
				Status:  "pass",
				Message: fmt.Sprintf("%d forwarder(s) configured", len(forwarders)),
			})
		}
	}

	return items
}

// checkSmokeTests reads the latest smoke test results for all networks
// and reports them as consistency check items.
func (p *MicroKubeProvider) checkSmokeTests() []CheckItem {
	var items []CheckItem

	results := GetSmokeTestResults()
	for _, r := range results {
		status := "pass"
		if !r.Pass {
			status = "fail"
		}

		// Stale results (>10 min) are a warning — smoke test may not have run recently
		age := time.Since(r.Timestamp)
		if age > 10*time.Minute {
			if r.Pass {
				status = "warn"
			}
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("smoketest/%s", r.Network),
				Status:  status,
				Message: r.Message,
				Details: fmt.Sprintf("age=%s (stale, >10m)", age.Truncate(time.Second)),
			})
		} else {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("smoketest/%s", r.Network),
				Status:  status,
				Message: r.Message,
				Details: fmt.Sprintf("age=%s", age.Truncate(time.Second)),
			})
		}
	}

	// Flag networks with no smoke test result at all
	for _, net := range p.networks {
		if net.Spec.ExternalDNS || net.Spec.DNS.Zone == "" || net.Spec.DNS.Server == "" {
			continue
		}
		found := false
		for _, r := range results {
			if r.Network == net.Name {
				found = true
				break
			}
		}
		if !found {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("smoketest/%s", net.Name),
				Status:  "warn",
				Message: "no smoke test result (not yet run)",
			})
		}
	}

	return items
}

// checkPodLiveness probes declared TCP ports on all tracked running pods.
// This catches the case where RouterOS reports a container as "running"
// but the process inside is dead or unresponsive.
func (p *MicroKubeProvider) checkPodLiveness(ctx context.Context) []CheckItem {
	var items []CheckItem

	for _, pod := range p.pods {
		for i, c := range pod.Spec.Containers {
			// Only check containers that declare TCP ports
			tcpPorts := collectTCPPorts(c)
			if len(tcpPorts) == 0 {
				continue
			}

			// Get pod IP from veth
			vn := vethName(pod, i)
			podIP, _, ok := p.deps.NetworkMgr.GetPortInfo(vn)
			if !ok || podIP == "" {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("liveness/%s/%s", pod.Name, c.Name),
					Status:  "warn",
					Message: "no IP allocated, cannot probe",
				})
				continue
			}

			// Verify container is running in RouterOS first
			rosName := sanitizeName(pod, c.Name)
			ct, err := p.deps.Runtime.GetContainer(ctx, rosName)
			if err != nil || ct == nil || !ct.IsRunning() {
				// Not running — other checks already report this
				continue
			}

			// Probe each declared TCP port
			allReachable := true
			var portResults []string
			for _, port := range tcpPorts {
				addr := fmt.Sprintf("%s:%d", podIP, port)
				conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					allReachable = false
					portResults = append(portResults, fmt.Sprintf("%d=FAIL", port))
				} else {
					conn.Close()
					portResults = append(portResults, fmt.Sprintf("%d=OK", port))
				}
			}

			if allReachable {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("liveness/%s/%s", pod.Name, c.Name),
					Status:  "pass",
					Message: "all ports reachable",
					Details: fmt.Sprintf("ip=%s ports=%s", podIP, strings.Join(portResults, ",")),
				})
			} else {
				items = append(items, CheckItem{
					Name:    fmt.Sprintf("liveness/%s/%s", pod.Name, c.Name),
					Status:  "fail",
					Message: "port(s) unreachable on running container",
					Details: fmt.Sprintf("ip=%s ports=%s", podIP, strings.Join(portResults, ",")),
				})
			}
		}
	}

	return items
}

// collectTCPPorts returns all TCP container ports from a container spec.
func collectTCPPorts(c corev1.Container) []int32 {
	var ports []int32
	for _, p := range c.Ports {
		if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
			ports = append(ports, p.ContainerPort)
		}
	}
	return ports
}
