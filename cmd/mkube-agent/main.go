package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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

func main() {
	apiURL := os.Getenv("MKUBE_API")
	if apiURL == "" {
		apiURL = "http://192.168.200.2:8082"
	}

	log.Printf("mkube-agent %s (%s) starting, api=%s", version, commit, apiURL)

	// Capture our current image digest at startup for self-update detection.
	currentDigest := getImageDigest(selfImage)
	if currentDigest != "" {
		log.Printf("current image digest: %.12s", currentDigest)
	}

	// Main work loop — after completing a job, poll for the next one.
	// This keeps the agent alive so the scheduler can assign new jobs to
	// an already-online host without triggering a PXE reboot.
	for {
		// Check for self-update between jobs
		if currentDigest != "" {
			if newDigest := getImageDigest(selfImage); newDigest != "" && newDigest != currentDigest {
				log.Printf("new agent image detected: %.12s → %.12s — requesting container restart", currentDigest, newDigest)
				requestContainerRestart()
				// If stormd shutdown didn't kill us, sleep and let stormd handle it
				time.Sleep(10 * time.Second)
				continue
			}
		}

		job, err := pollForWork(apiURL)
		if err != nil {
			log.Printf("no work available: %v — will retry in 30s", err)
			time.Sleep(30 * time.Second)
			continue
		}

		log.Printf("job assigned: %s/%s", job.Metadata.Namespace, job.Metadata.Name)

		// Start heartbeat
		hbStop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			heartbeat(apiURL, hbStop)
		}()

		// Execute job — build container mode or legacy inline script
		var exitCode int
		var execErr error
		if job.Spec.Repo != "" && job.Spec.BuildScript != "" {
			log.Printf("build container mode: repo=%s script=%s", job.Spec.Repo, job.Spec.BuildScript)
			exitCode, execErr = executeBuildContainer(apiURL, job)
		} else if job.Spec.Script != "" {
			log.Printf("legacy inline script mode")
			exitCode, execErr = executeScript(apiURL, job)
		} else {
			execErr = fmt.Errorf("job has neither repo+buildScript nor script")
			exitCode = 1
		}

		// Stop heartbeat
		close(hbStop)
		wg.Wait()

		// Report completion
		reportComplete(apiURL, exitCode, execErr)

		log.Printf("job finished with exit code %d — polling for next job", exitCode)
	}
}

// pollForWork polls GET /api/v1/agent/work until a job is assigned.
func pollForWork(apiURL string) (*agentJob, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := apiURL + "/api/v1/agent/work"

	backoff := 5 * time.Second
	maxRetries := 60 // 5 minutes

	for i := 0; i < maxRetries; i++ {
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("work poll error (attempt %d/%d): %v", i+1, maxRetries, err)
			time.Sleep(backoff)
			continue
		}

		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			log.Printf("no work yet (attempt %d/%d)", i+1, maxRetries)
			time.Sleep(backoff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("work poll: %d %s", resp.StatusCode, string(body))
			time.Sleep(backoff)
			continue
		}

		var job agentJob
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding job: %w", err)
		}
		resp.Body.Close()
		return &job, nil
	}

	return nil, fmt.Errorf("gave up after %d attempts", maxRetries)
}

// heartbeat sends periodic heartbeats to mkube.
func heartbeat(apiURL string, stop <-chan struct{}) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := apiURL + "/api/v1/agent/heartbeat"
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			resp, err := client.Post(url, "application/json", strings.NewReader("{}"))
			if err != nil {
				log.Printf("heartbeat error: %v", err)
				continue
			}
			resp.Body.Close()
		}
	}
}

// executeBuildContainer runs the job in a disposable build container via podman.
// Flow: pull image → podman run (git clone repo, run buildScript) → stream logs → dispose.
func executeBuildContainer(apiURL string, job *agentJob) (int, error) {
	image := job.Spec.BuildImage
	if image == "" {
		image = defaultBuildImage
	}

	// Pull the build image
	log.Printf("pulling build image: %s", image)
	pullCmd := exec.Command("podman", "pull", "--tls-verify=false", image)
	pullOut, err := pullCmd.CombinedOutput()
	if err != nil {
		return 1, fmt.Errorf("pulling image %s: %v\n%s", image, err, string(pullOut))
	}
	log.Printf("image pulled successfully")

	// Build the command to run inside the container.
	// Ensure git is available (bare images like fedora:rawhide may not have it).
	// If GIT_TOKEN is set, configure git credential helper for HTTPS clones of private repos.
	// Clone the repo, cd into it, run the build script.
	var parts []string
	parts = append(parts, "set -e")
	parts = append(parts, "command -v git >/dev/null 2>&1 || { echo 'Installing git...'; dnf install -y git 2>&1 | tail -3 || yum install -y git 2>&1 | tail -3 || (apt-get update && apt-get install -y git) 2>&1 | tail -3; }")

	// Configure git auth if GIT_TOKEN is provided
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

	// Construct podman run args
	args := []string{
		"run", "--rm",
		"--name", fmt.Sprintf("build-%s-%s", job.Metadata.Namespace, job.Metadata.Name),
	}

	// Pass environment variables
	for k, v := range job.Spec.Env {
		args = append(args, "-e", k+"="+v)
	}

	// Mount /data as /output inside the container for artifact collection
	args = append(args, "-v", "/data:/output")

	// Give the build container access to podman socket if available,
	// so build scripts can build and push container images.
	if _, err := os.Stat("/run/podman/podman.sock"); err == nil {
		args = append(args,
			"-v", "/run/podman/podman.sock:/run/podman/podman.sock",
			"--security-opt", "label=disable",
		)
	}

	args = append(args, image, "bash", "-c", buildCmd)

	log.Printf("running: podman %s", strings.Join(args, " "))

	cmd := exec.Command("podman", args...)

	// Capture output — tee to both log streamer and a buffer for git commit
	pr, pw := io.Pipe()
	var logBuf bytes.Buffer
	tee := io.MultiWriter(pw, &logBuf)
	cmd.Stdout = tee
	cmd.Stderr = tee

	// Stream logs in background
	go streamLogs(apiURL, pr)

	if err := cmd.Start(); err != nil {
		pw.Close()
		return 1, fmt.Errorf("starting build container: %w", err)
	}

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

	// Commit job logs back to the git repo
	commitJobLogs(job, logBuf.Bytes(), exitCode)

	return exitCode, nil
}

// executeScript runs an inline script directly (legacy mode).
func executeScript(apiURL string, job *agentJob) (int, error) {
	scriptPath := "/tmp/job.sh"
	if err := os.WriteFile(scriptPath, []byte(job.Spec.Script), 0755); err != nil {
		return 1, fmt.Errorf("writing script: %w", err)
	}

	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.Dir = "/data"

	// Set environment
	cmd.Env = os.Environ()
	for k, v := range job.Spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Capture output
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	// Stream logs in background
	go streamLogs(apiURL, pr)

	if err := cmd.Start(); err != nil {
		pw.Close()
		return 1, fmt.Errorf("starting script: %w", err)
	}

	err := cmd.Wait()
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

	return exitCode, nil
}

// streamLogs reads from the pipe and sends log chunks to mkube.
func streamLogs(apiURL string, r io.Reader) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := apiURL + "/api/v1/agent/logs"

	buf := make([]byte, 4096)
	var batch []byte

	flush := func() {
		if len(batch) == 0 {
			return
		}
		resp, err := client.Post(url, "text/plain", bytes.NewReader(batch))
		if err != nil {
			log.Printf("log stream error: %v", err)
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

// reportComplete sends the exit code to mkube.
func reportComplete(apiURL string, exitCode int, execErr error) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := apiURL + "/api/v1/agent/complete"

	errMsg := ""
	if execErr != nil {
		errMsg = execErr.Error()
	}

	body, _ := json.Marshal(map[string]interface{}{
		"exitCode":     exitCode,
		"errorMessage": errMsg,
	})

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("complete report error: %v", err)
		return
	}
	resp.Body.Close()
}

// commitJobLogs clones the job's repo and pushes the build log as logs/job-{datetime}.log.
// Best-effort — failures are logged but don't affect the job result.
func commitJobLogs(job *agentJob, logData []byte, exitCode int) {
	if job.Spec.Repo == "" {
		return
	}

	now := time.Now().UTC()
	logFile := fmt.Sprintf("logs/job-%s.log", now.Format("2006-01-02-150405"))
	cloneDir := "/tmp/log-commit"

	// Build git URL with auth if available
	repoURL := job.Spec.Repo
	token := job.Spec.Env["GIT_TOKEN"]
	pushURL := repoURL
	if token != "" {
		// Inject token into HTTPS URL: https://github.com/... → https://x-access-token:TOKEN@github.com/...
		pushURL = strings.Replace(repoURL, "https://", fmt.Sprintf("https://x-access-token:%s@", token), 1)
	}

	// Clean up any previous log-commit dir
	os.RemoveAll(cloneDir)

	// Shallow clone
	cloneCmd := exec.Command("git", "clone", "--depth", "1", pushURL, cloneDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		log.Printf("log commit: clone failed: %v\n%s", err, string(out))
		return
	}

	// Create logs dir and write log file
	logsDir := cloneDir + "/logs"
	os.MkdirAll(logsDir, 0755)

	// Add header with job metadata
	var header bytes.Buffer
	fmt.Fprintf(&header, "# Job: %s/%s\n", job.Metadata.Namespace, job.Metadata.Name)
	fmt.Fprintf(&header, "# Date: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&header, "# Exit code: %d\n", exitCode)
	fmt.Fprintf(&header, "# Repo: %s\n", job.Spec.Repo)
	fmt.Fprintf(&header, "# Script: %s\n", job.Spec.BuildScript)
	fmt.Fprintf(&header, "# Image: %s\n\n", job.Spec.BuildImage)
	header.Write(logData)

	if err := os.WriteFile(cloneDir+"/"+logFile, header.Bytes(), 0644); err != nil {
		log.Printf("log commit: write failed: %v", err)
		return
	}

	// Git add, commit, push
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
		cmd := exec.Command(c.args[0], c.args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("log commit: %s failed: %v\n%s", c.name, err, string(out))
			return
		}
	}

	log.Printf("log committed: %s", logFile)
	os.RemoveAll(cloneDir)
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// getImageDigest queries the registry for the current digest of an image tag.
// Returns empty string on any error (non-fatal — update check is best-effort).
func getImageDigest(image string) string {
	// Parse image into registry/repo:tag
	// e.g. "registry.gt.lo:5000/mkube-agent:edge" → registry="registry.gt.lo:5000", repo="mkube-agent", tag="edge"
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

	// HEAD request to registry v2 manifest endpoint
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, tag)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequest("HEAD", url, nil)
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
// stormd exits → container dies → restart mechanism pulls fresh image.
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
