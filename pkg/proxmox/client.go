// Package proxmox implements a REST API client for Proxmox VE.
// It wraps the PVE API for LXC container lifecycle management.
package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client wraps the Proxmox VE REST API (port 8006).
type Client struct {
	baseURL    string // e.g. "https://pvex.gw.lo:8006/api2/json"
	tokenID    string // e.g. "mkube@pve!mkube-token"
	tokenSecret string
	node       string // e.g. "pvex"
	httpClient *http.Client
	vmids      *VMIDAllocator
}

// ClientConfig holds connection settings for a Proxmox VE node.
type ClientConfig struct {
	URL            string // e.g. "https://pvex.gw.lo:8006"
	TokenID        string // e.g. "mkube@pve!mkube-token"
	TokenSecret    string // UUID
	Node           string // e.g. "pvex"
	InsecureVerify bool   // allow self-signed certs
	VMIDRange      string // e.g. "200-299"
	Storage        string // template storage, e.g. "local"
	RootFSStorage  string // container rootfs storage, e.g. "local-lvm"
	RootFSSize     string // default rootfs size GB, e.g. "8"
}

// NewClient creates a Proxmox VE API client.
func NewClient(cfg ClientConfig) (*Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureVerify,
		},
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	vmidAlloc, err := NewVMIDAllocator(cfg.VMIDRange)
	if err != nil {
		return nil, fmt.Errorf("parsing VMID range: %w", err)
	}

	return &Client{
		baseURL:     strings.TrimRight(cfg.URL, "/") + "/api2/json",
		tokenID:     cfg.TokenID,
		tokenSecret: cfg.TokenSecret,
		node:        cfg.Node,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		vmids: vmidAlloc,
	}, nil
}

// Close releases any resources held by the client.
func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// ─── Types ──────────────────────────────────────────────────────────────────

// LXCContainer represents a Proxmox LXC container status response.
type LXCContainer struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"` // "running", "stopped"
	Uptime int64  `json:"uptime"`
	CPU    float64 `json:"cpu"`
	Mem    int64  `json:"mem"`
	MaxMem int64  `json:"maxmem"`
	Disk   int64  `json:"disk"`
	MaxDisk int64 `json:"maxdisk"`
	NetIn  int64  `json:"netin"`
	NetOut int64  `json:"netout"`
}

// LXCConfig represents a Proxmox LXC container configuration.
type LXCConfig struct {
	Hostname     string `json:"hostname"`
	OSTemplate   string `json:"ostemplate"`
	Net0         string `json:"net0"`
	Net1         string `json:"net1,omitempty"`
	Memory       int    `json:"memory"`
	Swap         int    `json:"swap"`
	Cores        int    `json:"cores"`
	RootFS       string `json:"rootfs"`
	Unprivileged int    `json:"unprivileged"`
	Features     string `json:"features"`
	Nameserver   string `json:"nameserver"`
	Searchdomain string `json:"searchdomain"`
	OnBoot       int    `json:"onboot"`

	// Mount points (mp0, mp1, ...)
	MP0 string `json:"mp0,omitempty"`
	MP1 string `json:"mp1,omitempty"`
	MP2 string `json:"mp2,omitempty"`
	MP3 string `json:"mp3,omitempty"`
	MP4 string `json:"mp4,omitempty"`
}

// LXCCreateSpec describes parameters for creating an LXC container.
type LXCCreateSpec struct {
	VMID         int
	Hostname     string
	OSTemplate   string // e.g. "local:vztmpl/rootfs.tar.gz"
	Net0         string // e.g. "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1"
	Memory       int    // MB
	Swap         int    // MB
	Cores        int
	RootFS       string // e.g. "local-lvm:8"
	Unprivileged bool
	Features     string // e.g. "nesting=1"
	Nameserver   string
	Searchdomain string
	OnBoot       bool
	Start        bool

	// Mount points
	MountPoints map[int]string // mp index → mount spec
}

// NodeStatus represents Proxmox node system information.
type NodeStatus struct {
	Uptime  int64   `json:"uptime"`
	CPU     float64 `json:"cpu"`
	CPUInfo struct {
		Cores   int    `json:"cores"`
		CPUs    int    `json:"cpus"`
		Model   string `json:"model"`
		Sockets int    `json:"sockets"`
	} `json:"cpuinfo"`
	Memory struct {
		Free  int64 `json:"free"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"memory"`
	RootFS struct {
		Avail int64 `json:"avail"`
		Free  int64 `json:"free"`
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"rootfs"`
	PVEVersion string `json:"pveversion"`
}

// NetworkInterface represents a Proxmox network interface.
type NetworkInterface struct {
	Name     string `json:"iface"`
	Type     string `json:"type"` // "bridge", "eth", "bond", etc.
	Active   int    `json:"active"`
	Bridge   string `json:"bridge_ports,omitempty"`
	CIDR     string `json:"cidr,omitempty"`
	Address  string `json:"address,omitempty"`
	Gateway  string `json:"gateway,omitempty"`
	Netmask  string `json:"netmask,omitempty"`
}

// LogLine represents a single log line from a container.
type LogLine struct {
	LineNo int    `json:"n"`
	Text   string `json:"t"`
}

// VersionInfo represents the PVE version response.
type VersionInfo struct {
	Release string `json:"release"` // e.g. "8.3"
	RepID   string `json:"repoid"`
	Version string `json:"version"` // e.g. "8.3.2"
}

// ─── Container Operations ───────────────────────────────────────────────────

// ListContainers returns all LXC containers on the node.
func (c *Client) ListContainers(ctx context.Context) ([]LXCContainer, error) {
	path := fmt.Sprintf("/nodes/%s/lxc", c.node)
	var containers []LXCContainer
	if err := c.get(ctx, path, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// GetContainerStatus returns the current status of an LXC container.
func (c *Client) GetContainerStatus(ctx context.Context, vmid int) (*LXCContainer, error) {
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/current", c.node, vmid)
	var ct LXCContainer
	if err := c.get(ctx, path, &ct); err != nil {
		return nil, err
	}
	return &ct, nil
}

// GetContainerConfig returns the configuration of an LXC container.
func (c *Client) GetContainerConfig(ctx context.Context, vmid int) (*LXCConfig, error) {
	path := fmt.Sprintf("/nodes/%s/lxc/%d/config", c.node, vmid)
	var cfg LXCConfig
	if err := c.get(ctx, path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// CreateContainer creates an LXC container and waits for the task to complete.
func (c *Client) CreateContainer(ctx context.Context, spec LXCCreateSpec) error {
	path := fmt.Sprintf("/nodes/%s/lxc", c.node)

	params := map[string]string{
		"vmid":       strconv.Itoa(spec.VMID),
		"hostname":   spec.Hostname,
		"ostemplate": spec.OSTemplate,
		"net0":       spec.Net0,
		"memory":     strconv.Itoa(spec.Memory),
		"swap":       strconv.Itoa(spec.Swap),
		"cores":      strconv.Itoa(spec.Cores),
		"rootfs":     spec.RootFS,
		"start":      boolToStr(spec.Start),
		"onboot":     boolToStr(spec.OnBoot),
	}

	if spec.Unprivileged {
		params["unprivileged"] = "1"
	}
	if spec.Features != "" {
		params["features"] = spec.Features
	}
	if spec.Nameserver != "" {
		params["nameserver"] = spec.Nameserver
	}
	if spec.Searchdomain != "" {
		params["searchdomain"] = spec.Searchdomain
	}

	// Add mount points
	for idx, mp := range spec.MountPoints {
		params[fmt.Sprintf("mp%d", idx)] = mp
	}

	upid, err := c.post(ctx, path, params)
	if err != nil {
		return fmt.Errorf("creating container VMID %d: %w", spec.VMID, err)
	}

	return c.waitForTask(ctx, upid, 120*time.Second)
}

// StartContainer starts an LXC container.
func (c *Client) StartContainer(ctx context.Context, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/start", c.node, vmid)
	upid, err := c.post(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("starting container VMID %d: %w", vmid, err)
	}
	return c.waitForTask(ctx, upid, 60*time.Second)
}

// StopContainer stops an LXC container.
func (c *Client) StopContainer(ctx context.Context, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/lxc/%d/status/stop", c.node, vmid)
	upid, err := c.post(ctx, path, nil)
	if err != nil {
		return fmt.Errorf("stopping container VMID %d: %w", vmid, err)
	}
	return c.waitForTask(ctx, upid, 60*time.Second)
}

// RemoveContainer deletes an LXC container (must be stopped first).
func (c *Client) RemoveContainer(ctx context.Context, vmid int) error {
	path := fmt.Sprintf("/nodes/%s/lxc/%d", c.node, vmid)
	upid, err := c.delete(ctx, path)
	if err != nil {
		return fmt.Errorf("removing container VMID %d: %w", vmid, err)
	}
	return c.waitForTask(ctx, upid, 60*time.Second)
}

// GetContainerLogs returns the log output of a container.
func (c *Client) GetContainerLogs(ctx context.Context, vmid int) ([]LogLine, error) {
	path := fmt.Sprintf("/nodes/%s/lxc/%d/log?limit=100", c.node, vmid)
	var logs []LogLine
	if err := c.get(ctx, path, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// ─── Node Operations ────────────────────────────────────────────────────────

// GetNodeStatus returns system resource information for the node.
func (c *Client) GetNodeStatus(ctx context.Context) (*NodeStatus, error) {
	path := fmt.Sprintf("/nodes/%s/status", c.node)
	var status NodeStatus
	if err := c.get(ctx, path, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// GetVersion returns the PVE version information.
func (c *Client) GetVersion(ctx context.Context) (*VersionInfo, error) {
	var version VersionInfo
	if err := c.get(ctx, "/version", &version); err != nil {
		return nil, err
	}
	return &version, nil
}

// ListNetworkInterfaces returns all network interfaces on the node.
func (c *Client) ListNetworkInterfaces(ctx context.Context) ([]NetworkInterface, error) {
	path := fmt.Sprintf("/nodes/%s/network", c.node)
	var ifaces []NetworkInterface
	if err := c.get(ctx, path, &ifaces); err != nil {
		return nil, err
	}
	return ifaces, nil
}

// ─── Storage / Template Operations ──────────────────────────────────────────

// UploadTemplate uploads a template file to Proxmox storage.
func (c *Client) UploadTemplate(ctx context.Context, storage, filename string, data io.Reader) error {
	path := fmt.Sprintf("/nodes/%s/storage/%s/upload", c.node, storage)
	url := c.baseURL + path

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("content", "vztmpl"); err != nil {
		return fmt.Errorf("writing content field: %w", err)
	}

	part, err := writer.CreateFormFile("filename", filename)
	if err != nil {
		return fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(part, data); err != nil {
		return fmt.Errorf("copying template data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading template: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload template %s returned %d: %s", filename, resp.StatusCode, string(b))
	}

	// Parse UPID from response and wait for upload to complete
	var result struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil // Upload succeeded but no UPID to track
	}
	if result.Data != "" {
		return c.waitForTask(ctx, result.Data, 120*time.Second)
	}

	return nil
}

// RemoveTemplate removes a template from Proxmox storage.
func (c *Client) RemoveTemplate(ctx context.Context, storage, volid string) error {
	path := fmt.Sprintf("/nodes/%s/storage/%s/content/%s", c.node, storage, volid)
	_, err := c.delete(ctx, path)
	return err
}

// ─── VMID Management ────────────────────────────────────────────────────────

// VMIDs returns the VMID allocator for direct access.
func (c *Client) VMIDs() *VMIDAllocator {
	return c.vmids
}

// SyncVMIDs queries existing containers and marks their VMIDs as used.
func (c *Client) SyncVMIDs(ctx context.Context) error {
	containers, err := c.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("syncing VMIDs: %w", err)
	}

	for _, ct := range containers {
		c.vmids.MarkUsed(ct.VMID, ct.Name)
	}
	return nil
}

// ─── Async Task Polling ─────────────────────────────────────────────────────

// waitForTask polls the Proxmox task status until it completes or times out.
func (c *Client) waitForTask(ctx context.Context, upid string, timeout time.Duration) error {
	if upid == "" {
		return nil
	}

	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", c.node, upid)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s timed out after %v", upid, timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var status struct {
			Status   string `json:"status"`   // "running", "stopped"
			ExitCode string `json:"exitstatus"` // "OK" on success
		}
		if err := c.get(ctx, path, &status); err != nil {
			return fmt.Errorf("polling task %s: %w", upid, err)
		}

		if status.Status == "stopped" {
			if status.ExitCode != "OK" {
				return fmt.Errorf("task %s failed: %s", upid, status.ExitCode)
			}
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

// ─── HTTP Helpers ───────────────────────────────────────────────────────────

func (c *Client) authHeader() string {
	return fmt.Sprintf("PVEAPIToken=%s=%s", c.tokenID, c.tokenSecret)
}

// get performs a GET request and decodes the "data" field of the response.
func (c *Client) get(ctx context.Context, path string, result interface{}) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	// Proxmox wraps all responses in {"data": ...}
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return fmt.Errorf("decoding response for %s: %w", path, err)
	}

	if result != nil && len(wrapper.Data) > 0 {
		if err := json.Unmarshal(wrapper.Data, result); err != nil {
			return fmt.Errorf("unmarshaling data for %s: %w", path, err)
		}
	}
	return nil
}

// post performs a POST request and returns the UPID string (for async tasks).
func (c *Client) post(ctx context.Context, path string, params map[string]string) (string, error) {
	url := c.baseURL + path

	var body io.Reader
	if params != nil {
		values := make([]string, 0, len(params))
		for k, v := range params {
			values = append(values, fmt.Sprintf("%s=%s", k, v))
		}
		body = strings.NewReader(strings.Join(values, "&"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	if params != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return "", nil // Response may not have JSON body
	}

	// Data is typically a UPID string for async operations
	var upid string
	if len(wrapper.Data) > 0 {
		_ = json.Unmarshal(wrapper.Data, &upid)
	}

	return upid, nil
}

// delete performs a DELETE request and returns the UPID string.
func (c *Client) delete(ctx context.Context, path string) (string, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("DELETE %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return "", nil
	}

	var upid string
	if len(wrapper.Data) > 0 {
		_ = json.Unmarshal(wrapper.Data, &upid)
	}

	return upid, nil
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
