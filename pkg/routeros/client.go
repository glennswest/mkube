package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	Comment     string `json:"comment,omitempty"` // error message when stopped (e.g. "could not acquire interface")
	Logging     string `json:"logging"`
	WorkDir     string `json:"workdir,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	DNS         string `json:"dns,omitempty"`
	User        string `json:"user,omitempty"`
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
	File        string `json:"file,omitempty"`         // tarball path on RouterOS filesystem
	RemoteImage string `json:"remote-image,omitempty"` // registry image ref (RouterOS pulls directly)
	Interface   string `json:"interface"`
	RootDir     string `json:"root-dir"`
	MountLists  string `json:"mountlists,omitempty"`
	Cmd         string `json:"cmd,omitempty"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	WorkDir     string `json:"workdir,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	DNS         string `json:"dns,omitempty"`
	User        string `json:"user,omitempty"`
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
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 4,
		MaxConnsPerHost:     8,
		IdleConnTimeout:     30 * time.Second,
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
// Idempotent: if the veth already exists with matching config, returns nil.
// If it exists with different address/gateway, updates it in place.
func (c *Client) CreateVeth(ctx context.Context, name, address, gateway string) error {
	veths, err := c.ListVeths(ctx)
	if err != nil {
		return fmt.Errorf("listing veths for idempotent create %q: %w", name, err)
	}
	for _, v := range veths {
		if v.Name == name {
			if v.Address == address && v.Gateway == gateway {
				return nil // already exists with correct config
			}
			// exists with different config — update in place
			return c.restPOST(ctx, "/interface/veth/set", map[string]string{
				".id":     v.ID,
				"address": address,
				"gateway": gateway,
			}, nil)
		}
	}
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
// Idempotent: if the interface is already on the correct bridge, returns nil.
// If on a different bridge, removes and re-adds.
func (c *Client) AddBridgePort(ctx context.Context, bridge, iface string) error {
	ports, err := c.ListBridgePorts(ctx)
	if err != nil {
		return fmt.Errorf("listing bridge ports for idempotent add %q: %w", iface, err)
	}
	for _, p := range ports {
		if p.Interface == iface {
			if p.Bridge == bridge {
				return nil // already on correct bridge
			}
			// on wrong bridge — remove first
			if err := c.restPOST(ctx, "/interface/bridge/port/remove", map[string]string{".id": p.ID}, nil); err != nil {
				return fmt.Errorf("removing %q from bridge %q: %w", iface, p.Bridge, err)
			}
			break
		}
	}
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

// RemoveDirectory removes a directory and all its contents from the RouterOS
// filesystem. First tries a simple RemoveFile (works on RouterOS 7.x for
// non-empty directories). If that fails, falls back to listing all children
// and removing them individually before removing the parent.
func (c *Client) RemoveDirectory(ctx context.Context, path string) error {
	// Normalize path: strip leading "/" — RouterOS uses disk-relative paths
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return fmt.Errorf("refusing to remove root directory")
	}

	// Try simple removal first (works on newer RouterOS 7.x)
	if err := c.RemoveFile(ctx, path); err == nil {
		return nil
	}

	// Fallback: list all files under the path and remove individually
	var files []map[string]interface{}
	if err := c.restGET(ctx, "/file", &files); err != nil {
		return fmt.Errorf("listing files for recursive removal of %s: %w", path, err)
	}

	// Collect children sorted by path length descending (deepest first)
	type fileEntry struct {
		id   string
		name string
	}
	var children []fileEntry
	prefix := path + "/"
	for _, f := range files {
		name, _ := f["name"].(string)
		id, _ := f[".id"].(string)
		if name == path || strings.HasPrefix(name, prefix) {
			children = append(children, fileEntry{id: id, name: name})
		}
	}

	// Sort deepest-first by counting path separators (descending)
	for i := 0; i < len(children); i++ {
		for j := i + 1; j < len(children); j++ {
			if strings.Count(children[i].name, "/") < strings.Count(children[j].name, "/") {
				children[i], children[j] = children[j], children[i]
			}
		}
	}

	// Remove each child
	for _, child := range children {
		_ = c.restPOST(ctx, "/file/remove", map[string]string{".id": child.id}, nil)
	}

	// Final attempt to remove the now-empty parent
	return c.RemoveFile(ctx, path)
}

// EnsureDirectory ensures a directory exists on the RouterOS filesystem.
// RouterOS auto-creates intermediate directories when a file is uploaded.
// We upload a tiny marker file to force directory creation, then verify.
func (c *Client) EnsureDirectory(ctx context.Context, path string) error {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}

	// Check if directory already exists
	exists, err := c.FileExists(ctx, path)
	if err == nil && exists {
		return nil
	}

	// Upload a zero-byte marker file to force directory creation.
	// RouterOS creates parent directories automatically.
	markerPath := path + "/.keep"
	if uploadErr := c.UploadFile(ctx, markerPath, bytes.NewReader(nil)); uploadErr != nil {
		return fmt.Errorf("creating directory %s: %w", path, uploadErr)
	}
	return nil
}

// FileExists checks whether a file or directory exists on the RouterOS filesystem.
func (c *Client) FileExists(ctx context.Context, path string) (bool, error) {
	path = strings.TrimPrefix(path, "/")
	files, err := c.ListFiles(ctx, path)
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

// ListDirectory returns the names of entries under a directory on RouterOS.
func (c *Client) ListDirectory(ctx context.Context, path string) ([]string, error) {
	path = strings.TrimPrefix(path, "/")
	prefix := path + "/"

	var allFiles []map[string]interface{}
	if err := c.restGET(ctx, "/file", &allFiles); err != nil {
		return nil, fmt.Errorf("listing directory %s: %w", path, err)
	}

	var entries []string
	for _, f := range allFiles {
		name, _ := f["name"].(string)
		if strings.HasPrefix(name, prefix) {
			// Only direct children (no nested subdirectory entries)
			rest := strings.TrimPrefix(name, prefix)
			if !strings.Contains(rest, "/") && rest != "" {
				entries = append(entries, rest)
			}
		}
	}
	return entries, nil
}

// DirectoryDiskUsage returns the total size in bytes of all files under a directory.
func (c *Client) DirectoryDiskUsage(ctx context.Context, path string) (int64, error) {
	path = strings.TrimPrefix(path, "/")
	prefix := path + "/"

	var allFiles []map[string]interface{}
	if err := c.restGET(ctx, "/file", &allFiles); err != nil {
		return 0, fmt.Errorf("directory disk usage %s: %w", path, err)
	}

	var total int64
	for _, f := range allFiles {
		name, _ := f["name"].(string)
		if strings.HasPrefix(name, prefix) || name == path {
			switch sz := f["size"].(type) {
			case float64:
				total += int64(sz)
			case string:
				if n, err := fmt.Sscanf(sz, "%d", new(int64)); n == 1 && err == nil {
					var v int64
					fmt.Sscanf(sz, "%d", &v)
					total += v
				}
			}
		}
	}
	return total, nil
}

// FileUsageIndex is a pre-fetched index of file sizes from RouterOS /file.
// Use it to compute directory disk usage without repeated REST calls.
type FileUsageIndex struct {
	files []fileEntry
}

type fileEntry struct {
	name string
	size int64
}

// FetchFileUsageIndex fetches /file once and builds a reusable index.
func (c *Client) FetchFileUsageIndex(ctx context.Context) (*FileUsageIndex, error) {
	var allFiles []map[string]interface{}
	if err := c.restGET(ctx, "/file", &allFiles); err != nil {
		return nil, fmt.Errorf("fetching file index: %w", err)
	}
	idx := &FileUsageIndex{files: make([]fileEntry, 0, len(allFiles))}
	for _, f := range allFiles {
		name, _ := f["name"].(string)
		if name == "" {
			continue
		}
		var sz int64
		switch v := f["size"].(type) {
		case float64:
			sz = int64(v)
		case string:
			fmt.Sscanf(v, "%d", &sz)
		}
		idx.files = append(idx.files, fileEntry{name: name, size: sz})
	}
	return idx, nil
}

// DirectoryUsage computes the total size under a directory path from the index.
func (idx *FileUsageIndex) DirectoryUsage(path string) int64 {
	path = strings.TrimPrefix(path, "/")
	prefix := path + "/"
	var total int64
	for _, f := range idx.files {
		if strings.HasPrefix(f.name, prefix) || f.name == path {
			total += f.size
		}
	}
	return total
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

// CreateBridge creates a bridge interface on RouterOS.
func (c *Client) CreateBridge(ctx context.Context, name string) error {
	return c.restPOST(ctx, "/interface/bridge/add", map[string]string{
		"name": name,
	}, nil)
}

// DeleteBridge removes a bridge interface by name.
func (c *Client) DeleteBridge(ctx context.Context, name string) error {
	bridges, err := c.ListBridges(ctx)
	if err != nil {
		return fmt.Errorf("listing bridges to find %q: %w", name, err)
	}
	for _, b := range bridges {
		if b.Name == name {
			return c.restPOST(ctx, "/interface/bridge/remove", map[string]string{".id": b.ID}, nil)
		}
	}
	return nil // already gone
}

// AddIPAddress assigns an IP address to an interface.
func (c *Client) AddIPAddress(ctx context.Context, address, iface string) error {
	return c.restPOST(ctx, "/ip/address/add", map[string]string{
		"address":   address,
		"interface": iface,
	}, nil)
}

// RemoveIPAddress removes an IP address assignment by its .id.
func (c *Client) RemoveIPAddress(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/ip/address/remove", map[string]string{".id": id}, nil)
}

// RemoveIPAddressByInterface removes all IP addresses assigned to an interface.
func (c *Client) RemoveIPAddressByInterface(ctx context.Context, iface string) error {
	addrs, err := c.ListIPAddresses(ctx)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if a.Interface == iface {
			if err := c.RemoveIPAddress(ctx, a.ID); err != nil {
				return fmt.Errorf("removing IP %s from %s: %w", a.Address, iface, err)
			}
		}
	}
	return nil
}

// DHCPRelay represents a DHCP relay configuration on RouterOS.
type DHCPRelay struct {
	ID           string `json:".id"`
	Name         string `json:"name"`
	Interface    string `json:"interface"`
	DHCPServer   string `json:"dhcp-server"`
	LocalAddress string `json:"local-address"`
}

// ListDHCPRelays returns all DHCP relay configurations.
func (c *Client) ListDHCPRelays(ctx context.Context) ([]DHCPRelay, error) {
	var relays []DHCPRelay
	err := c.restGET(ctx, "/ip/dhcp-relay", &relays)
	return relays, err
}

// AddDHCPRelay creates a DHCP relay on an interface.
func (c *Client) AddDHCPRelay(ctx context.Context, name, iface, server, localAddr string) error {
	return c.restPOST(ctx, "/ip/dhcp-relay/add", map[string]string{
		"name":          name,
		"interface":     iface,
		"dhcp-server":   server,
		"local-address": localAddr,
	}, nil)
}

// RemoveDHCPRelay removes a DHCP relay by its .id.
func (c *Client) RemoveDHCPRelay(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/ip/dhcp-relay/remove", map[string]string{".id": id}, nil)
}

// RemoveDHCPRelayByInterface removes all DHCP relays on an interface.
func (c *Client) RemoveDHCPRelayByInterface(ctx context.Context, iface string) error {
	relays, err := c.ListDHCPRelays(ctx)
	if err != nil {
		return err
	}
	for _, r := range relays {
		if r.Interface == iface {
			if err := c.RemoveDHCPRelay(ctx, r.ID); err != nil {
				return fmt.Errorf("removing DHCP relay %s from %s: %w", r.Name, iface, err)
			}
		}
	}
	return nil
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

// ─── iSCSI Operations (via ROSE /disk) ──────────────────────────────────────
//
// RouterOS ROSE storage uses /disk to manage iSCSI exports. A file-backed disk
// is created with type=file, then iscsi-export=yes enables the iSCSI target.
// RouterOS auto-generates the IQN based on the disk slot name.

// FileDisk represents a RouterOS file-backed disk entry.
type FileDisk struct {
	ID             string `json:".id"`
	Slot           string `json:"slot"`
	Type           string `json:"type"`
	FilePath       string `json:"file-path"`
	FileSize       string `json:"file-size,omitempty"`
	Filesystem     string `json:"fs,omitempty"`
	MountPoint     string `json:"mount-point,omitempty"`
	Mounted        string `json:"mounted,omitempty"`
	Size           string `json:"size,omitempty"`
	Free           string `json:"free,omitempty"`
	ISCSIExport    string `json:"iscsi-export"`
	ISCSIServerIQN string `json:"iscsi-server-iqn,omitempty"`
}

// CreateISCSITarget creates a file-backed disk and enables iSCSI export.
// Returns the .id of the created disk.
func (c *Client) CreateISCSITarget(ctx context.Context, name, filePath string) (string, error) {
	// RouterOS stores file-path with leading /
	rosPath := "/" + strings.TrimPrefix(filePath, "/")

	// Step 1: Create file-backed disk
	var result map[string]interface{}
	err := c.restPOST(ctx, "/disk/add", map[string]string{
		"type":      "file",
		"file-path": rosPath,
	}, &result)
	if err != nil {
		return "", fmt.Errorf("creating file disk for %s: %w", rosPath, err)
	}

	// Find the disk by file-path to get its .id
	disks, err := c.listFileDisks(ctx)
	if err != nil {
		return "", fmt.Errorf("listing disks after create: %w", err)
	}
	var diskID string
	for _, d := range disks {
		// Normalize both paths for comparison (strip leading /)
		if strings.TrimPrefix(d.FilePath, "/") == strings.TrimPrefix(rosPath, "/") {
			diskID = d.ID
			break
		}
	}
	if diskID == "" {
		return "", fmt.Errorf("created file disk for %s but could not find it", rosPath)
	}

	// Step 2: Enable iSCSI export
	err = c.restPOST(ctx, "/disk/set", map[string]string{
		".id":          diskID,
		"iscsi-export": "yes",
	}, nil)
	if err != nil {
		// Clean up the disk we just created
		_ = c.restPOST(ctx, "/disk/remove", map[string]string{".id": diskID}, nil)
		return "", fmt.Errorf("enabling iSCSI export: %w", err)
	}

	return diskID, nil
}

// RemoveISCSITarget disables iSCSI export and removes the file-backed disk.
func (c *Client) RemoveISCSITarget(ctx context.Context, id string) error {
	// Disable export first
	_ = c.restPOST(ctx, "/disk/set", map[string]string{
		".id":          id,
		"iscsi-export": "no",
	}, nil)
	return c.restPOST(ctx, "/disk/remove", map[string]string{".id": id}, nil)
}

// GetISCSIDisk returns the file disk details including auto-generated IQN.
func (c *Client) GetISCSIDisk(ctx context.Context, id string) (*FileDisk, error) {
	disks, err := c.listFileDisks(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range disks {
		if d.ID == id {
			return &d, nil
		}
	}
	return nil, fmt.Errorf("disk %s not found", id)
}

// FindFileDiskByPath finds a file-backed disk by its file path.
// Returns nil if no matching disk is found.
func (c *Client) FindFileDiskByPath(ctx context.Context, filePath string) (*FileDisk, error) {
	disks, err := c.listFileDisks(ctx)
	if err != nil {
		return nil, err
	}
	normalized := strings.TrimPrefix(filePath, "/")
	for _, d := range disks {
		if strings.TrimPrefix(d.FilePath, "/") == normalized {
			return &d, nil
		}
	}
	return nil, nil
}

// SetISCSIExport enables or disables iSCSI export on a disk.
func (c *Client) SetISCSIExport(ctx context.Context, id string, enabled bool) error {
	val := "no"
	if enabled {
		val = "yes"
	}
	return c.restPOST(ctx, "/disk/set", map[string]string{
		".id":          id,
		"iscsi-export": val,
	}, nil)
}

// CreateFileDisk creates a file-backed disk entry (without iSCSI export).
// Returns the .id of the created disk.
func (c *Client) CreateFileDisk(ctx context.Context, filePath string) (string, error) {
	rosPath := "/" + strings.TrimPrefix(filePath, "/")

	err := c.restPOST(ctx, "/disk/add", map[string]string{
		"type":      "file",
		"file-path": rosPath,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("creating file disk for %s: %w", rosPath, err)
	}

	// Find the disk by file-path to get its .id
	disks, err := c.listFileDisks(ctx)
	if err != nil {
		return "", fmt.Errorf("listing disks after create: %w", err)
	}
	for _, d := range disks {
		if strings.TrimPrefix(d.FilePath, "/") == strings.TrimPrefix(rosPath, "/") {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("created file disk for %s but could not find it", rosPath)
}

// RemoveFileDisk removes a file-backed disk entry.
func (c *Client) RemoveFileDisk(ctx context.Context, id string) error {
	return c.restPOST(ctx, "/disk/remove", map[string]string{".id": id}, nil)
}

// listFileDisks returns all file-type disks.
func (c *Client) listFileDisks(ctx context.Context) ([]FileDisk, error) {
	var allDisks []FileDisk
	err := c.restGET(ctx, "/disk", &allDisks)
	if err != nil {
		return nil, err
	}
	var fileDisks []FileDisk
	for _, d := range allDisks {
		if d.Type == "file" {
			fileDisks = append(fileDisks, d)
		}
	}
	return fileDisks, nil
}

// FileDiskIndex is a pre-fetched index of file-backed disks keyed by file path.
// Used for batch PVC capacity/usage enrichment without repeated REST calls.
type FileDiskIndex struct {
	byPath map[string]*FileDisk
	byID   map[string]*FileDisk
}

// FetchFileDiskIndex fetches /disk once and builds a reusable index of file-type disks.
func (c *Client) FetchFileDiskIndex(ctx context.Context) (*FileDiskIndex, error) {
	disks, err := c.listFileDisks(ctx)
	if err != nil {
		return nil, err
	}
	idx := &FileDiskIndex{
		byPath: make(map[string]*FileDisk, len(disks)),
		byID:   make(map[string]*FileDisk, len(disks)),
	}
	for i := range disks {
		d := &disks[i]
		path := strings.TrimPrefix(d.FilePath, "/")
		idx.byPath[path] = d
		idx.byID[d.ID] = d
	}
	return idx, nil
}

// ByPath looks up a file disk by its file path (without leading /).
func (idx *FileDiskIndex) ByPath(path string) *FileDisk {
	return idx.byPath[strings.TrimPrefix(path, "/")]
}

// ByID looks up a file disk by its RouterOS .id.
func (idx *FileDiskIndex) ByID(id string) *FileDisk {
	return idx.byID[id]
}

// ─── Physical Disk Discovery ────────────────────────────────────────────────

// PhysicalDisk represents a hardware or RAID disk as returned by /disk.
type PhysicalDisk struct {
	ID              string `json:".id"`
	Slot            string `json:"slot"`
	Type            string `json:"type"`               // hardware, raid, file
	Model           string `json:"model,omitempty"`
	Size            string `json:"size,omitempty"`      // e.g. "953870516224"
	Free            string `json:"free,omitempty"`      // e.g. "915953082368"
	Filesystem      string `json:"fs,omitempty"`        // e.g. "ext4"
	MountPoint      string `json:"mount-point,omitempty"`
	RaidType        string `json:"raid-type,omitempty"` // e.g. "raid-0", "raid-1"
	RaidDeviceCount string `json:"raid-device-count,omitempty"`
	Interface       string `json:"interface,omitempty"` // e.g. "NVMe", "SATA"
	State           string `json:"state,omitempty"`
	DiskReads       string `json:"disk-reads,omitempty"`
	DiskWrites      string `json:"disk-writes,omitempty"`
	SmartStatus     string `json:"smart-rollup,omitempty"`
}

// ListPhysicalDisks returns hardware and RAID disks that have a filesystem
// and mount-point (i.e. are usable for storage). File-type virtual disks
// are excluded.
func (c *Client) ListPhysicalDisks(ctx context.Context) ([]PhysicalDisk, error) {
	var allDisks []PhysicalDisk
	if err := c.restGET(ctx, "/disk", &allDisks); err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}
	var result []PhysicalDisk
	for _, d := range allDisks {
		if d.Type != "hardware" && d.Type != "raid" {
			continue
		}
		if d.MountPoint == "" || d.Filesystem == "" {
			continue
		}
		result = append(result, d)
	}
	return result, nil
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
