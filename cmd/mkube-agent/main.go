package main

import (
	"bytes"
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
	defaultBuildImage = "registry.gt.lo:5000/fedoradev:latest"
	defaultRegistry   = "registry.gt.lo:5000"
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

	// Poll for work
	job, err := pollForWork(apiURL)
	if err != nil {
		log.Fatalf("failed to get work: %v", err)
	}

	log.Printf("job assigned: %s/%s", job.Metadata.Namespace, job.Metadata.Name)

	// Start heartbeat
	ctx := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeat(apiURL, ctx)
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
	close(ctx)
	wg.Wait()

	// Report completion
	reportComplete(apiURL, exitCode, execErr)

	log.Printf("job finished with exit code %d", exitCode)
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
	// Clone the repo, cd into it, run the build script.
	buildCmd := fmt.Sprintf(
		"set -e; git clone %s /build && cd /build && chmod +x ./%s && ./%s",
		shellQuote(job.Spec.Repo),
		shellQuote(job.Spec.BuildScript),
		shellQuote(job.Spec.BuildScript),
	)

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

	// Capture output
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

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

// shellQuote wraps a string in single quotes for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
