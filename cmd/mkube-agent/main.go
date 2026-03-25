package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glennswest/mkube/pkg/podman"
)

var (
	version = "dev"
	commit  = "none"
)

const (
	defaultBuildImage = "registry.gt.lo:5000/rawhidedev:latest"
	defaultRegistry   = "registry.gt.lo:5000"
	selfImage         = "registry.gt.lo:5000/mkube-agent:edge"
	stormdAPI         = "http://127.0.0.1:9080"
	podmanSocket      = "/run/podman/podman.sock"
)

// podmanClient is the global podman API client (set in main).
var podmanClient *podman.Client

// hostDataDir returns the host-visible path for /data.
// Nested podman resolves volume mount paths against the HOST filesystem,
// not the container's mount namespace. So when the agent container has
// -v /var/data:/data, nested podman needs -v /var/data/jobname:/output
// (the host path), not -v /data/jobname:/output (the container path).
func hostDataDir() string {
	if v := os.Getenv("HOST_DATA_DIR"); v != "" {
		return v
	}
	return "/var/data"
}

// agentJob mirrors the Job type from mkube, with only the fields the agent needs.
type agentJob struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Repo        string            `json:"repo"`
		BuildScript string            `json:"buildScript"`
		BuildImage  string            `json:"buildImage"`
		Script      string            `json:"script"`
		Env         map[string]string `json:"env"`
		Timeout     int               `json:"timeout"` // seconds, 0 = default (2h)
	} `json:"spec"`
}

// jobKey returns "namespace/name" for a job.
func (j *agentJob) jobKey() string {
	return j.Metadata.Namespace + "/" + j.Metadata.Name
}

func main() {
	apiURL := os.Getenv("MKUBE_API")
	if apiURL == "" {
		apiURL = "http://192.168.200.2:8082"
	}

	maxWorkers := 4
	if v := os.Getenv("MKUBE_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxWorkers = n
		}
	}

	// Podman socket path (configurable for testing)
	sock := os.Getenv("PODMAN_SOCKET")
	if sock == "" {
		sock = podmanSocket
	}
	podmanClient = podman.New(sock)

	log.Printf("mkube-agent %s (%s) starting, api=%s, maxConcurrent=%d, socket=%s",
		version, commit, apiURL, maxWorkers, sock)

	// Log DNS servers for build containers
	dns := getNameservers()
	log.Printf("build container DNS: %v", dns)

	// Diagnostic: check /data mount at startup
	if fi, err := os.Stat("/data"); err != nil {
		log.Printf("WARNING: /data does not exist: %v", err)
		if err := os.MkdirAll("/data", 0755); err != nil {
			log.Printf("FATAL: cannot create /data: %v", err)
		} else {
			log.Printf("/data created as fallback directory")
		}
	} else {
		log.Printf("/data exists: dir=%v", fi.IsDir())
		if entries, err := os.ReadDir("/data"); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			log.Printf("/data contents: %v", names)
		}
	}

	// Ensure TMPDIR exists on the data disk
	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		os.MkdirAll(tmpDir, 0755)
	} else {
		if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
			os.MkdirAll("/data/tmp", 0755)
			os.Setenv("TMPDIR", "/data/tmp")
			log.Printf("TMPDIR set to /data/tmp (data disk)")
		}
	}

	// Clean up stale containers and images from previous agent lifecycle
	{
		bgCtx := context.Background()
		if n, err := podmanClient.PruneContainers(bgCtx); err == nil && n > 0 {
			log.Printf("[startup] pruned %d stopped containers", n)
		}
		if n, err := podmanClient.PruneImages(bgCtx, false, ""); err == nil && n > 0 {
			log.Printf("[startup] pruned %d dangling images", n)
		}
	}

	// Collect and report podman environment on startup
	agentEnv := collectPodmanEnv()
	if agentEnv != nil {
		log.Printf("podman %s, driver=%s, cgroup=%s, %s/%s",
			agentEnv.PodmanVersion, agentEnv.StorageDriver,
			agentEnv.CgroupVersion, agentEnv.OS, agentEnv.Arch)
		sendEnvHeartbeat(apiURL, agentEnv)
	}

	// Capture our current image digest at startup for self-update detection.
	currentDigest := getImageDigest(selfImage)
	if currentDigest != "" {
		log.Printf("current image digest: %.12s", currentDigest)
	}

	// Semaphore channel limits concurrent workers
	sem := make(chan struct{}, maxWorkers)
	var activeJobs sync.Map
	var activeCount int64

	// Track when we last ran container cleanup
	var lastCleanup time.Time

	// Main dispatch loop
	for {
		// Check for self-update when no jobs are running
		if atomic.LoadInt64(&activeCount) == 0 && currentDigest != "" {
			if newDigest := getImageDigest(selfImage); newDigest != "" && newDigest != currentDigest {
				log.Printf("new agent image detected: %.12s → %.12s — requesting container restart", currentDigest, newDigest)
				requestContainerRestart()
				time.Sleep(10 * time.Second)
				continue
			}
		}

		// Periodically clean up build containers whose jobs no longer exist
		if atomic.LoadInt64(&activeCount) == 0 && time.Since(lastCleanup) > 60*time.Second {
			cleanupOrphanedContainers(apiURL)
			lastCleanup = time.Now()
		}

		job, err := tryGetWork(apiURL)
		if err != nil || job == nil {
			if atomic.LoadInt64(&activeCount) == 0 {
				time.Sleep(5 * time.Second)
			} else {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		key := job.jobKey()
		if _, loaded := activeJobs.LoadOrStore(key, true); loaded {
			log.Printf("job %s already running, skipping", key)
			time.Sleep(2 * time.Second)
			continue
		}

		sem <- struct{}{}
		atomic.AddInt64(&activeCount, 1)
		log.Printf("job %s assigned (active: %d/%d)", key, atomic.LoadInt64(&activeCount), maxWorkers)

		go func(job *agentJob) {
			defer func() {
				<-sem
				activeJobs.Delete(job.jobKey())
				atomic.AddInt64(&activeCount, -1)
				log.Printf("job %s worker done (active: %d/%d)", job.jobKey(), atomic.LoadInt64(&activeCount), maxWorkers)
			}()
			runJob(apiURL, job)
		}(job)

		time.Sleep(500 * time.Millisecond)
	}
}

// tryGetWork makes a single attempt to get work from the server.
func tryGetWork(apiURL string) (*agentJob, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL + "/api/v1/agent/work")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("work poll: %d %s", resp.StatusCode, string(body))
	}

	var job agentJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("decoding job: %w", err)
	}
	return &job, nil
}

// runJob executes a single job end-to-end: heartbeat, execute, report.
func runJob(apiURL string, job *agentJob) {
	key := job.jobKey()
	log.Printf("[%s] starting execution", key)

	// Build timeout from job spec (default 2h)
	timeout := 2 * time.Hour
	if job.Spec.Timeout > 0 {
		timeout = time.Duration(job.Spec.Timeout) * time.Second
	}

	// Cancellable context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Heartbeat goroutine — also watches for server-side cancellation
	hbStop := make(chan struct{})
	cancelSignal := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeat(apiURL, key, hbStop, cancelSignal)
	}()

	// Cancel context when server signals cancellation
	go func() {
		select {
		case <-cancelSignal:
			log.Printf("[%s] cancellation signal received from server", key)
			cancel()
		case <-ctx.Done():
		}
	}()

	var exitCode int
	var execErr error
	if job.Spec.Repo != "" && job.Spec.BuildScript != "" {
		reportStatus(apiURL, key, fmt.Sprintf("Build container job: %s → %s (timeout: %s)", job.Spec.Repo, job.Spec.BuildScript, timeout))
		exitCode, execErr = executeBuildContainer(ctx, apiURL, job)
	} else if job.Spec.Script != "" {
		reportStatus(apiURL, key, "Inline script job")
		exitCode, execErr = executeScript(apiURL, job)
	} else {
		execErr = fmt.Errorf("job has neither repo+buildScript nor script")
		exitCode = 1
	}

	// Determine if cancellation/timeout caused the failure
	if ctx.Err() == context.Canceled {
		execErr = fmt.Errorf("cancelled")
		exitCode = 137
	} else if ctx.Err() == context.DeadlineExceeded {
		execErr = fmt.Errorf("timed out after %s", timeout)
		exitCode = 137
	}

	close(hbStop)
	wg.Wait()

	reportComplete(apiURL, job, exitCode, execErr)
	log.Printf("[%s] finished with exit code %d", key, exitCode)

	pruneContainerStorage(key)

	// Re-report podman environment (images may have changed)
	if env := collectPodmanEnv(); env != nil {
		sendEnvHeartbeat(apiURL, env)
	}

	cleanJobOutputDir(job)
}

// heartbeat sends periodic heartbeats for a specific job.
// If the server responds with {"cancel": true}, signals the cancelCh.
func heartbeat(apiURL string, jobKey string, stop <-chan struct{}, cancelCh chan<- struct{}) {
	client := &http.Client{Timeout: 5 * time.Second}
	hbURL := apiURL + "/api/v1/agent/heartbeat"
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	body, _ := json.Marshal(map[string]string{"job": jobKey})
	cancelled := false

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			resp, err := client.Post(hbURL, "application/json", bytes.NewReader(body))
			if err != nil {
				log.Printf("[%s] heartbeat error: %v", jobKey, err)
				continue
			}
			if !cancelled {
				// 404 = job no longer running on server (cancelled/timed out)
				if resp.StatusCode == http.StatusNotFound {
					log.Printf("[%s] heartbeat 404 — job no longer running on server", jobKey)
					cancelled = true
					select {
					case cancelCh <- struct{}{}:
					default:
					}
				} else if resp.Body != nil {
					// Check response body for explicit cancel signal
					var hbResp struct {
						Cancel bool `json:"cancel"`
					}
					if json.NewDecoder(resp.Body).Decode(&hbResp) == nil && hbResp.Cancel {
						log.Printf("[%s] cancel signal received from server", jobKey)
						cancelled = true
						select {
						case cancelCh <- struct{}{}:
						default:
						}
					}
				}
			}
			resp.Body.Close()
		}
	}
}

// reportStatus sends a status message to the runner log via heartbeat endpoint.
func reportStatus(apiURL string, jobKey string, msg string) {
	log.Printf("[%s] %s", jobKey, msg)
	client := &http.Client{Timeout: 5 * time.Second}
	body, _ := json.Marshal(map[string]string{"job": jobKey, "status": msg})
	resp, err := client.Post(apiURL+"/api/v1/agent/heartbeat", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// executeBuildContainer runs the job in a disposable build container via podman socket API.
// The ctx controls cancellation and timeout — when cancelled, the build container is stopped.
func executeBuildContainer(ctx context.Context, apiURL string, job *agentJob) (int, error) {
	key := job.jobKey()
	image := job.Spec.BuildImage
	if image == "" {
		image = defaultBuildImage
	}

	// Check if image is already present (pre-cached by imagesync); pull only if missing
	imagePresent := false
	if imgs, err := podmanClient.Images(ctx); err == nil {
		for _, img := range imgs {
			for _, name := range img.Names {
				if name == image {
					imagePresent = true
					break
				}
			}
			if imagePresent {
				break
			}
		}
	}
	if imagePresent {
		log.Printf("[%s] Image %s already present, skipping pull", key, image)
	} else {
		reportStatus(apiURL, key, fmt.Sprintf("Pulling build image %s", image))
		if err := podmanClient.Pull(ctx, image, false); err != nil {
			reportStatus(apiURL, key, fmt.Sprintf("FAILED to pull %s: %v", image, err))
			return 1, fmt.Errorf("pulling image %s: %w", image, err)
		}
	}
	reportStatus(apiURL, key, fmt.Sprintf("Image %s ready", image))

	// Build the command to run inside the container
	var parts []string
	parts = append(parts, "set -e")
	parts = append(parts, "command -v git >/dev/null 2>&1 || { echo 'Installing git...'; dnf install -y git 2>&1 | tail -3 || yum install -y git 2>&1 | tail -3 || (apt-get update && apt-get install -y git) 2>&1 | tail -3; }")

	if token := job.Spec.Env["GIT_TOKEN"]; token != "" {
		parts = append(parts,
			`git config --global credential.helper 'store --file /tmp/.git-credentials'`,
			fmt.Sprintf(`echo 'https://x-access-token:%s@github.com' > /tmp/.git-credentials`, token),
		)
	}

	parts = append(parts,
		fmt.Sprintf("git clone %s /build", shellQuote(job.Spec.Repo)),
		"cd /build",
		fmt.Sprintf("chmod +x ./%s", shellQuote(job.Spec.BuildScript)),
		fmt.Sprintf("./%s", shellQuote(job.Spec.BuildScript)),
	)
	buildCmd := strings.Join(parts, " && ")

	containerName := fmt.Sprintf("build-%s-%s", job.Metadata.Namespace, job.Metadata.Name)

	// Build mounts
	buildStorageHost := fmt.Sprintf("%s/build-storage", hostDataDir())
	os.MkdirAll("/data/build-storage", 0755)

	localOutputDir := fmt.Sprintf("/data/%s", job.Metadata.Name)
	hostOutputDir := fmt.Sprintf("%s/%s", hostDataDir(), job.Metadata.Name)
	if err := os.MkdirAll(localOutputDir, 0755); err != nil {
		log.Printf("[%s] WARNING: failed to create output dir %s: %v", key, localOutputDir, err)
	}

	mounts := []podman.Mount{
		{Source: buildStorageHost, Dest: "/var/lib/containers"},
		{Source: hostOutputDir, Dest: "/output"},
	}

	// Mount podman socket if available (for builds that need nested podman)
	if _, err := os.Stat(podmanSocket); err == nil {
		mounts = append(mounts, podman.Mount{Source: podmanSocket, Dest: podmanSocket})
	}

	// Force-remove any stale container from a previous crashed run
	_ = podmanClient.RemoveContainer(ctx, containerName, true)

	cfg := podman.ContainerConfig{
		Name:       containerName,
		Image:      image,
		Command:    []string{"bash", "-c", buildCmd},
		Env:        job.Spec.Env,
		Mounts:     mounts,
		Privileged: true,
		Remove:     true,
		DNS:        getNameservers(),
	}

	reportStatus(apiURL, key, fmt.Sprintf("Starting build: git clone %s → ./%s (image: %s)",
		job.Spec.Repo, job.Spec.BuildScript, image))

	// Stream logs to mkube as they arrive
	logURL := apiURL + "/api/v1/agent/logs?job=" + url.QueryEscape(key)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	var batchBuf []byte
	lastFlush := time.Now()

	flushLogs := func(force bool) {
		logMu.Lock()
		defer logMu.Unlock()
		if len(batchBuf) == 0 {
			return
		}
		if !force && len(batchBuf) < 4096 && time.Since(lastFlush) < time.Second {
			return
		}
		resp, err := httpClient.Post(logURL, "text/plain", bytes.NewReader(batchBuf))
		if err != nil {
			log.Printf("[%s] log stream error: %v", key, err)
		} else {
			resp.Body.Close()
		}
		batchBuf = batchBuf[:0]
		lastFlush = time.Now()
	}

	// Periodic log flusher
	flushTicker := time.NewTicker(1 * time.Second)
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		for {
			select {
			case <-flushTicker.C:
				flushLogs(false)
			case <-ctx.Done():
				return
			}
		}
	}()

	onLog := func(chunk []byte) {
		logBuf.Write(chunk)
		logMu.Lock()
		batchBuf = append(batchBuf, chunk...)
		logMu.Unlock()
		if len(batchBuf) > 4096 {
			flushLogs(true)
		}
	}

	result, err := podmanClient.Run(ctx, cfg, onLog)
	flushTicker.Stop()
	flushLogs(true) // final flush

	// If context was cancelled/timed out, stop the build container
	if ctx.Err() != nil {
		reason := "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			reason = "timed out"
		}
		reportStatus(apiURL, key, fmt.Sprintf("Build %s — stopping container %s", reason, containerName))
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if stopErr := podmanClient.StopContainer(stopCtx, containerName, 10); stopErr != nil {
			log.Printf("[%s] stop container failed: %v — force removing", key, stopErr)
			_ = podmanClient.RemoveContainer(stopCtx, containerName, true)
		}
		stopCancel()
		return 137, ctx.Err()
	}

	if err != nil {
		reportStatus(apiURL, key, fmt.Sprintf("FAILED: %v", err))
		return 1, err
	}

	exitCode := result.ExitCode
	if exitCode == 0 {
		reportStatus(apiURL, key, "Build completed successfully")
	} else {
		reportStatus(apiURL, key, fmt.Sprintf("Build FAILED (exit code %d)", exitCode))
	}

	// Logs are streamed to mkube in real-time; no need for git commit
	return exitCode, nil
}

// executeScript runs an inline script directly (legacy mode).
func executeScript(apiURL string, job *agentJob) (int, error) {
	key := job.jobKey()
	scriptPath := fmt.Sprintf("/tmp/job-%s.sh", job.Metadata.Name)
	if err := os.WriteFile(scriptPath, []byte(job.Spec.Script), 0755); err != nil {
		return 1, fmt.Errorf("writing script: %w", err)
	}
	defer os.Remove(scriptPath)

	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.Dir = "/data"

	cmd.Env = os.Environ()
	for k, v := range job.Spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	go streamLogs(apiURL, key, pr)

	if err := cmd.Start(); err != nil {
		pw.Close()
		return 1, fmt.Errorf("starting script: %w", err)
	}

	err := cmd.Wait()
	pw.Close()

	time.Sleep(500 * time.Millisecond)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return 1, err
		}
	}

	return exitCode, nil
}

// streamLogs reads from the pipe and sends log chunks to mkube with job identity.
func streamLogs(apiURL string, jobKey string, r io.Reader) {
	client := &http.Client{Timeout: 10 * time.Second}
	logURL := apiURL + "/api/v1/agent/logs?job=" + url.QueryEscape(jobKey)

	buf := make([]byte, 4096)
	var batch []byte

	flush := func() {
		if len(batch) == 0 {
			return
		}
		resp, err := client.Post(logURL, "text/plain", bytes.NewReader(batch))
		if err != nil {
			log.Printf("[%s] log stream error: %v", jobKey, err)
		} else {
			resp.Body.Close()
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		for {
			n, err := r.Read(buf)
			if n > 0 {
				batch = append(batch, buf[:n]...)
				if len(batch) > 4096 {
					flush()
				}
			}
			if err != nil {
				close(done)
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			flush()
			return
		case <-ticker.C:
			flush()
		}
	}
}

// reportComplete sends the exit code to mkube with job identity.
func reportComplete(apiURL string, job *agentJob, exitCode int, execErr error) {
	client := &http.Client{Timeout: 10 * time.Second}

	errMsg := ""
	if execErr != nil {
		errMsg = execErr.Error()
	}

	body, _ := json.Marshal(map[string]interface{}{
		"exitCode":     exitCode,
		"errorMessage": errMsg,
		"jobName":      job.Metadata.Name,
		"jobNamespace": job.Metadata.Namespace,
	})

	resp, err := client.Post(apiURL+"/api/v1/agent/complete", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[%s] complete report error: %v", job.jobKey(), err)
		return
	}
	resp.Body.Close()
}


// pruneContainerStorage removes dangling (untagged) images.
// Stopped containers are kept so their filesystems remain accessible
// for extracting build artifacts. They get cleaned up on next startup.
func pruneContainerStorage(ctx2 string) {
	bgCtx := context.Background()

	if n, err := podmanClient.PruneImages(bgCtx, false, ""); err == nil && n > 0 {
		log.Printf("[%s] pruned %d dangling images", ctx2, n)
	}

	// Clean TMPDIR if it exists
	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		entries, _ := os.ReadDir(tmpDir)
		for _, e := range entries {
			os.RemoveAll(tmpDir + "/" + e.Name())
		}
		if len(entries) > 0 {
			log.Printf("[%s] cleaned %d tmp entries from %s", ctx2, len(entries), tmpDir)
		}
	}
}

// cleanJobOutputDir removes the job's output directory if it's empty.
func cleanJobOutputDir(job *agentJob) {
	outputDir := fmt.Sprintf("/data/%s", job.Metadata.Name)
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		os.Remove(outputDir)
		log.Printf("[%s] removed empty output dir", job.jobKey())
	}
}

// cleanupOrphanedContainers removes stopped build containers whose jobs
// no longer exist in mkube. This frees disk after jobs are deleted.
func cleanupOrphanedContainers(apiURL string) {
	bgCtx := context.Background()
	containers, err := podmanClient.ListContainers(bgCtx)
	if err != nil {
		return
	}

	// Collect stopped build containers: name → container ID
	buildContainers := make(map[string]string) // "namespace/jobname" → container ID
	for _, c := range containers {
		if c.State == "running" {
			continue
		}
		for _, name := range c.Names {
			// Container names are "build-<namespace>-<jobname>"
			if !strings.HasPrefix(name, "build-") {
				continue
			}
			rest := strings.TrimPrefix(name, "build-")
			// Split into namespace and job name at first "-"
			if ns, jobName, ok := strings.Cut(rest, "-"); ok {
				buildContainers[ns+"/"+jobName] = c.ID
			}
		}
	}

	if len(buildContainers) == 0 {
		return
	}

	// Check which jobs still exist in mkube
	for jobKey, containerID := range buildContainers {
		parts := strings.SplitN(jobKey, "/", 2)
		if len(parts) != 2 {
			continue
		}
		ns, name := parts[0], parts[1]

		// GET the job from mkube — 404 means it's been deleted
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(apiURL + "/api/v1/namespaces/" + url.PathEscape(ns) + "/jobs/" + url.PathEscape(name))
		if err != nil {
			continue // can't reach mkube, skip
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			log.Printf("[cleanup] removing container for deleted job %s", jobKey)
			_ = podmanClient.RemoveContainer(bgCtx, containerID, true)
		}
	}
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// ── Podman environment collection ───────────────────────────────────────────

// agentEnvironment mirrors the AgentEnvironment type from mkube provider.
type agentEnvironment struct {
	AgentVersion  string       `json:"agentVersion,omitempty"`
	AgentCommit   string       `json:"agentCommit,omitempty"`
	PodmanVersion string       `json:"podmanVersion"`
	StorageDriver string       `json:"storageDriver"`
	StoragePath   string       `json:"storagePath"`
	CgroupVersion string       `json:"cgroupVersion"`
	OS            string       `json:"os"`
	Arch          string       `json:"arch"`
	Images        []agentImage `json:"images,omitempty"`
	ReportedAt    string       `json:"reportedAt"`
}

type agentImage struct {
	Name string `json:"name"`
	Arch string `json:"arch"`
	Size string `json:"size"`
}

// collectPodmanEnv gathers runtime details via the podman socket API.
func collectPodmanEnv() *agentEnvironment {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := podmanClient.Info(ctx)
	if err != nil {
		log.Printf("podman info failed: %v", err)
		return nil
	}

	env := &agentEnvironment{
		AgentVersion:  version,
		AgentCommit:   commit,
		PodmanVersion: info.Version,
		StorageDriver: info.StorageDriver,
		StoragePath:   info.StoragePath,
		CgroupVersion: info.CgroupVersion,
		OS:            info.OS,
		Arch:          info.Arch,
	}

	images, err := podmanClient.Images(ctx)
	if err == nil {
		for _, img := range images {
			name := "<none>"
			if len(img.Names) > 0 {
				name = img.Names[0]
			}
			env.Images = append(env.Images, agentImage{
				Name: name,
				Arch: img.Arch,
				Size: formatSize(img.Size),
			})
		}
	}

	return env
}

// sendEnvHeartbeat sends the agent environment to mkube via the heartbeat endpoint.
func sendEnvHeartbeat(apiURL string, env *agentEnvironment) {
	client := &http.Client{Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]interface{}{
		"job": "",
		"env": env,
	})
	resp, err := client.Post(apiURL+"/api/v1/agent/heartbeat", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("env heartbeat failed: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("agent environment reported (%d images)", len(env.Images))
}

// formatSize formats bytes to human-readable (e.g. "1.8G", "256M").
func formatSize(b int64) string {
	const (
		gb = 1024 * 1024 * 1024
		mb = 1024 * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0fM", float64(b)/float64(mb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// getNameservers returns DNS servers for build containers.
// Priority: MKUBE_DNS env var > systemd-resolved upstream > resolv.conf > fallback 8.8.8.8
func getNameservers() []string {
	// Explicit DNS override (set by mkube on agent container)
	if dns := os.Getenv("MKUBE_DNS"); dns != "" {
		var servers []string
		for _, s := range strings.Split(dns, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				servers = append(servers, s)
			}
		}
		if len(servers) > 0 {
			return servers
		}
	}

	// Try systemd-resolved's actual upstream config
	if data, err := os.ReadFile("/run/systemd/resolve/resolv.conf"); err == nil {
		servers := parseNameservers(data)
		if len(servers) > 0 {
			return servers
		}
	}

	// Fall back to /etc/resolv.conf, filtering out stub resolver
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		servers := parseNameservers(data)
		// Filter out 127.0.0.53 (systemd-resolved stub — unreachable from nested containers)
		var real []string
		for _, s := range servers {
			if s != "127.0.0.53" && s != "127.0.0.1" {
				real = append(real, s)
			}
		}
		if len(real) > 0 {
			return real
		}
	}

	// Last resort — public DNS
	return []string{"8.8.8.8", "1.1.1.1"}
}

func parseNameservers(data []byte) []string {
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "nameserver ") {
			ns := strings.TrimSpace(strings.TrimPrefix(line, "nameserver "))
			if ns != "" {
				servers = append(servers, ns)
			}
		}
	}
	return servers
}

// getImageDigest queries the registry for the current digest of an image tag.
func getImageDigest(image string) string {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	registry := parts[0]
	repoTag := parts[1]

	repo := repoTag
	tag := "latest"
	if idx := strings.LastIndex(repoTag, ":"); idx != -1 {
		repo = repoTag[:idx]
		tag = repoTag[idx+1:]
	}

	reqURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, tag)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequest("HEAD", reqURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	return resp.Header.Get("Docker-Content-Digest")
}

// requestContainerRestart asks stormd to shut down the entire container.
func requestContainerRestart() {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(stormdAPI+"/api/v1/shutdown", "application/json", strings.NewReader("{}"))
	if err != nil {
		log.Printf("stormd shutdown request failed: %v — falling back to process exit", err)
		os.Exit(0)
	}
	resp.Body.Close()
	log.Printf("stormd shutdown requested (status %d)", resp.StatusCode)
}
