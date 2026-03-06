package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	mkCmd           = flag.String("mk", "oc", "oc/mk command (default: oc)")
	kubeconfig      = flag.String("kubeconfig", os.ExpandEnv("$HOME/.kube/mkube.config"), "kubeconfig path")
	networkName     = flag.String("network", "gtest", "test network for container tests")
	dnsNetwork      = flag.String("dns-network", "gt", "existing network for DNS/DHCP tests (must have running microdns)")
	containerImage  = flag.String("image", "192.168.200.3:5000/microdns:edge", "container image for pod tests")
	containerCycles = flag.Int("container-cycles", 5, "number of container start/stop cycles")
	dhcpCycles      = flag.Int("dhcp-cycles", 100, "number of DHCP reservation CRUD cycles")
	dnsCycles       = flag.Int("dns-cycles", 100, "number of DNS record CRUD cycles")
	dnsStressRounds = flag.Int("dns-stress-rounds", 100, "number of DNS stress test rounds")
	dnsStressCount  = flag.Int("dns-stress-count", 100, "number of A records per DNS stress round")
	mkubeAPI        = flag.String("api", "http://192.168.200.2:8082", "mkube API base URL")
	skipSetup       = flag.Bool("skip-setup", false, "skip test network creation")
	skipTeardown    = flag.Bool("skip-teardown", false, "skip test network deletion at end")
	suite           = flag.String("suite", "all", "run specific suite: all, containers, dhcp, dns, pool, dns-stress")
)

type stats struct {
	count   int
	minMs   int64
	maxMs   int64
	totalMs int64
}

func (s *stats) record(ms int64) {
	s.count++
	s.totalMs += ms
	if s.count == 1 || ms < s.minMs {
		s.minMs = ms
	}
	if ms > s.maxMs {
		s.maxMs = ms
	}
}

func (s *stats) avgMs() int64 {
	if s.count == 0 {
		return 0
	}
	return s.totalMs / int64(s.count)
}

func (s *stats) summary() string {
	if s.count == 0 {
		return "no data"
	}
	return fmt.Sprintf("min=%dms max=%dms avg=%dms", s.minMs, s.maxMs, s.avgMs())
}

func main() {
	flag.Parse()

	fmt.Println("=== mkube Integration Test ===")
	fmt.Printf("Container network: %s\n", *networkName)
	fmt.Printf("DNS/DHCP network:  %s\n", *dnsNetwork)
	fmt.Printf("Command: %s\n\n", *mkCmd)

	// Verify mk command works
	out, err := mk("get", "nodes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: mk command failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	fmt.Printf("Cluster nodes:\n%s\n", out)

	// Always clean up test network on exit (even on panic/failure)
	if !*skipTeardown {
		defer func() {
			fmt.Printf("\nTearing down test network %s...\n", *networkName)
			// Delete all test pods first (network delete is protected if pods reference it)
			cleanupTestPods()
			time.Sleep(2 * time.Second)
			out, err := mk("delete", "network", *networkName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: teardown failed: %v\n%s\n", err, out)
			} else {
				fmt.Println("Network deleted.")
			}
		}()
	}

	// Setup: create test network
	if !*skipSetup {
		fmt.Printf("Setting up test network %s...\n", *networkName)
		// Delete stale network from previous interrupted run
		_, _ = mk("delete", "network", *networkName)
		time.Sleep(3 * time.Second)

		if err := createTestNetwork(); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: failed to create test network: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Network %s created. Waiting 15s for DNS pod to start...\n", *networkName)
		time.Sleep(15 * time.Second)

		// Verify DNS pod is running
		out, _ := mk("get", "pods", "-n", *networkName)
		fmt.Printf("Test network pods:\n%s\n", out)
	}

	allPassed := true
	runAll := *suite == "all"

	if runAll || *suite == "containers" {
		if !runContainerSuite() {
			allPassed = false
		}
	}

	if runAll || *suite == "dhcp" {
		if !runDHCPSuite() {
			allPassed = false
		}
	}

	if runAll || *suite == "dns" {
		if !runDNSSuite() {
			allPassed = false
		}
	}

	if runAll || *suite == "pool" {
		if !runPoolSuite() {
			allPassed = false
		}
	}

	if runAll || *suite == "dns-stress" {
		if !runDNSStressSuite() {
			allPassed = false
		}
	}

	fmt.Println()
	if allPassed {
		fmt.Println("=== RESULT: ALL PASSED ===")
	} else {
		fmt.Println("=== RESULT: SOME FAILURES ===")
		os.Exit(1)
	}
}

// ─── mk command helpers ─────────────────────────────────────────────────────

func mk(args ...string) (string, error) {
	cmd := exec.Command(*mkCmd, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+*kubeconfig)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mkApply(yaml string) (string, error) {
	cmd := exec.Command(*mkCmd, "apply", "-f", "-")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+*kubeconfig)
	cmd.Stdin = strings.NewReader(yaml)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mkGetJSON(args ...string) (map[string]interface{}, error) {
	fullArgs := append(args, "-o", "json")
	out, err := mk(fullArgs...)
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("json parse: %v (output: %s)", err, truncate(out, 200))
	}
	return result, nil
}

// ─── Network Setup ──────────────────────────────────────────────────────────

func createTestNetwork() error {
	yaml := fmt.Sprintf(`apiVersion: v1
kind: Network
metadata:
  name: %s
spec:
  cidr: "192.168.99.0/24"
  gateway: "192.168.99.1"
  type: data
  managed: true
  dns:
    zone: "%s.lo"
    server: "192.168.99.252"
  dhcp:
    enabled: true
    rangeStart: "192.168.99.100"
    rangeEnd: "192.168.99.199"
    leaseTime: 60
  ipam:
    start: "192.168.99.200"
    end: "192.168.99.250"
`, *networkName, *networkName)

	out, err := mkApply(yaml)
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	fmt.Printf("  %s\n", out)
	return nil
}

func cleanupTestPods() {
	out, err := mk("get", "pods", "-n", *networkName, "-o", "json")
	if err != nil {
		return
	}
	var list map[string]interface{}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return
	}
	items, _ := list["items"].([]interface{})
	for _, item := range items {
		pod, _ := item.(map[string]interface{})
		meta, _ := pod["metadata"].(map[string]interface{})
		if meta == nil {
			continue
		}
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if strings.HasPrefix(name, "test-pod-") {
			mk("delete", "pod", name, "-n", ns)
		}
	}
}

// ─── Suite 1: Container Start/Stop ──────────────────────────────────────────

func runContainerSuite() bool {
	n := *containerCycles
	fmt.Printf("--- Suite 1: Container Start/Stop (%d cycles) ---\n", n)

	createSt := &stats{}
	deleteSt := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		podName := fmt.Sprintf("test-pod-%d", i)
		ns := *networkName

		// Create pod via mk apply
		createStart := time.Now()
		podYAML := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  annotations:
    vkube.io/network: "%s"
spec:
  restartPolicy: Always
  containers:
  - name: test
    image: "%s"
`, podName, ns, *networkName, *containerImage)

		out, err := mkApply(podYAML)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v (%s)\n", i, n, err, out)
			continue
		}

		// Wait for pod Running
		if err := waitForPod(ns, podName, "Running", 120*time.Second); err != nil {
			createMs := time.Since(createStart).Milliseconds()
			fmt.Printf("  cycle %3d/%d: create=%dms TIMEOUT: %v\n", i, n, createMs, err)
			// Show pod status for debugging
			status, _ := mk("get", "pod", podName, "-n", ns)
			fmt.Printf("    pod status: %s\n", status)
			_, _ = mk("delete", "pod", podName, "-n", ns)
			waitForPodGone(ns, podName, 30*time.Second)
			continue
		}
		createMs := time.Since(createStart).Milliseconds()
		createSt.record(createMs)

		// Delete pod
		deleteStart := time.Now()
		out, err = mk("delete", "pod", podName, "-n", ns)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms DELETE FAILED: %v (%s)\n", i, n, createMs, err, out)
			continue
		}

		// Wait for pod gone
		if err := waitForPodGone(ns, podName, 30*time.Second); err != nil {
			deleteMs := time.Since(deleteStart).Milliseconds()
			fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms GONE_TIMEOUT\n", i, n, createMs, deleteMs)
			continue
		}
		deleteMs := time.Since(deleteStart).Milliseconds()
		deleteSt.record(deleteMs)

		passed++
		fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms PASS\n", i, n, createMs, deleteMs)
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if createSt.count > 0 {
		fmt.Printf("    create: %s\n", createSt.summary())
	}
	if deleteSt.count > 0 {
		fmt.Printf("    delete: %s\n", deleteSt.summary())
	}
	fmt.Println()
	return passed == n
}

// ─── Suite 2: DHCP Reservations ─────────────────────────────────────────────

func runDHCPSuite() bool {
	n := *dhcpCycles
	ns := *dnsNetwork
	fmt.Printf("--- Suite 2: DHCP Reservations (%d cycles on %s) ---\n", n, ns)

	createSt := &stats{}
	deleteSt := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		mac := fmt.Sprintf("02:00:00:00:%02x:%02x", i/256, i%256)
		ip := fmt.Sprintf("10.99.0.%d", 1+(i%254))
		hostname := fmt.Sprintf("test-host-%d", i)
		macDash := strings.ReplaceAll(mac, ":", "-")
		cycleFailed := false

		// Create reservation via mk apply
		createStart := time.Now()
		resYAML := fmt.Sprintf(`apiVersion: v1
kind: DHCPReservation
metadata:
  name: %s
  namespace: %s
spec:
  mac: "%s"
  ip: "%s"
  hostname: "%s"
`, macDash, ns, mac, ip, hostname)

		out, err := mkApply(resYAML)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v (%s)\n", i, n, err, truncate(out, 100))
			continue
		}
		createMs := time.Since(createStart).Milliseconds()
		createSt.record(createMs)

		// Verify exists
		_, err = mk("get", "dhcpr", macDash, "-n", ns)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms VERIFY FAILED\n", i, n, createMs)
			mk("delete", "dhcpr", macDash, "-n", ns)
			continue
		}

		// Update hostname via mk apply (PATCH)
		updateYAML := fmt.Sprintf(`apiVersion: v1
kind: DHCPReservation
metadata:
  name: %s
  namespace: %s
spec:
  mac: "%s"
  ip: "%s"
  hostname: "%s-updated"
`, macDash, ns, mac, ip, hostname)
		upOut, upErr := mkApply(updateYAML)
		if upErr != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms UPDATE FAILED: %s\n", i, n, createMs, truncate(upOut, 100))
			cycleFailed = true
		}

		// Delete
		deleteStart := time.Now()
		_, err = mk("delete", "dhcpr", macDash, "-n", ns)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms DELETE FAILED\n", i, n, createMs)
			continue
		}
		deleteMs := time.Since(deleteStart).Milliseconds()
		deleteSt.record(deleteMs)

		if cycleFailed {
			continue
		}

		passed++
		if i%10 == 0 || i == n || i <= 3 {
			fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms PASS\n", i, n, createMs, deleteMs)
		}
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if createSt.count > 0 {
		fmt.Printf("    create: %s\n", createSt.summary())
	}
	if deleteSt.count > 0 {
		fmt.Printf("    delete: %s\n", deleteSt.summary())
	}
	fmt.Println()
	return passed == n
}

// ─── Suite 3: DNS Records ───────────────────────────────────────────────────

func runDNSSuite() bool {
	n := *dnsCycles
	ns := *dnsNetwork
	fmt.Printf("--- Suite 3: DNS Records (%d cycles on %s) ---\n", n, ns)

	crudSt := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		hostname := fmt.Sprintf("test-dns-%d", i)
		ip1 := fmt.Sprintf("10.99.1.%d", 1+(i%254))
		ip2 := fmt.Sprintf("10.99.2.%d", 1+(i%254))
		cycleFailed := false

		cycleStart := time.Now()

		// Create A record via mk apply
		recYAML := fmt.Sprintf(`apiVersion: v1
kind: DNSRecord
metadata:
  name: "%s"
  namespace: %s
spec:
  hostname: "%s"
  type: A
  data: "%s"
  ttl: 60
`, hostname, ns, hostname, ip1)

		out, err := mkApply(recYAML)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v (%s)\n", i, n, err, truncate(out, 100))
			continue
		}

		// Get the record to find the real name (microdns UUID)
		obj, err := mkGetJSON("get", "dr", "-n", ns)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: LIST FAILED: %v\n", i, n, err)
			continue
		}

		// Find the record we just created by hostname
		recordID := findDNSRecordByHostname(obj, hostname)
		if recordID == "" {
			fmt.Printf("  cycle %3d/%d: RECORD NOT FOUND after create\n", i, n)
			continue
		}

		// Update IP via mk apply (PATCH — use record UUID as name)
		updateYAML := fmt.Sprintf(`apiVersion: v1
kind: DNSRecord
metadata:
  name: "%s"
  namespace: %s
spec:
  hostname: "%s"
  type: A
  data: "%s"
  ttl: 120
`, recordID, ns, hostname, ip2)

		upOut, upErr := mkApply(updateYAML)
		if upErr != nil {
			fmt.Printf("  cycle %3d/%d: UPDATE FAILED: %s\n", i, n, truncate(upOut, 100))
			cycleFailed = true
		}

		// Delete by ID
		_, err = mk("delete", "dr", recordID, "-n", ns)
		if err != nil {
			fmt.Printf("  cycle %3d/%d: DELETE FAILED\n", i, n)
			continue
		}

		if cycleFailed {
			continue
		}

		crudMs := time.Since(cycleStart).Milliseconds()
		crudSt.record(crudMs)
		passed++

		if i%10 == 0 || i == n || i <= 3 {
			fmt.Printf("  cycle %3d/%d: crud=%dms PASS\n", i, n, crudMs)
		}
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if crudSt.count > 0 {
		fmt.Printf("    crud: %s\n", crudSt.summary())
	}
	fmt.Println()
	return passed == n
}

func findDNSRecordByHostname(obj map[string]interface{}, hostname string) string {
	items, _ := obj["items"].([]interface{})
	for _, item := range items {
		rec, _ := item.(map[string]interface{})
		spec, _ := rec["spec"].(map[string]interface{})
		meta, _ := rec["metadata"].(map[string]interface{})
		if spec == nil || meta == nil {
			continue
		}
		h, _ := spec["hostname"].(string)
		if h == hostname {
			name, _ := meta["name"].(string)
			return name
		}
	}
	return ""
}

// ─── Suite 4: DHCP Pool CRUD ────────────────────────────────────────────────

func runPoolSuite() bool {
	ns := *dnsNetwork
	fmt.Printf("--- Suite 4: DHCP Pool CRUD (on %s) ---\n", ns)

	// List pools
	out, err := mk("get", "dp", "-n", ns)
	if err != nil {
		fmt.Printf("  List pools FAILED: %v (%s)\n", err, out)
		return false
	}
	fmt.Printf("  Pools:\n%s\n", out)

	// Get pools as JSON to find pool ID
	obj, err := mkGetJSON("get", "dp", "-n", ns)
	if err != nil {
		fmt.Printf("  List pools JSON FAILED: %v\n", err)
		return false
	}

	items, _ := obj["items"].([]interface{})
	if len(items) == 0 {
		fmt.Println("  No pools found. SKIP")
		return true
	}

	firstPool, _ := items[0].(map[string]interface{})
	meta, _ := firstPool["metadata"].(map[string]interface{})
	poolID, _ := meta["name"].(string)
	if poolID == "" {
		fmt.Println("  Could not extract pool ID. FAIL")
		return false
	}

	// Get single pool
	_, err = mk("get", "dp", poolID, "-n", ns)
	if err != nil {
		fmt.Printf("  GET pool %s FAILED\n", poolID)
		return false
	}
	fmt.Printf("  GET pool %s: PASS\n", poolID)

	fmt.Println("  Summary: DHCP Pool CRUD PASSED")
	fmt.Println()
	return true
}

// ─── Wait Helpers ───────────────────────────────────────────────────────────

func waitForPod(ns, name, phase string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s/%s to be %s", ns, name, phase)
		case <-ticker.C:
			obj, err := mkGetJSON("get", "pod", name, "-n", ns)
			if err != nil {
				continue
			}
			status, _ := obj["status"].(map[string]interface{})
			if status != nil {
				p, _ := status["phase"].(string)
				if p == phase {
					return nil
				}
			}
		}
	}
}

func waitForPodGone(ns, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s/%s to be deleted", ns, name)
		case <-ticker.C:
			_, err := mk("get", "pod", name, "-n", ns)
			if err != nil {
				return nil // gone
			}
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ─── Suite 5: DNS Stress Test ───────────────────────────────────────────────
// Creates N A records, verifies all via lookup, deletes all. Repeats M rounds.
// Monitors system memory between rounds to detect leaks.

func runDNSStressSuite() bool {
	rounds := *dnsStressRounds
	count := *dnsStressCount
	ns := *dnsNetwork
	fmt.Printf("--- Suite 5: DNS Stress Test (%d rounds x %d records on %s) ---\n", rounds, count, ns)

	// Get baseline memory
	baseMem := getSystemMemory()
	if baseMem != nil {
		fmt.Printf("  Baseline: %s\n", baseMem)
	}

	roundSt := &stats{}
	passed := 0

	for round := 1; round <= rounds; round++ {
		roundStart := time.Now()
		roundFailed := false
		prefix := fmt.Sprintf("stress-r%d", round)

		// Phase 1: Create N A records
		createStart := time.Now()
		for i := 1; i <= count; i++ {
			hostname := fmt.Sprintf("%s-h%d", prefix, i)
			ip := fmt.Sprintf("10.99.%d.%d", (i/254)+10, 1+(i%254))
			recYAML := fmt.Sprintf(`apiVersion: v1
kind: DNSRecord
metadata:
  name: "%s"
  namespace: %s
spec:
  hostname: "%s"
  type: A
  data: "%s"
  ttl: 60
`, hostname, ns, hostname, ip)
			out, err := mkApply(recYAML)
			if err != nil {
				fmt.Printf("  round %3d/%d: CREATE %d FAILED: %s\n", round, rounds, i, truncate(out, 100))
				roundFailed = true
				break
			}
		}
		createMs := time.Since(createStart).Milliseconds()
		if roundFailed {
			cleanupStressRecords(ns, prefix, count)
			continue
		}

		// Phase 2: Verify all N records exist via oc get
		verifyStart := time.Now()
		obj, err := mkGetJSON("get", "dr", "-n", ns)
		if err != nil {
			fmt.Printf("  round %3d/%d: LIST FAILED: %v\n", round, rounds, err)
			cleanupStressRecords(ns, prefix, count)
			continue
		}
		foundCount := 0
		for i := 1; i <= count; i++ {
			hostname := fmt.Sprintf("%s-h%d", prefix, i)
			if findDNSRecordByHostname(obj, hostname) != "" {
				foundCount++
			}
		}
		verifyMs := time.Since(verifyStart).Milliseconds()
		if foundCount != count {
			fmt.Printf("  round %3d/%d: VERIFY FAILED: found %d/%d records\n", round, rounds, foundCount, count)
			roundFailed = true
		}

		// Phase 3: Delete all N records
		deleteStart := time.Now()
		cleanupStressRecords(ns, prefix, count)
		deleteMs := time.Since(deleteStart).Milliseconds()

		roundMs := time.Since(roundStart).Milliseconds()

		if roundFailed {
			continue
		}

		roundSt.record(roundMs)
		passed++

		if round%10 == 0 || round == rounds || round <= 3 {
			fmt.Printf("  round %3d/%d: create=%dms verify=%dms delete=%dms total=%dms PASS\n",
				round, rounds, createMs, verifyMs, deleteMs, roundMs)
		}

		// Check memory every 10 rounds
		if round%10 == 0 {
			mem := getSystemMemory()
			if mem != nil {
				fmt.Printf("    memory: %s\n", mem)
			}
		}
	}

	// Final memory check and leak detection
	finalMem := getSystemMemory()
	if finalMem != nil && baseMem != nil {
		drift := baseMem.FreeMB - finalMem.FreeMB
		fmt.Printf("  Memory: baseline %s\n", baseMem)
		fmt.Printf("  Memory: final    %s\n", finalMem)
		if drift > 50 {
			fmt.Printf("  WARNING: memory drift %dMB — possible leak\n", drift)
		} else {
			fmt.Printf("  Memory drift: %dMB (OK)\n", drift)
		}
	}

	fmt.Printf("  Summary: %d/%d rounds passed\n", passed, rounds)
	if roundSt.count > 0 {
		fmt.Printf("    round: %s\n", roundSt.summary())
	}
	fmt.Println()
	return passed == rounds
}

func cleanupStressRecords(ns, prefix string, count int) {
	// List all records and delete matching ones
	obj, err := mkGetJSON("get", "dr", "-n", ns)
	if err != nil {
		return
	}
	items, _ := obj["items"].([]interface{})
	for _, item := range items {
		rec, _ := item.(map[string]interface{})
		spec, _ := rec["spec"].(map[string]interface{})
		meta, _ := rec["metadata"].(map[string]interface{})
		if spec == nil || meta == nil {
			continue
		}
		h, _ := spec["hostname"].(string)
		if strings.HasPrefix(h, prefix+"-h") {
			name, _ := meta["name"].(string)
			if name != "" {
				mk("delete", "dr", name, "-n", ns)
			}
		}
	}
}

// ─── System Memory Helpers ──────────────────────────────────────────────────

type memInfo struct {
	FreeMB  int64
	TotalMB int64
}

func (m *memInfo) String() string {
	if m == nil {
		return "unavailable"
	}
	usedMB := m.TotalMB - m.FreeMB
	return fmt.Sprintf("free=%dMB used=%dMB total=%dMB", m.FreeMB, usedMB, m.TotalMB)
}

func getSystemMemory() *memInfo {
	resp, err := http.Get(*mkubeAPI + "/api/v1/nodes")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil
	}
	items, _ := result["items"].([]interface{})
	if len(items) == 0 {
		return nil
	}
	node, _ := items[0].(map[string]interface{})
	status, _ := node["status"].(map[string]interface{})
	if status == nil {
		return nil
	}
	capacity, _ := status["capacity"].(map[string]interface{})
	allocatable, _ := status["allocatable"].(map[string]interface{})
	if capacity == nil || allocatable == nil {
		return nil
	}
	totalStr, _ := capacity["memory"].(string)
	freeStr, _ := allocatable["memory"].(string)

	return &memInfo{
		FreeMB:  parseMemMB(freeStr),
		TotalMB: parseMemMB(totalStr),
	}
}

func parseMemMB(s string) int64 {
	// Node API returns bytes as plain number string
	s = strings.TrimSpace(s)
	var val int64
	fmt.Sscanf(s, "%d", &val)
	return val / (1024 * 1024)
}
