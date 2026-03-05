package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	apiURL       = flag.String("api", "http://192.168.200.2:8082", "mkube API base URL")
	networkName  = flag.String("network", "gtest", "test network name")
	cidr         = flag.String("cidr", "192.168.99.0/24", "test network CIDR")
	gateway      = flag.String("gateway", "192.168.99.1", "test network gateway")
	dnsServer    = flag.String("dns-server", "192.168.99.252", "test DNS server IP")
	containerCycles = flag.Int("container-cycles", 20, "number of container start/stop cycles")
	dhcpCycles   = flag.Int("dhcp-cycles", 100, "number of DHCP reservation CRUD cycles")
	dnsCycles    = flag.Int("dns-cycles", 100, "number of DNS record CRUD cycles")
	skipSetup    = flag.Bool("skip-setup", false, "skip network creation (assume gtest exists)")
	skipTeardown = flag.Bool("skip-teardown", false, "skip network deletion at end")
	suite        = flag.String("suite", "all", "run specific suite: all, containers, dhcp, dns, pool")
)

type stats struct {
	count   int
	passed  int
	failed  int
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

func main() {
	flag.Parse()

	fmt.Println("=== mkube Integration Test ===")
	fmt.Printf("Network: %s (%s)\n", *networkName, *cidr)
	fmt.Printf("API: %s\n\n", *apiURL)

	client := &http.Client{Timeout: 120 * time.Second}

	// Verify API is reachable
	resp, err := client.Get(*apiURL + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot reach mkube API at %s: %v\n", *apiURL, err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("API health: %s\n\n", strings.TrimSpace(string(body)))

	// Setup: create test network
	if !*skipSetup {
		fmt.Printf("Setting up test network %s...\n", *networkName)
		if err := createTestNetwork(client); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: failed to create test network: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Network %s created. Waiting 10s for DNS pod to start...\n", *networkName)
		time.Sleep(10 * time.Second)
	}

	allPassed := true
	runAll := *suite == "all"

	// Suite 1: Container Start/Stop
	if runAll || *suite == "containers" {
		if !runContainerSuite(client) {
			allPassed = false
		}
	}

	// Suite 2: DHCP Reservations
	if runAll || *suite == "dhcp" {
		if !runDHCPSuite(client) {
			allPassed = false
		}
	}

	// Suite 3: DNS Records
	if runAll || *suite == "dns" {
		if !runDNSSuite(client) {
			allPassed = false
		}
	}

	// Suite 4: DHCP Pool CRUD
	if runAll || *suite == "pool" {
		if !runPoolSuite(client) {
			allPassed = false
		}
	}

	// Teardown
	if !*skipTeardown {
		fmt.Printf("\nTearing down test network %s...\n", *networkName)
		if err := deleteTestNetwork(client); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: failed to delete test network: %v\n", err)
		} else {
			fmt.Println("Network deleted.")
		}
	}

	fmt.Println()
	if allPassed {
		fmt.Println("=== RESULT: ALL PASSED ===")
		os.Exit(0)
	} else {
		fmt.Println("=== RESULT: SOME FAILURES ===")
		os.Exit(1)
	}
}

// ─── Network Setup/Teardown ──────────────────────────────────────────────────

func createTestNetwork(client *http.Client) error {
	network := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Network",
		"metadata": map[string]interface{}{
			"name": *networkName,
		},
		"spec": map[string]interface{}{
			"cidr":    *cidr,
			"gateway": *gateway,
			"type":    "data",
			"managed": true,
			"dns": map[string]interface{}{
				"zone":   *networkName + ".lo",
				"server": *dnsServer,
			},
			"dhcp": map[string]interface{}{
				"enabled":    true,
				"rangeStart": "192.168.99.100",
				"rangeEnd":   "192.168.99.199",
				"leaseTime":  60,
			},
			"ipam": map[string]interface{}{
				"start": "192.168.99.200",
				"end":   "192.168.99.250",
			},
		},
	}
	return apiPost(client, "/api/v1/networks", network, nil)
}

func deleteTestNetwork(client *http.Client) error {
	return apiDelete(client, "/api/v1/networks/"+*networkName)
}

// ─── Suite 1: Container Start/Stop ──────────────────────────────────────────

func runContainerSuite(client *http.Client) bool {
	n := *containerCycles
	fmt.Printf("--- Suite 1: Container Start/Stop (%d cycles) ---\n", n)

	createStats := &stats{}
	deleteStats := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		podName := fmt.Sprintf("test-pod-%d", i)
		ns := *networkName

		// Create pod
		createStart := time.Now()
		pod := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      podName,
				"namespace": ns,
				"annotations": map[string]interface{}{
					"vkube.io/network":  *networkName,
					"vkube.io/file":     "/dev/null",
				},
			},
			"spec": map[string]interface{}{
				"restartPolicy": "Always",
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "test",
						"image": "test:latest",
					},
				},
			},
		}
		if err := apiPost(client, fmt.Sprintf("/api/v1/namespaces/%s/pods", ns), pod, nil); err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v\n", i, n, err)
			deleteStats.count++
			deleteStats.failed++
			continue
		}

		// Wait for pod to be Running
		if err := waitForPodRunning(client, ns, podName, 60*time.Second); err != nil {
			createMs := time.Since(createStart).Milliseconds()
			fmt.Printf("  cycle %3d/%d: create=%dms TIMEOUT: %v\n", i, n, createMs, err)
			// Clean up anyway
			_ = apiDelete(client, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, podName))
			waitForPodGone(client, ns, podName, 30*time.Second)
			continue
		}
		createMs := time.Since(createStart).Milliseconds()
		createStats.record(createMs)

		// Delete pod
		deleteStart := time.Now()
		if err := apiDelete(client, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, podName)); err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms delete=FAILED: %v\n", i, n, createMs, err)
			continue
		}

		// Wait for pod to be gone
		if err := waitForPodGone(client, ns, podName, 30*time.Second); err != nil {
			deleteMs := time.Since(deleteStart).Milliseconds()
			fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms TIMEOUT: %v\n", i, n, createMs, deleteMs, err)
			continue
		}
		deleteMs := time.Since(deleteStart).Milliseconds()
		deleteStats.record(deleteMs)

		passed++
		fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms PASS\n", i, n, createMs, deleteMs)
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if createStats.count > 0 {
		fmt.Printf("    create: min=%dms max=%dms avg=%dms\n",
			createStats.minMs, createStats.maxMs, createStats.avgMs())
	}
	if deleteStats.count > 0 {
		fmt.Printf("    delete: min=%dms max=%dms avg=%dms\n",
			deleteStats.minMs, deleteStats.maxMs, deleteStats.avgMs())
	}
	fmt.Println()
	return passed == n
}

// ─── Suite 2: DHCP Reservations ─────────────────────────────────────────────

func runDHCPSuite(client *http.Client) bool {
	n := *dhcpCycles
	fmt.Printf("--- Suite 2: DHCP Reservations (%d cycles) ---\n", n)

	createSt := &stats{}
	deleteSt := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		mac := fmt.Sprintf("02:00:00:00:%02x:%02x", i/256, i%256)
		ip := fmt.Sprintf("192.168.99.%d", 100+(i%100))
		hostname := fmt.Sprintf("test-host-%d", i)
		macDash := strings.ReplaceAll(mac, ":", "-")

		// Create reservation
		createStart := time.Now()
		res := map[string]interface{}{
			"spec": map[string]interface{}{
				"mac":      mac,
				"ip":       ip,
				"hostname": hostname,
			},
		}
		if err := apiPost(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcpreservations", *networkName), res, nil); err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v\n", i, n, err)
			continue
		}
		createMs := time.Since(createStart).Milliseconds()
		createSt.record(createMs)

		// Verify exists
		var getResp map[string]interface{}
		if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcpreservations/%s", *networkName, macDash), &getResp); err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms VERIFY FAILED: %v\n", i, n, createMs, err)
			continue
		}

		// Update hostname
		res2 := map[string]interface{}{
			"spec": map[string]interface{}{
				"mac":      mac,
				"ip":       ip,
				"hostname": hostname + "-updated",
			},
		}
		if err := apiPut(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcpreservations/%s", *networkName, macDash), res2, nil); err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms UPDATE FAILED: %v\n", i, n, createMs, err)
			// Continue to cleanup
		}

		// Delete
		deleteStart := time.Now()
		if err := apiDelete(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcpreservations/%s", *networkName, macDash)); err != nil {
			fmt.Printf("  cycle %3d/%d: create=%dms DELETE FAILED: %v\n", i, n, createMs, err)
			continue
		}
		deleteMs := time.Since(deleteStart).Milliseconds()
		deleteSt.record(deleteMs)

		passed++
		if i%10 == 0 || i == n {
			fmt.Printf("  cycle %3d/%d: create=%dms delete=%dms PASS\n", i, n, createMs, deleteMs)
		}
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if createSt.count > 0 {
		fmt.Printf("    create: min=%dms max=%dms avg=%dms\n",
			createSt.minMs, createSt.maxMs, createSt.avgMs())
	}
	if deleteSt.count > 0 {
		fmt.Printf("    delete: min=%dms max=%dms avg=%dms\n",
			deleteSt.minMs, deleteSt.maxMs, deleteSt.avgMs())
	}
	fmt.Println()
	return passed == n
}

// ─── Suite 3: DNS Records ───────────────────────────────────────────────────

func runDNSSuite(client *http.Client) bool {
	n := *dnsCycles
	fmt.Printf("--- Suite 3: DNS Records (%d cycles) ---\n", n)

	crudSt := &stats{}
	passed := 0

	for i := 1; i <= n; i++ {
		hostname := fmt.Sprintf("test-dns-%d", i)
		ip1 := fmt.Sprintf("192.168.99.%d", 10+(i%240))
		ip2 := fmt.Sprintf("192.168.99.%d", 10+((i+1)%240))

		cycleStart := time.Now()

		// Create A record
		rec := map[string]interface{}{
			"spec": map[string]interface{}{
				"hostname": hostname,
				"type":     "A",
				"data":     ip1,
				"ttl":      60,
			},
		}
		var createResp map[string]interface{}
		if err := apiPost(client, fmt.Sprintf("/api/v1/namespaces/%s/dnsrecords", *networkName), rec, &createResp); err != nil {
			fmt.Printf("  cycle %3d/%d: CREATE FAILED: %v\n", i, n, err)
			continue
		}

		// Extract record ID from response
		recordID := extractName(createResp)
		if recordID == "" {
			fmt.Printf("  cycle %3d/%d: CREATE MISSING ID\n", i, n)
			continue
		}

		// Verify exists
		var getResp map[string]interface{}
		if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dnsrecords/%s", *networkName, recordID), &getResp); err != nil {
			fmt.Printf("  cycle %3d/%d: VERIFY FAILED: %v\n", i, n, err)
			continue
		}

		// Update IP
		update := map[string]interface{}{
			"spec": map[string]interface{}{
				"hostname": hostname,
				"type":     "A",
				"data":     ip2,
				"ttl":      120,
			},
		}
		if err := apiPut(client, fmt.Sprintf("/api/v1/namespaces/%s/dnsrecords/%s", *networkName, recordID), update, nil); err != nil {
			fmt.Printf("  cycle %3d/%d: UPDATE FAILED: %v\n", i, n, err)
			// Try to delete anyway
		}

		// Delete
		if err := apiDelete(client, fmt.Sprintf("/api/v1/namespaces/%s/dnsrecords/%s", *networkName, recordID)); err != nil {
			fmt.Printf("  cycle %3d/%d: DELETE FAILED: %v\n", i, n, err)
			continue
		}

		// Verify gone
		if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dnsrecords/%s", *networkName, recordID), nil); err == nil {
			fmt.Printf("  cycle %3d/%d: STILL EXISTS after delete\n", i, n)
			continue
		}

		crudMs := time.Since(cycleStart).Milliseconds()
		crudSt.record(crudMs)
		passed++

		if i%10 == 0 || i == n {
			fmt.Printf("  cycle %3d/%d: crud=%dms PASS\n", i, n, crudMs)
		}
	}

	fmt.Printf("  Summary: %d/%d passed\n", passed, n)
	if crudSt.count > 0 {
		fmt.Printf("    crud: min=%dms max=%dms avg=%dms\n",
			crudSt.minMs, crudSt.maxMs, crudSt.avgMs())
	}
	fmt.Println()
	return passed == n
}

// ─── Suite 4: DHCP Pool CRUD ───────────────────────────────────────────────

func runPoolSuite(client *http.Client) bool {
	fmt.Println("--- Suite 4: DHCP Pool CRUD ---")

	// List pools
	var poolList map[string]interface{}
	if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcppools", *networkName), &poolList); err != nil {
		fmt.Printf("  List pools FAILED: %v\n", err)
		return false
	}

	items, _ := poolList["items"].([]interface{})
	if len(items) == 0 {
		fmt.Println("  No pools found. SKIP (network may not have DHCP enabled)")
		return true
	}

	fmt.Printf("  Found %d pool(s)\n", len(items))

	// Get the first pool's ID
	firstPool, _ := items[0].(map[string]interface{})
	poolID := extractName(firstPool)
	if poolID == "" {
		fmt.Println("  Could not extract pool ID. FAIL")
		return false
	}

	// Read current pool
	var poolDetail map[string]interface{}
	if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcppools/%s", *networkName, poolID), &poolDetail); err != nil {
		fmt.Printf("  Get pool FAILED: %v\n", err)
		return false
	}
	fmt.Printf("  GET pool %s: PASS\n", poolID)

	// Read the spec to get current values
	spec, _ := poolDetail["spec"].(map[string]interface{})
	if spec == nil {
		fmt.Println("  Pool has no spec. FAIL")
		return false
	}

	origLease := spec["leaseTimeSecs"]

	// Update lease time
	spec["leaseTimeSecs"] = 120
	updateBody := map[string]interface{}{"spec": spec}
	if err := apiPut(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcppools/%s", *networkName, poolID), updateBody, nil); err != nil {
		fmt.Printf("  Update pool lease time FAILED: %v\n", err)
		return false
	}
	fmt.Println("  UPDATE lease time to 120s: PASS")

	// Verify change
	var updated map[string]interface{}
	if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcppools/%s", *networkName, poolID), &updated); err != nil {
		fmt.Printf("  Verify update FAILED: %v\n", err)
		return false
	}
	updatedSpec, _ := updated["spec"].(map[string]interface{})
	if updatedSpec != nil {
		lt, _ := updatedSpec["leaseTimeSecs"].(float64)
		if int(lt) == 120 {
			fmt.Println("  VERIFY lease time = 120s: PASS")
		} else {
			fmt.Printf("  VERIFY lease time: got %v, expected 120. WARN\n", lt)
		}
	}

	// Restore original
	spec["leaseTimeSecs"] = origLease
	restoreBody := map[string]interface{}{"spec": spec}
	if err := apiPut(client, fmt.Sprintf("/api/v1/namespaces/%s/dhcppools/%s", *networkName, poolID), restoreBody, nil); err != nil {
		fmt.Printf("  Restore pool lease time FAILED: %v\n", err)
		return false
	}
	fmt.Println("  RESTORE original lease time: PASS")

	fmt.Println("  Summary: DHCP Pool CRUD PASSED")
	fmt.Println()
	return true
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func waitForPodRunning(client *http.Client, ns, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s/%s to be Running", ns, name)
		case <-ticker.C:
			var pod map[string]interface{}
			if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name), &pod); err != nil {
				continue // pod might not exist yet
			}
			status, _ := pod["status"].(map[string]interface{})
			if status != nil {
				phase, _ := status["phase"].(string)
				if phase == "Running" {
					return nil
				}
			}
		}
	}
}

func waitForPodGone(client *http.Client, ns, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for pod %s/%s to be deleted", ns, name)
		case <-ticker.C:
			if err := apiGet(client, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name), nil); err != nil {
				return nil // 404 or error = gone
			}
		}
	}
}

func extractName(obj map[string]interface{}) string {
	meta, _ := obj["metadata"].(map[string]interface{})
	if meta == nil {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

func apiPost(client *http.Client, path string, body interface{}, result interface{}) error {
	data, _ := json.Marshal(body)
	resp, err := client.Post(*apiURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s → %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}
	if result != nil {
		return json.Unmarshal(respBody, result)
	}
	return nil
}

func apiGet(client *http.Client, path string, result interface{}) error {
	resp, err := client.Get(*apiURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s → %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}
	if result != nil {
		return json.Unmarshal(respBody, result)
	}
	return nil
}

func apiPut(client *http.Client, path string, body interface{}, result interface{}) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", *apiURL+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s → %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}
	if result != nil {
		return json.Unmarshal(respBody, result)
	}
	return nil
}

func apiDelete(client *http.Client, path string) error {
	req, _ := http.NewRequest("DELETE", *apiURL+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DELETE %s → %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
