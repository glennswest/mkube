package provider

import (
	"context"
	"fmt"
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

// ConsistencyChecks groups the four check categories.
type ConsistencyChecks struct {
	Containers []CheckItem `json:"containers"`
	DNS        []CheckItem `json:"dns"`
	Manifest   []CheckItem `json:"manifest"`
	IPAM       []CheckItem `json:"ipam"`
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

	for _, items := range [][]CheckItem{
		report.Checks.Containers,
		report.Checks.DNS,
		report.Checks.Manifest,
		report.Checks.IPAM,
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
				items = append(items, CheckItem{
					Name:    checkName,
					Status:  "fail",
					Message: fmt.Sprintf("container exists but status is %q", ct.Status),
					Details: fmt.Sprintf("id=%s", ct.ID),
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

// checkDNS verifies DNS records match expected state from the manifest.
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

	manifestPath := p.deps.Config.Lifecycle.BootManifestPath
	if manifestPath == "" {
		return nil
	}

	pods, _, err := loadManifests(manifestPath)
	if err != nil {
		return []CheckItem{{
			Name:    "manifest-load",
			Status:  "fail",
			Message: fmt.Sprintf("failed to load manifest for DNS check: %v", err),
		}}
	}

	// Check each network that has DNS configured
	for _, netName := range p.deps.NetworkMgr.Networks() {
		netDef, ok := p.deps.NetworkMgr.NetworkDef(netName)
		if !ok || netDef.DNS.Endpoint == "" || netDef.DNS.Zone == "" {
			continue
		}

		zoneID, ok := p.deps.NetworkMgr.NetworkZoneID(netName)
		if !ok {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns-zone/%s", netName),
				Status:  "warn",
				Message: "zone ID not cached (DNS may not be initialized)",
			})
			continue
		}

		records, err := dnsClient.ListRecords(ctx, netDef.DNS.Endpoint, zoneID)
		if err != nil {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns-zone/%s", netName),
				Status:  "fail",
				Message: fmt.Sprintf("failed to list records: %v", err),
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

		// Build expected records from manifest
		expectedRecords := p.buildExpectedDNSRecords(pods, netName)

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

		// Any remaining actual records are stale
		for hostname, ips := range actualRecords {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("dns/%s/%s", netName, hostname),
				Status:  "warn",
				Message: "stale DNS record (not in manifest)",
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

	// Tracked pods not in manifest
	for key := range p.pods {
		if !manifestSet[key] {
			items = append(items, CheckItem{
				Name:    fmt.Sprintf("manifest/%s", key),
				Status:  "warn",
				Message: "tracked pod not in boot manifest",
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

// CheckConsistencyAsync runs a consistency check in the background after
// container operations. It detects and cleans up orphaned veths and IPAM entries.
// The delay gives the system time to settle after the triggering operation.
func (p *MicroKubeProvider) CheckConsistencyAsync(reason string) {
	go func() {
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

	for _, pod := range p.pods {
		for i := range pod.Spec.Containers {
			expectedVeths[vethName(pod, i)] = true
		}
	}

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

	// Source 1: tracked pods
	for _, pod := range p.pods {
		for _, c := range pod.Spec.Containers {
			expectedContainers[sanitizeName(pod, c.Name)] = true
		}
	}

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

