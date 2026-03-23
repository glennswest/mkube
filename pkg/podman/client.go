// Package podman provides a minimal client for the Podman REST API via Unix socket.
// No external dependencies — uses net/http with a Unix socket transport.
package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultSocket = "/run/podman/podman.sock"
const apiBase = "http://d/v5.0.0/libpod"

// Client talks to the Podman REST API over a Unix socket.
type Client struct {
	http   *http.Client
	socket string
}

// New creates a Client connected to the given Unix socket path.
func New(socketPath string) *Client {
	if socketPath == "" {
		socketPath = defaultSocket
	}
	return &Client{
		socket: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.DialTimeout("unix", socketPath, 10*time.Second)
				},
			},
			Timeout: 0, // per-request timeouts via context
		},
	}
}

// ── Info ────────────────────────────────────────────────────────────────────

// PodmanInfo holds the fields we care about from `podman info`.
type PodmanInfo struct {
	Version       string
	StorageDriver string
	StoragePath   string
	CgroupVersion string
	OS            string
	Arch          string
}

// Info returns podman system information.
func (c *Client) Info(ctx context.Context) (*PodmanInfo, error) {
	body, err := c.get(ctx, "/info", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("podman info: %w", err)
	}
	defer body.Close()

	var raw struct {
		Host struct {
			Arch          string `json:"arch"`
			OS            string `json:"os"`
			CgroupVersion string `json:"cgroupVersion"`
		} `json:"host"`
		Store struct {
			GraphDriverName string `json:"graphDriverName"`
			GraphRoot       string `json:"graphRoot"`
		} `json:"store"`
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("podman info decode: %w", err)
	}

	return &PodmanInfo{
		Version:       raw.Version.Version,
		StorageDriver: raw.Store.GraphDriverName,
		StoragePath:   raw.Store.GraphRoot,
		CgroupVersion: raw.Host.CgroupVersion,
		OS:            raw.Host.OS,
		Arch:          raw.Host.Arch,
	}, nil
}

// ── Images ──────────────────────────────────────────────────────────────────

// ImageInfo describes a local container image.
type ImageInfo struct {
	Names []string
	Size  int64
	Arch  string
}

// Images lists local images.
func (c *Client) Images(ctx context.Context) ([]ImageInfo, error) {
	body, err := c.get(ctx, "/images/json", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("podman images: %w", err)
	}
	defer body.Close()

	var raw []struct {
		Names []string `json:"Names"`
		Size  int64    `json:"Size"`
		Arch  string   `json:"Arch"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("podman images decode: %w", err)
	}

	out := make([]ImageInfo, len(raw))
	for i, r := range raw {
		out[i] = ImageInfo{Names: r.Names, Size: r.Size, Arch: r.Arch}
	}
	return out, nil
}

// ── Pull ────────────────────────────────────────────────────────────────────

// Pull downloads an image. Set tlsVerify=false for insecure registries.
func (c *Client) Pull(ctx context.Context, image string, tlsVerify bool) error {
	params := url.Values{
		"reference": {image},
		"tlsVerify": {fmt.Sprintf("%v", tlsVerify)},
	}

	ctx2, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx2, "POST", apiBase+"/images/pull?"+params.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("podman pull %s: %w", image, err)
	}
	defer resp.Body.Close()

	// Pull streams JSON objects; read them all to completion
	dec := json.NewDecoder(resp.Body)
	var lastErr string
	for {
		var msg struct {
			Error string `json:"error"`
			ID    string `json:"id"`
		}
		if err := dec.Decode(&msg); err != nil {
			break
		}
		if msg.Error != "" {
			lastErr = msg.Error
		}
	}
	if lastErr != "" {
		return fmt.Errorf("podman pull %s: %s", image, lastErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("podman pull %s: status %d", image, resp.StatusCode)
	}
	return nil
}

// ── Container Create + Start + Wait + Logs (= podman run) ───────────────────

// ContainerConfig describes a container to create.
type ContainerConfig struct {
	Name       string
	Image      string
	Command    []string
	Env        map[string]string
	Mounts     []Mount
	Privileged bool
	Remove     bool // --rm
	DNS        []string
}

// Mount describes a bind mount.
type Mount struct {
	Source   string
	Dest    string
	Options []string // e.g. ["ro"]
}

// RunResult holds the outcome of a container run.
type RunResult struct {
	ExitCode int
	Logs     []byte
}

// Run creates, starts, waits, and captures logs from a container (like podman run).
// The onLog callback is called with log chunks as they arrive (may be nil).
func (c *Client) Run(ctx context.Context, cfg ContainerConfig, onLog func([]byte)) (*RunResult, error) {
	id, err := c.createContainer(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Always clean up
	if cfg.Remove {
		defer c.RemoveContainer(context.Background(), id, true)
	}

	if err := c.startContainer(ctx, id); err != nil {
		return nil, err
	}

	// Stream logs in background
	logsDone := make(chan []byte, 1)
	go func() {
		data := c.streamLogs(ctx, id, onLog)
		logsDone <- data
	}()

	exitCode, err := c.waitContainer(ctx, id)
	if err != nil {
		return nil, err
	}

	logs := <-logsDone
	return &RunResult{ExitCode: exitCode, Logs: logs}, nil
}

func (c *Client) createContainer(ctx context.Context, cfg ContainerConfig) (string, error) {
	// Build the spec
	env := make(map[string]string)
	for k, v := range cfg.Env {
		env[k] = v
	}

	mounts := make([]map[string]interface{}, 0, len(cfg.Mounts))
	for _, m := range cfg.Mounts {
		mt := map[string]interface{}{
			"Type":        "bind",
			"Source":      m.Source,
			"Destination": m.Dest,
		}
		if len(m.Options) > 0 {
			mt["Options"] = m.Options
		}
		mounts = append(mounts, mt)
	}

	spec := map[string]interface{}{
		"name":       cfg.Name,
		"image":      cfg.Image,
		"command":    cfg.Command,
		"env":        env,
		"mounts":     mounts,
		"privileged": cfg.Privileged,
		"remove":     cfg.Remove,
	}
	if len(cfg.DNS) > 0 {
		spec["dns_server"] = cfg.DNS
	}

	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	respBody, err := c.post(ctx2, "/containers/create", spec)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	defer respBody.Close()

	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.NewDecoder(respBody).Decode(&result); err != nil {
		return "", fmt.Errorf("create container decode: %w", err)
	}
	return result.ID, nil
}

func (c *Client) startContainer(ctx context.Context, id string) error {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	respBody, err := c.post(ctx2, "/containers/"+id+"/start", nil)
	if err != nil {
		return fmt.Errorf("start container %s: %w", id[:12], err)
	}
	respBody.Close()
	return nil
}

func (c *Client) waitContainer(ctx context.Context, id string) (int, error) {
	// Wait can take a long time (build duration)
	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+"/containers/"+id+"/wait", nil)
	if err != nil {
		return -1, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return -1, fmt.Errorf("wait container %s: %w", id[:12], err)
	}
	defer resp.Body.Close()

	var result struct {
		StatusCode int    `json:"StatusCode"`
		Error      string `json:"Error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return -1, fmt.Errorf("wait decode: %w", err)
	}
	if result.Error != "" {
		return result.StatusCode, fmt.Errorf("wait: %s", result.Error)
	}
	return result.StatusCode, nil
}

func (c *Client) streamLogs(ctx context.Context, id string, onLog func([]byte)) []byte {
	params := url.Values{
		"follow": {"true"},
		"stdout": {"true"},
		"stderr": {"true"},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+"/containers/"+id+"/logs?"+params.Encode(), nil)
	if err != nil {
		return nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var all []byte
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			// Podman log stream has 8-byte header per frame: [stream_type, 0, 0, 0, size(4 bytes)]
			// Parse frames to extract raw log data
			chunk := extractLogData(buf[:n])
			all = append(all, chunk...)
			if onLog != nil {
				onLog(chunk)
			}
		}
		if err != nil {
			break
		}
	}
	return all
}

// extractLogData strips the 8-byte multiplexed stream headers from podman log output.
// Each frame: [1 byte stream type] [3 bytes padding] [4 bytes big-endian size] [payload]
func extractLogData(raw []byte) []byte {
	var out []byte
	for len(raw) >= 8 {
		// Read 4-byte big-endian size from bytes 4-7
		size := int(raw[4])<<24 | int(raw[5])<<16 | int(raw[6])<<8 | int(raw[7])
		raw = raw[8:]
		if size > len(raw) {
			// Partial frame — take what we have
			out = append(out, raw...)
			break
		}
		out = append(out, raw[:size]...)
		raw = raw[size:]
	}
	// If no frames were parsed, return raw (might be non-multiplexed)
	if len(out) == 0 && len(raw) > 0 {
		return raw
	}
	return out
}

// ── Remove / Prune ──────────────────────────────────────────────────────────

// RemoveContainer removes a container by ID or name.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	params := url.Values{"force": {fmt.Sprintf("%v", force)}}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx2, "DELETE", apiBase+"/containers/"+id+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PruneContainers removes all stopped containers.
func (c *Client) PruneContainers(ctx context.Context) (int, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, err := c.post(ctx2, "/containers/prune", nil)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	var result []struct {
		ID    string `json:"Id"`
		Error string `json:"Err"`
		Size  int64  `json:"Size"`
	}
	json.NewDecoder(body).Decode(&result)
	return len(result), nil
}

// PruneImages removes unused images. If all=true, removes all unused (not just dangling).
// filter can be empty or e.g. "until=24h".
func (c *Client) PruneImages(ctx context.Context, all bool, filter string) (int, error) {
	params := url.Values{"all": {fmt.Sprintf("%v", all)}}
	if filter != "" {
		params.Set("filters", fmt.Sprintf(`{"until":["%s"]}`, filter))
	}

	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx2, "POST", apiBase+"/images/prune?"+params.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result []struct {
		ID    string `json:"Id"`
		Error string `json:"Err"`
		Size  int64  `json:"Size"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return len(result), nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, path string, timeout time.Duration) (io.ReadCloser, error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	_ = cancel // caller closes body which triggers cancel

	req, err := http.NewRequestWithContext(ctx2, "GET", apiBase+path, nil)
	if err != nil {
		cancel()
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

func (c *Client) post(ctx context.Context, path string, body interface{}) (io.ReadCloser, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("POST %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return resp.Body, nil
}
