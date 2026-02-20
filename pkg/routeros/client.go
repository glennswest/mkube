package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/glennswest/mkube/pkg/config"
)

// Client wraps both the RouterOS REST API and the RouterOS protocol API.
// The REST API (available since RouterOS 7.1) is preferred for container
// management. The protocol API (port 8728) is used for operations not
// yet exposed via REST.
type Client struct {
	cfg        config.RouterOSConfig
	httpClient *http.Client
}

// Container represents a RouterOS container as returned by /rest/container.
type Container struct {
	ID          string `json:".id"`
	Name        string `json:"name"`
	Tag         string `json:"tag"`  // image tag, e.g. "docker.io/library/microdns:arm64"
	Image       string `json:"file"` // tarball path on RouterOS
	Interface   string `json:"interface"`
	RootDir     string `json:"root-dir"`
	MountLists  string `json:"mountlists"`
	Cmd         string `json:"cmd,omitempty"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	Running     string `json:"running,omitempty"` // "true" if running
	Stopped     string `json:"stopped,omitempty"` // "true" if stopped
	Logging     string `json:"logging"`
	WorkDir     string `json:"workdir,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	DNS         string `json:"dns,omitempty"`
	StartOnBoot string `json:"start-on-boot"`
}

// IsRunning returns true if the container is currently running.
func (c Container) IsRunning() bool {
	return c.Running == "true"
}

// IsStopped returns true if the container is stopped.
func (c Container) IsStopped() bool {
	return c.Stopped == "true"
}

// ContainerSpec is used to create/update a container.
type ContainerSpec struct {
	Name        string `json:"name"`
	File        string `json:"file,omitempty"` // tarball path
	Tag         string `json:"tag,omitempty"`  // registry image ref, alternative to File
	Interface   string `json:"interface"`
	RootDir     string `json:"root-dir"`
	MountLists  string `json:"mountlists,omitempty"`
	Cmd         string `json:"cmd,omitempty"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	WorkDir     string `json:"workdir,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	DNS         string `json:"dns,omitempty"`
	Logging     string `json:"logging"`
	StartOnBoot string `json:"start-on-boot"`
}

// NetworkInterface represents a veth interface for containers.
type NetworkInterface struct {
	ID      string `json:".id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Gateway string `json:"gateway"`
	Bridge  string `json:"bridge"`
}

// NewClient creates a new RouterOS API client.
func NewClient(cfg config.RouterOSConfig) (*Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureVerify,
		},
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}, nil
}

// Close releases resources held by the client.
func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// ─── Container Operations ───────────────────────────────────────────────────

// ListContainers returns all containers on the RouterOS device.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var containers []Container
	err := c.restGET(ctx, "/container", &containers)
	return containers, err
}

// GetContainer returns a single container by name.
func (c *Client) GetContainer(ctx context.Context, name string) (*Container, error) {
	containers, err := c.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, ct := range containers {
		if ct.Name == name {
			return &ct, nil
		}
	}
	return nil, fmt.Errorf("container %q not found", name)
}

// CreateContainer creates a new container from a spec.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	return c.restPOST(ctx, "/container/add", spec, nil)
}

// RemoveContainer removes a container by its RouterOS .id.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/container/remove", map[string]string{".id": id}, nil)
}

// StartContainer starts a container by its RouterOS .id.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/container/start", map[string]string{".id": id}, nil)
}

// StopContainer stops a container by its RouterOS .id.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/container/stop", map[string]string{".id": id}, nil)
}

// ─── Mount Operations ────────────────────────────────────────────────────

// MountEntry represents a container mount point on RouterOS.
type MountEntry struct {
	ID   string `json:".id"`
	List string `json:"list"` // mount list name
	Src  string `json:"src"`  // host path
	Dst  string `json:"dst"`  // container path
}

// CreateMount creates a container mount entry.
func (c *Client) CreateMount(ctx context.Context, listName, src, dst string) error {
	return c.restPOST(ctx, "/container/mounts/add", map[string]string{
		"list": listName,
		"src":  src,
		"dst":  dst,
	}, nil)
}

// ListMounts returns all container mount entries.
func (c *Client) ListMounts(ctx context.Context) ([]MountEntry, error) {
	var mounts []MountEntry
	err := c.restGET(ctx, "/container/mounts", &mounts)
	return mounts, err
}

// RemoveMountsByList removes all mount entries with the given list name.
func (c *Client) RemoveMountsByList(ctx context.Context, listName string) error {
	mounts, err := c.ListMounts(ctx)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if m.List == listName {
			if err := c.restPOST(ctx, "/container/mounts/remove", map[string]string{".id": m.ID}, nil); err != nil {
				return fmt.Errorf("removing mount %s: %w", m.ID, err)
			}
		}
	}
	return nil
}

// ─── Network Operations ─────────────────────────────────────────────────────

// CreateVeth creates a virtual ethernet interface for a container.
func (c *Client) CreateVeth(ctx context.Context, name, address, gateway string) error {
	return c.restPOST(ctx, "/interface/veth/add", map[string]string{
		"name":    name,
		"address": address,
		"gateway": gateway,
	}, nil)
}

// RemoveVeth removes a virtual ethernet interface by name.
// It looks up the veth's .id first, since RouterOS requires .id for removal.
func (c *Client) RemoveVeth(ctx context.Context, name string) error {
	veths, err := c.ListVeths(ctx)
	if err != nil {
		return fmt.Errorf("listing veths to find %q: %w", name, err)
	}
	for _, v := range veths {
		if v.Name == name {
			return c.restPOST(ctx, "/interface/veth/remove", map[string]string{".id": v.ID}, nil)
		}
	}
	return nil // already gone
}

// AddBridgePort adds a veth to a bridge.
func (c *Client) AddBridgePort(ctx context.Context, bridge, iface string) error {
	return c.restPOST(ctx, "/interface/bridge/port/add", map[string]string{
		"bridge":    bridge,
		"interface": iface,
	}, nil)
}

// ListVeths returns all veth interfaces.
func (c *Client) ListVeths(ctx context.Context) ([]NetworkInterface, error) {
	var veths []NetworkInterface
	err := c.restGET(ctx, "/interface/veth", &veths)
	return veths, err
}

// ─── File Operations ────────────────────────────────────────────────────────

// UploadFile uploads a tarball or file to the RouterOS filesystem.
func (c *Client) UploadFile(ctx context.Context, remotePath string, data io.Reader) error {
	// RouterOS file upload via REST API uses /rest/file
	// For large tarballs, we use the FTP/SFTP approach or the REST upload endpoint.
	// This is a simplified version using REST.
	body, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("reading file data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT",
		fmt.Sprintf("%s/file/%s", c.cfg.RESTURL, remotePath),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// ListFiles lists files at a given path on RouterOS.
func (c *Client) ListFiles(ctx context.Context, path string) ([]map[string]interface{}, error) {
	var files []map[string]interface{}
	err := c.restPOST(ctx, "/file/print", map[string]string{
		".query": fmt.Sprintf("name=%s", path),
	}, &files)
	return files, err
}

// RemoveFile deletes a file from the RouterOS filesystem.
func (c *Client) RemoveFile(ctx context.Context, path string) error {
	return c.restPOST(ctx, "/file/remove", map[string]string{
		"name": path,
	}, nil)
}

// ─── Bridge Operations ──────────────────────────────────────────────────────

// BridgePort represents a bridge port assignment.
type BridgePort struct {
	ID        string `json:".id"`
	Bridge    string `json:"bridge"`
	Interface string `json:"interface"`
}

// ListBridgePorts returns all bridge port assignments.
func (c *Client) ListBridgePorts(ctx context.Context) ([]BridgePort, error) {
	var ports []BridgePort
	err := c.restGET(ctx, "/interface/bridge/port", &ports)
	return ports, err
}

// Bridge represents a bridge interface.
type Bridge struct {
	ID   string `json:".id"`
	Name string `json:"name"`
}

// ListBridges returns all bridge interfaces.
func (c *Client) ListBridges(ctx context.Context) ([]Bridge, error) {
	var bridges []Bridge
	err := c.restGET(ctx, "/interface/bridge", &bridges)
	return bridges, err
}

// ─── EoIP Tunnel Operations ──────────────────────────────────────────────────

// CreateEoIPTunnel creates an EoIP tunnel interface.
func (c *Client) CreateEoIPTunnel(ctx context.Context, name, localIP, remoteIP string, tunnelID int) error {
	return c.restPOST(ctx, "/interface/eoip/add", map[string]interface{}{
		"name":           name,
		"local-address":  localIP,
		"remote-address": remoteIP,
		"tunnel-id":      fmt.Sprintf("%d", tunnelID),
	}, nil)
}

// DeleteEoIPTunnel removes an EoIP tunnel interface by name.
func (c *Client) DeleteEoIPTunnel(ctx context.Context, name string) error {
	return c.restPOST(ctx, "/interface/eoip/remove", map[string]string{
		"name": name,
	}, nil)
}

// ─── Bridge VLAN Operations ──────────────────────────────────────────────────

// AddBridgeVLAN adds a VLAN entry on a bridge port.
func (c *Client) AddBridgeVLAN(ctx context.Context, bridge string, vid int, tagged, pvid, untagged bool) error {
	body := map[string]interface{}{
		"bridge":   bridge,
		"vlan-ids": fmt.Sprintf("%d", vid),
	}
	if tagged {
		body["tagged"] = bridge
	}
	if untagged {
		body["untagged"] = bridge
	}
	return c.restPOST(ctx, "/interface/bridge/vlan/add", body, nil)
}

// RemoveBridgeVLAN removes a VLAN entry from a bridge.
func (c *Client) RemoveBridgeVLAN(ctx context.Context, bridge string, vid int) error {
	// Need to find the .id first
	var entries []struct {
		ID      string `json:".id"`
		VLANIDs string `json:"vlan-ids"`
		Bridge  string `json:"bridge"`
	}
	if err := c.restGET(ctx, "/interface/bridge/vlan", &entries); err != nil {
		return err
	}

	for _, e := range entries {
		if e.Bridge == bridge && e.VLANIDs == fmt.Sprintf("%d", vid) {
			return c.restPOST(ctx, "/interface/bridge/vlan/remove", map[string]string{".id": e.ID}, nil)
		}
	}
	return fmt.Errorf("VLAN %d not found on bridge %s", vid, bridge)
}

// ─── IP Address Operations ──────────────────────────────────────────────────

// IPAddress represents an IP address assignment on an interface.
type IPAddress struct {
	ID        string `json:".id"`
	Address   string `json:"address"`
	Interface string `json:"interface"`
	Network   string `json:"network"`
}

// ListIPAddresses returns all IP address assignments.
func (c *Client) ListIPAddresses(ctx context.Context) ([]IPAddress, error) {
	var addrs []IPAddress
	err := c.restGET(ctx, "/ip/address", &addrs)
	return addrs, err
}

// ─── System Operations ──────────────────────────────────────────────────────

// SystemResource represents system resource information.
type SystemResource struct {
	Uptime       string `json:"uptime"`
	CPUCount     string `json:"cpu-count"`
	CPULoad      string `json:"cpu-load"`
	FreeMemory   string `json:"free-memory"`
	TotalMemory  string `json:"total-memory"`
	Architecture string `json:"architecture-name"`
	BoardName    string `json:"board-name"`
	Version      string `json:"version"`
	Platform     string `json:"platform"`
}

// GetSystemResource returns system resource information.
func (c *Client) GetSystemResource(ctx context.Context) (*SystemResource, error) {
	var resource SystemResource
	err := c.restGET(ctx, "/system/resource", &resource)
	if err != nil {
		return nil, err
	}
	return &resource, nil
}

// ─── Log Operations ─────────────────────────────────────────────────────────

// LogEntry represents a RouterOS log entry.
type LogEntry struct {
	ID      string `json:".id"`
	Time    string `json:"time"`
	Topics  string `json:"topics"`
	Message string `json:"message"`
}

// GetLogs returns log entries. Use topic filter like "container" to narrow results.
func (c *Client) GetLogs(ctx context.Context) ([]LogEntry, error) {
	var logs []LogEntry
	err := c.restGET(ctx, "/log", &logs)
	return logs, err
}

// ─── REST Helpers ───────────────────────────────────────────────────────────

func (c *Client) restGET(ctx context.Context, path string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.RESTURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("REST GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("REST GET %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func (c *Client) restPOST(ctx context.Context, path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.RESTURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("REST POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("REST POST %s returned %d: %s", path, resp.StatusCode, string(b))
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}
