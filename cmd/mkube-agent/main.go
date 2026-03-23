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
)

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

	log.Printf("mkube-agent %s (%s) starting, api=%s, maxConcurrent=%d", version, commit, apiURL, maxWorkers)

	// Diagnostic: check /data mount at startup
	if fi, err := os.Stat("/data"); err != nil {
		log.Printf("WARNING: /data does not exist: %v", err)
		// Try to create it as fallback
		if err := os.MkdirAll("/data", 0755); err != nil {
			log.Printf("FATAL: cannot create /data: %v", err)
		} else {
			log.Printf("/data created as fallback directory")
		}
	} else {
		log.Printf("/data exists: dir=%v", fi.IsDir())
		// List contents for diagnostics
		if entries, err := os.ReadDir("/data"); err == nil {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			log.Printf("/data contents: %v", names)
		}
	}

	// Ensure TMPDIR exists on the data disk — podman uses it for blob
	// downloads during image pulls. Without this, pulls write to /var/tmp
	// on the OS disk which is typically small (10-20GB).
	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		os.MkdirAll(tmpDir, 0755)
	} else {
		// Default: use /data/tmp if /data exists (mounted from host data disk)
		if fi, err := os.Stat("/data"); err == nil && fi.IsDir() {
			os.MkdirAll("/data/tmp", 0755)
			os.Setenv("TMPDIR", "/data/tmp")
			log.Printf("TMPDIR set to /data/tmp (data disk)")
		}
	}

	// Clean up stale container storage on startup — previous images, stopped
	// containers, and dangling layers accumulate across job runs.
	pruneContainerStorage("startup")

	// Capture our current image digest at startup for self-update detection.
	currentDigest := getImageDigest(selfImage)
	if currentDigest != "" {
		log.Printf("current image digest: %.12s", currentDigest)
	}

	// Semaphore channel limits concurrent workers
	sem := make(chan struct{}, maxWorkers)
	var activeJobs sync.Map // track running job keys to avoid duplicates
	var activeCount int64

	// Main dispatch loop — polls for work and spawns workers
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

		// Try to get a job
		job, err := tryGetWork(apiURL)
		if err != nil || job == nil {
			// No work available — back off
			if atomic.LoadInt64(&activeCount) == 0 {
				time.Sleep(5 * time.Second)
			} else {
				// Jobs running, poll faster for additional work
				time.Sleep(2 * time.Second)
			}
			continue
		}

		// Deduplicate — don't run same job twice
		key := job.jobKey()
		if _, loaded := activeJobs.LoadOrStore(key, true); loaded {
			log.Printf("job %s already running, skipping", key)
			time.Sleep(2 * time.Second)
			continue
		}

		// Acquire semaphore slot (blocks if at max concurrency)
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

		// Small delay before next poll to let server update state
		time.Sleep(500 * time.Millisecond)
	}
}

// tryGetWork makes a single attempt to get work from the server.
// Returns nil, nil if no work is available (204).
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

	// Start job-scoped heartbeat
	hbStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeat(apiURL, key, hbStop)
	}()

	// Execute job
	var exitCode int
	var execErr error
	if job.Spec.Repo != "" && job.Spec.BuildScript != "" {
		reportStatus(apiURL, key, fmt.Sprintf("Build container job: %s → %s", job.Spec.Repo, job.Spec.BuildScript))
		exitCode, execErr = executeBuildContainer(apiURL, job)
	} else if job.Spec.Script != "" {
		reportStatus(apiURL, key, "Inline script job")
		exitCode, execErr = executeScript(apiURL, job)
	} else {
		execErr = fmt.Errorf("job has neither repo+buildScript nor script")
		exitCode = 1
	}

	// Stop heartbeat
	close(hbStop)
	wg.Wait()

	// Report completion with job identity
	reportComplete(apiURL, job, exitCode, execErr)
	log.Printf("[%s] finished with exit code %d", key, exitCode)

	// Clean up container storage after each job — remove unused images
	// older than 24h to prevent disk exhaustion from large build images.
	pruneContainerStorage(key)

	// Clean job output directory if empty (artifact collection already happened)
	cleanJobOutputDir(job)
}

// heartbeat sends periodic heartbeats for a specific job.
func heartbeat(apiURL string, jobKey string, stop <-chan struct{}) {
	client := &http.Client{Timeout: 5 * time.Second}
	hbURL := apiURL + "/api/v1/agent/heartbeat"
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	body, _ := json.Marshal(map[string]string{"job": jobKey})

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

// executeBuildContainer runs the job in a disposable build container via podman.
func executeBuildContainer(apiURL string, job *agentJob) (int, error) {
	key := job.jobKey()
	image := job.Spec.BuildImage
	if image == "" {
		image = defaultBuildImage
	}

	// Pull the build image
	reportStatus(apiURL, key, fmt.Sprintf("Pulling build image %s", image))
	pullCmd := exec.Command("podman", "pull", "--tls-verify=false", image)
	pullOut, err := pullCmd.CombinedOutput()
	if err != nil {
		reportStatus(apiURL, key, fmt.Sprintf("FAILED to pull %s", image))
		return 1, fmt.Errorf("pulling image %s: %v\n%s", image, err, string(pullOut))
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

	// Container name unique per job
	containerName := fmt.Sprintf("build-%s-%s", job.Metadata.Namespace, job.Metadata.Name)

	args := []string{
		"run", "--rm",
		"--name", containerName,
	}

	for k, v := range job.Spec.Env {
		args = append(args, "-e", k+"="+v)
	}

	// Mount output dir for artifact isolation between parallel jobs.
	// Create the dir using the container-local path (/data/...) but pass
	// the host-visible path to nested podman (which resolves against the host).
	localOutputDir := fmt.Sprintf("/data/%s", job.Metadata.Name)
	hostOutputDir := fmt.Sprintf("%s/%s", hostDataDir(), job.Metadata.Name)
	if err := os.MkdirAll(localOutputDir, 0755); err != nil {
		log.Printf("[%s] WARNING: failed to create output dir %s: %v", key, localOutputDir, err)
	}
	args = append(args, "-v", hostOutputDir+":/output")

	if _, err := os.Stat("/run/podman/podman.sock"); err == nil {
		args = append(args,
			"-v", "/run/podman/podman.sock:/run/podman/podman.sock",
			"--security-opt", "label=disable",
		)
	}

	args = append(args, image, "bash", "-c", buildCmd)

	reportStatus(apiURL, key, fmt.Sprintf("Starting build: git clone %s → ./%s (image: %s)",
		job.Spec.Repo, job.Spec.BuildScript, image))

	cmd := exec.Command("podman", args...)

	// Capture output — tee to both log streamer and a buffer for git commit
	pr, pw := io.Pipe()
	var logBuf bytes.Buffer
	tee := io.MultiWriter(pw, &logBuf)
	cmd.Stdout = tee
	cmd.Stderr = tee

	// Stream logs with job identity
	go streamLogs(apiURL, key, pr)

	if err := cmd.Start(); err != nil {
		pw.Close()
		reportStatus(apiURL, key, fmt.Sprintf("FAILED to start container: %v", err))
		return 1, fmt.Errorf("starting build container: %w", err)
	}

	reportStatus(apiURL, key, fmt.Sprintf("Build running in container %s", containerName))

	err = cmd.Wait()
	pw.Close()

	// Give log streamer a moment to flush
	time.Sleep(500 * time.Millisecond)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return 1, err
		}
	}

	if exitCode == 0 {
		reportStatus(apiURL, key, "Build completed successfully")
	} else {
		reportStatus(apiURL, key, fmt.Sprintf("Build FAILED (exit code %d)", exitCode))
	}

	commitJobLogs(job, logBuf.Bytes(), exitCode)
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

// commitJobLogs clones the job's repo and pushes the build log as logs/job-{datetime}.log.
func commitJobLogs(job *agentJob, logData []byte, exitCode int) {
	if job.Spec.Repo == "" {
		return
	}

	token := job.Spec.Env["GIT_TOKEN"]
	if token == "" {
		log.Printf("[%s] log commit: skipped (no GIT_TOKEN)", job.jobKey())
		return
	}

	now := time.Now().UTC()
	logFile := fmt.Sprintf("logs/job-%s.log", now.Format("2006-01-02-150405"))
	cloneDir := fmt.Sprintf("/tmp/log-commit-%s", job.Metadata.Name)

	repoURL := job.Spec.Repo
	pushURL := strings.Replace(repoURL, "https://", fmt.Sprintf("https://x-access-token:%s@", token), 1)

	os.RemoveAll(cloneDir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", pushURL, cloneDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		log.Printf("[%s] log commit: clone failed: %v\n%s", job.jobKey(), err, string(out))
		return
	}

	logsDir := cloneDir + "/logs"
	os.MkdirAll(logsDir, 0755)

	var header bytes.Buffer
	fmt.Fprintf(&header, "# Job: %s/%s\n", job.Metadata.Namespace, job.Metadata.Name)
	fmt.Fprintf(&header, "# Date: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&header, "# Exit code: %d\n", exitCode)
	fmt.Fprintf(&header, "# Repo: %s\n", job.Spec.Repo)
	fmt.Fprintf(&header, "# Script: %s\n", job.Spec.BuildScript)
	fmt.Fprintf(&header, "# Image: %s\n\n", job.Spec.BuildImage)
	header.Write(logData)

	if err := os.WriteFile(cloneDir+"/"+logFile, header.Bytes(), 0644); err != nil {
		log.Printf("[%s] log commit: write failed: %v", job.jobKey(), err)
		return
	}

	cmds := []struct {
		name string
		args []string
	}{
		{"config", []string{"git", "-C", cloneDir, "config", "user.email", "mkube-agent@gt.lo"}},
		{"config", []string{"git", "-C", cloneDir, "config", "user.name", "mkube-agent"}},
		{"add", []string{"git", "-C", cloneDir, "add", logFile}},
		{"commit", []string{"git", "-C", cloneDir, "commit", "-m", fmt.Sprintf("build: job %s exit=%d", job.Metadata.Name, exitCode)}},
		{"push", []string{"git", "-C", cloneDir, "push"}},
	}

	for _, c := range cmds {
		cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(cmdCtx, c.args[0], c.args[1:]...)
		out, err := cmd.CombinedOutput()
		cmdCancel()
		if err != nil {
			log.Printf("[%s] log commit: %s failed: %v\n%s", job.jobKey(), c.name, err, string(out))
			return
		}
	}

	log.Printf("[%s] log committed: %s", job.jobKey(), logFile)
	os.RemoveAll(cloneDir)
}

// pruneContainerStorage removes unused container images and build cache.
// On startup it does a full prune; after jobs it prunes images older than 24h.
func pruneContainerStorage(context string) {
	log.Printf("[%s] pruning container storage", context)

	// Remove all stopped containers (should be none with --rm, but just in case)
	rmCmd := exec.Command("podman", "container", "prune", "-f")
	rmCmd.CombinedOutput()

	// Remove dangling (untagged) images
	danglingCmd := exec.Command("podman", "image", "prune", "-f")
	if out, err := danglingCmd.CombinedOutput(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			log.Printf("[%s] pruned dangling images: %s", context, s)
		}
	}

	// Remove unused images older than 24h (keeps the currently-pulled build image)
	oldCmd := exec.Command("podman", "image", "prune", "-af", "--filter", "until=24h")
	if out, err := oldCmd.CombinedOutput(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			log.Printf("[%s] pruned old images: %s", context, s)
		}
	}

	// Remove build cache
	buildCmd := exec.Command("podman", "builder", "prune", "-af")
	buildCmd.CombinedOutput()

	// Clean TMPDIR if it exists
	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		entries, _ := os.ReadDir(tmpDir)
		for _, e := range entries {
			os.RemoveAll(tmpDir + "/" + e.Name())
		}
		if len(entries) > 0 {
			log.Printf("[%s] cleaned %d tmp entries from %s", context, len(entries), tmpDir)
		}
	}

	// Log remaining disk usage
	dfCmd := exec.Command("df", "-h", "/var/lib/containers")
	if out, err := dfCmd.CombinedOutput(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 1 {
			log.Printf("[%s] container storage: %s", context, strings.TrimSpace(lines[len(lines)-1]))
		}
	}
}

// cleanJobOutputDir removes the job's output directory if it's empty.
// Non-empty dirs are preserved (contain artifacts the user may want).
func cleanJobOutputDir(job *agentJob) {
	outputDir := fmt.Sprintf("/data/%s", job.Metadata.Name)
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return // dir doesn't exist or not readable
	}
	if len(entries) == 0 {
		os.Remove(outputDir)
		log.Printf("[%s] removed empty output dir", job.jobKey())
	}
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
