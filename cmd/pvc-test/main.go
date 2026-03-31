package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dataDir := "/data"
	if d := os.Getenv("DATA_DIR"); d != "" {
		dataDir = d
	}
	interval := 10 * time.Second
	iteration := 0

	fmt.Printf("pvc-test: starting, dataDir=%s interval=%s\n", dataDir, interval)

	// Check if previous data survives restart
	counterFile := filepath.Join(dataDir, "counter.txt")
	if b, err := os.ReadFile(counterFile); err == nil {
		fmt.Printf("pvc-test: found previous counter: %s (data persisted!)\n", string(b))
	} else {
		fmt.Printf("pvc-test: no previous counter (fresh PVC)\n")
	}

	for {
		iteration++
		ts := time.Now().Format(time.RFC3339)

		// Write a test file with random data
		testFile := filepath.Join(dataDir, "test.dat")
		payload := make([]byte, 4096)
		if _, err := rand.Read(payload); err != nil {
			fmt.Printf("[%s] #%d ERROR generating random data: %v\n", ts, iteration, err)
			time.Sleep(interval)
			continue
		}

		if err := os.WriteFile(testFile, payload, 0644); err != nil {
			fmt.Printf("[%s] #%d ERROR writing %s: %v\n", ts, iteration, testFile, err)
			time.Sleep(interval)
			continue
		}

		// Read it back and verify
		readBack, err := os.ReadFile(testFile)
		if err != nil {
			fmt.Printf("[%s] #%d ERROR reading back %s: %v\n", ts, iteration, testFile, err)
			time.Sleep(interval)
			continue
		}

		if len(readBack) != len(payload) {
			fmt.Printf("[%s] #%d MISMATCH: wrote %d bytes, read %d bytes\n", ts, iteration, len(payload), len(readBack))
			time.Sleep(interval)
			continue
		}

		match := true
		for i := range payload {
			if payload[i] != readBack[i] {
				match = false
				break
			}
		}

		// Update persistent counter
		if err := os.WriteFile(counterFile, []byte(fmt.Sprintf("%d", iteration)), 0644); err != nil {
			fmt.Printf("[%s] #%d WARN: failed to write counter: %v\n", ts, iteration, err)
		}

		// Report disk usage
		used := int64(0)
		entries, _ := os.ReadDir(dataDir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				used += info.Size()
			}
		}

		if match {
			fmt.Printf("[%s] #%d OK: wrote+verified 4096 bytes, %d files, %d bytes used\n", ts, iteration, len(entries), used)
		} else {
			fmt.Printf("[%s] #%d FAIL: data mismatch after write+read!\n", ts, iteration)
		}

		time.Sleep(interval)
	}
}
