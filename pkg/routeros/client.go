package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	rosapi "github.com/go-routeros/routeros/v3"
	"github.com/glennswest/mkube/pkg/config"
)


// Client wraps the RouterOS native API (TCP port 8728) for container and
// infrastructure management. A single persistent TCP connection is maintained
// with automatic reconnect on failure. The native API uses tag-based
// multiplexing for concurrent requests over one connection, which gives
// proper session semantics: one TCP connection = one session, cleaned up
// on TCP close.
//
// HTTP is retained solely for UploadFile (the native API has no file
// transfer facility).
type Client struct {
	cfg config.RouterOSConfig

	// Native RouterOS API connection (port 8728).
	conn   *rosapi.Client
	connMu sync.Mutex

	// exec runs a command on the RouterOS device. Production uses nativeRun
	// (with automatic reconnect). Tests override this for mocking.
	exec func(ctx context.Context, words ...string) (*rosapi.Reply, error)

	// httpClient is retained only for UploadFile (native API has no file transfer).
	httpClient *http.Client

	// containerCache: short-lived cache for ListContainers to avoid
	// repeated /container/print calls during the reconcile loop.
	containerCacheMu sync.Mutex
	containerCache   []Container
	containerCacheAt time.Time
}

// Container represents a RouterOS container as returned by /container/print.
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
	Envlist     string `json:"envlist,omitempty"`
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
	Envlist     string `json:"envlist,omitempty"`
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

// NewClient creates a new RouterOS API client. The native API connection
// (port 8728) is established lazily on first command, allowing the client
// to be created even if RouterOS is temporarily unreachable.
// File uploads use HTTP via RESTURL.
func NewClient(cfg config.RouterOSConfig) (*Client, error) {
	c := &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: cfg.InsecureVerify,
				},
			},
			Timeout: 30 * time.Second,
		},
	}
	c.exec = c.nativeRun // wire up the production executor
	// Connection is established lazily on first command via nativeRun.
	return c, nil
}

// NewClientForTest creates a Client with a custom command executor for testing.
// It bypasses the native API connection entirely. External packages that wrap
// routeros.Client (e.g. network/driver) use this to inject mock responses.
func NewClientForTest(cfg config.RouterOSConfig, execFn func(ctx context.Context, words ...string) (*rosapi.Reply, error)) *Client {
	return &Client{
		cfg:  cfg,
		exec: execFn,
		httpClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: cfg.InsecureVerify,
				},
			},
			Timeout: 30 * time.Second,
		},
	}
}

// dialNative establishes a connection to the RouterOS native API.
// Authentication (login) is handled internally by the library.
func (c *Client) dialNative() error {
	addr := c.cfg.Address
	if addr == "" {
		return fmt.Errorf("RouterOS address not configured")
	}

	var conn *rosapi.Client
	var err error
	if c.cfg.UseTLS {
		tlsCfg := &tls.Config{InsecureSkipVerify: c.cfg.InsecureVerify}
		conn, err = rosapi.DialTLSTimeout(addr, c.cfg.User, c.cfg.Password, tlsCfg, 10*time.Second)
	} else {
		conn, err = rosapi.DialTimeout(addr, c.cfg.User, c.cfg.Password, 10*time.Second)
	}
	if err != nil {
		return err
	}

	// Enable async mode for concurrent command multiplexing via .tag
	conn.Async()
	c.conn = conn
	return nil
}

// Close releases resources held by the client.
func (c *Client) Close() error {
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()
	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
	return nil
}

// nativeRun executes a command on the native RouterOS API with automatic
// reconnect on connection failure. Device errors (RouterOS !trap) are
// returned without retry since they indicate a command-level problem,
// not a connection problem.
func (c *Client) nativeRun(ctx context.Context, words ...string) (*rosapi.Reply, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c.connMu.Lock()
		if c.conn == nil {
			if err := c.dialNative(); err != nil {
				c.connMu.Unlock()
				if attempt == 0 {
					time.Sleep(time.Duration(1<<attempt) * time.Second) // 1s backoff
					continue
				}
				return nil, fmt.Errorf("reconnect failed: %w", err)
			}
		}
		conn := c.conn
		c.connMu.Unlock()

		reply, err := conn.RunContext(ctx, words...)
		if err == nil {
			return reply, nil
		}

		// Device errors (RouterOS !trap) are command-level failures — don't retry
		var devErr *rosapi.DeviceError
		if errors.As(err, &devErr) {
			return nil, err
		}

		// Connection-level error — close and retry
		c.connMu.Lock()
		if c.conn == conn { // another goroutine hasn't already reconnected
			c.conn.Close()
			c.conn = nil
		}
		c.connMu.Unlock()
	}
	return nil, fmt.Errorf("RouterOS API unreachable after reconnect")
}

// ─── Container Operations ───────────────────────────────────────────────────

// containerCacheTTL controls how long ListContainers results are cached.
const containerCacheTTL = 5 * time.Second

// ListContainers returns all containers on the RouterOS device.
// Results are cached for containerCacheTTL to reduce API calls.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	c.containerCacheMu.Lock()
	if c.containerCache != nil && time.Since(c.containerCacheAt) < containerCacheTTL {
		cached := make([]Container, len(c.containerCache))
		copy(cached, c.containerCache)
		c.containerCacheMu.Unlock()
		return cached, nil
	}
	c.containerCacheMu.Unlock()

	var containers []Container
	err := c.restGET(ctx, "/container", &containers)
	if err != nil {
		return nil, err
	}

	c.containerCacheMu.Lock()
	c.containerCache = make([]Container, len(containers))
	copy(c.containerCache, containers)
	c.containerCacheAt = time.Now()
	c.containerCacheMu.Unlock()

	return containers, nil
}

// InvalidateContainerCache forces the next ListContainers call to fetch fresh data.
// Call this after creating, deleting, starting, or stopping a container.
func (c *Client) InvalidateContainerCache() {
	c.containerCacheMu.Lock()
	c.containerCache = nil
	c.containerCacheMu.Unlock()
}

// GetContainer returns a single container by name.
// Uses the cached container list to avoid a separate API call.
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
// Calls /container/add directly via the native API. Async tag-multiplexing
// keeps the connection responsive to other commands while RouterOS extracts
// the tarball in the background, so no script wrapper or polling loop is
// needed. Boolean fields (logging, start-on-boot) are passed as the CLI
// "yes"/"no" form that RouterOS accepts on both transports.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	defer c.InvalidateContainerCache()

	body := map[string]string{
		"name":      spec.Name,
		"interface": spec.Interface,
		"root-dir":  spec.RootDir,
	}
	if spec.File != "" {
		body["file"] = spec.File
	}
	if spec.RemoteImage != "" {
		body["remote-image"] = spec.RemoteImage
	}
	if spec.MountLists != "" {
		body["mountlists"] = spec.MountLists
	}
	if spec.Cmd != "" {
		body["cmd"] = spec.Cmd
	}
	if spec.Entrypoint != "" {
		body["entrypoint"] = spec.Entrypoint
	}
	if spec.WorkDir != "" {
		body["workdir"] = spec.WorkDir
	}
	if spec.Hostname != "" {
		body["hostname"] = spec.Hostname
	}
	if spec.DNS != "" {
		body["dns"] = spec.DNS
	}
	if spec.User != "" {
		body["user"] = spec.User
	}
	if spec.Envlist != "" {
		body["envlist"] = spec.Envlist
	}
	if spec.Logging != "" {
		body["logging"] = rosBool(spec.Logging)
	}
	if spec.StartOnBoot != "" {
		body["start-on-boot"] = rosBool(spec.StartOnBoot)
	}

	return c.restPOST(ctx, "/container/add", body, nil)
}

// rosBool converts REST/JSON boolean strings (true/false) to the RouterOS
// "yes"/"no" form accepted by both REST and native API. Passes through
// already-correct values.
func rosBool(v string) string {
	switch v {
	case "true":
		return "yes"
	case "false":
		return "no"
	default:
		return v
	}
}

// RemoveContainer removes a container by its RouterOS .id.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	defer c.InvalidateContainerCache()
	return c.restPOST(ctx, "/container/remove", map[string]string{".id": id}, nil)
}

// StartContainer starts a container by its RouterOS .id.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	defer c.InvalidateContainerCache()
	return c.restPOST(ctx, "/container/start", map[string]string{".id": id}, nil)
}

// StopContainer stops a container by its RouterOS .id.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	defer c.InvalidateContainerCache()
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

// ListMountsByList returns only mount entries matching the given list name.
// Uses RouterOS query filtering to avoid fetching all mounts.
func (c *Client) ListMountsByList(ctx context.Context, listName string) ([]MountEntry, error) {
	var mounts []MountEntry
	err := c.restGET(ctx, "/container/mounts?list="+listName, &mounts)
	return mounts, err
}

// RemoveMountsByList removes all mount entries with the given list name.
func (c *Client) RemoveMountsByList(ctx context.Context, listName string) error {
	mounts, err := c.ListMountsByList(ctx, listName)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if err := c.restPOST(ctx, "/container/mounts/remove", map[string]string{".id": m.ID}, nil); err != nil {
			return fmt.Errorf("removing mount %s: %w", m.ID, err)
		}
	}
	return nil
}

// DesiredMount describes a mount that should exist.
type DesiredMount struct {
	Src   string // host path
	Dst   string // container path
	IsPVC bool   // true = persistent volume claim, never auto-delete
}

// ReconcileMounts ensures the mount list for a container matches the desired
// state. Existing mounts that match are preserved. Missing mounts are added.
// Extra mounts are removed ONLY if they are NOT PVC-backed (any mount whose
// src contains "/pvc/" or "/volumes/pvc/" is treated as PVC-backed and
// preserved even if not in the desired list, to prevent data loss).
func (c *Client) ReconcileMounts(ctx context.Context, listName string, desired []DesiredMount) error {
	mounts, err := c.ListMountsByList(ctx, listName)
	if err != nil {
		return err
	}

	// Index existing mounts for this list by "src→dst"
	type existingMount struct {
		entry MountEntry
		seen  bool // true if matched to a desired mount
	}
	existing := make(map[string]*existingMount)
	for _, m := range mounts {
		key := m.Src + "→" + m.Dst
		existing[key] = &existingMount{entry: m}
	}

	// Add missing mounts
	for _, d := range desired {
		key := d.Src + "→" + d.Dst
		if em, ok := existing[key]; ok {
			em.seen = true // already exists, keep it
			continue
		}
		// Create the missing mount
		if err := c.CreateMount(ctx, listName, d.Src, d.Dst); err != nil {
			return fmt.Errorf("creating mount %s→%s: %w", d.Src, d.Dst, err)
		}
	}

	// Remove extra mounts (not in desired list) — but NEVER remove PVC mounts
	for _, em := range existing {
		if em.seen {
			continue // matched a desired mount, keep it
		}
		// Safety: never remove mounts that look like PVC volumes
		src := em.entry.Src
		if strings.Contains(src, "/pvc/") || strings.Contains(src, "/volumes/pvc/") {
			continue // PVC mount — preserve unconditionally
		}
		if err := c.restPOST(ctx, "/container/mounts/remove", map[string]string{".id": em.entry.ID}, nil); err != nil {
			return fmt.Errorf("removing stale mount %s: %w", em.entry.ID, err)
		}
	}

	return nil
}

// ─── Environment Variable Operations ────────────────────────────────────────

// EnvEntry represents a RouterOS container environment variable.
type EnvEntry struct {
	ID    string `json:".id"`
	List  string `json:"list"`  // env list name
	Name  string `json:"name"`  // env var key
	Value string `json:"value"` // env var value
}

// ListEnvs returns all container environment variable entries.
func (c *Client) ListEnvs(ctx context.Context) ([]EnvEntry, error) {
	var envs []EnvEntry
	err := c.restGET(ctx, "/container/envs", &envs)
	return envs, err
}

// CreateEnv creates a container environment variable entry.
func (c *Client) CreateEnv(ctx context.Context, listName, key, value string) error {
	return c.restPOST(ctx, "/container/envs/add", map[string]string{
		"list":  listName,
		"name":  key,
		"value": value,
	}, nil)
}

// RemoveEnvsByList removes all env entries with the given list name.
func (c *Client) RemoveEnvsByList(ctx context.Context, listName string) error {
	envs, err := c.ListEnvs(ctx)
	if err != nil {
		return err
	}
	for _, e := range envs {
		if e.List == listName {
			if err := c.restPOST(ctx, "/container/envs/remove", map[string]string{".id": e.ID}, nil); err != nil {
				return fmt.Errorf("removing env %s: %w", e.ID, err)
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
// If on a different bridge, removes and re-adds. If the existing port is
// dynamic (auto-created by RouterOS when a container starts without a static
// port), it cannot be removed directly — the veth is deleted and recreated
// to clear the dynamic assignment.
func (c *Client) AddBridgePort(ctx context.Context, bridge, iface string) error {
	// Use filtered query instead of listing all ports (can be 8000+).
	var ports []BridgePort
	if err := c.restGET(ctx, "/interface/bridge/port?interface="+iface, &ports); err != nil {
		return fmt.Errorf("querying bridge port for %q: %w", iface, err)
	}
	for _, p := range ports {
		if p.Bridge == bridge {
			return nil // already on correct bridge
		}
		// on wrong bridge — try to remove the static port
		if err := c.restPOST(ctx, "/interface/bridge/port/remove", map[string]string{".id": p.ID}, nil); err != nil {
			if strings.Contains(err.Error(), "dynamic port") {
				// Dynamic ports can't be removed/changed. Delete the veth
				// and recreate it so the caller can add a clean static port.
				if recreateErr := c.recreateVethForBridge(ctx, iface); recreateErr != nil {
					return fmt.Errorf("clearing dynamic bridge port for %q: %w", iface, recreateErr)
				}
				break // veth recreated — fall through to add static port
			}
			return fmt.Errorf("removing %q from bridge %q: %w", iface, p.Bridge, err)
		}
		break
	}
	return c.restPOST(ctx, "/interface/bridge/port/add", map[string]string{
		"bridge":    bridge,
		"interface": iface,
	}, nil)
}

// recreateVethForBridge deletes a veth and recreates it with the same config,
// clearing any dynamic bridge port assignment that RouterOS auto-created.
func (c *Client) recreateVethForBridge(ctx context.Context, name string) error {
	// Capture the veth's current config before deleting it.
	veths, err := c.ListVeths(ctx)
	if err != nil {
		return fmt.Errorf("listing veths: %w", err)
	}
	var address, gateway, id string
	for _, v := range veths {
		if v.Name == name {
			address = v.Address
			gateway = v.Gateway
			id = v.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("veth %q not found", name)
	}

	// Remove any ghost containers still referencing this veth.
	c.removeContainersUsingVeth(ctx, name)

	// Delete the veth — this also removes the dynamic bridge port.
	if err := c.restPOST(ctx, "/interface/veth/remove", map[string]string{".id": id}, nil); err != nil {
		return fmt.Errorf("removing veth %q: %w", name, err)
	}

	// Recreate with identical config.
	if err := c.restPOST(ctx, "/interface/veth/add", map[string]string{
		"name":    name,
		"address": address,
		"gateway": gateway,
	}, nil); err != nil {
		return fmt.Errorf("recreating veth %q: %w", name, err)
	}
	return nil
}

// removeContainersUsingVeth stops and removes any RouterOS containers that
// reference the named veth interface. This clears stale "in use by container"
// references that prevent veth deletion.
func (c *Client) removeContainersUsingVeth(ctx context.Context, vethName string) {
	var containers []Container
	if err := c.restGET(ctx, "/container", &containers); err != nil {
		return
	}
	for _, ct := range containers {
		if ct.Interface == vethName {
			_ = c.restPOST(ctx, "/container/stop", map[string]string{".id": ct.ID}, nil)
			time.Sleep(2 * time.Second) // allow stop to complete
			_ = c.restPOST(ctx, "/container/remove", map[string]string{".id": ct.ID}, nil)
		}
	}
	c.InvalidateContainerCache()
}

// ListVeths returns all veth interfaces.
func (c *Client) ListVeths(ctx context.Context) ([]NetworkInterface, error) {
	var veths []NetworkInterface
	err := c.restGET(ctx, "/interface/veth", &veths)
	return veths, err
}

// ─── File Operations ────────────────────────────────────────────────────────

// UploadFile uploads a tarball or file to the RouterOS filesystem.
// This is the one operation that still uses HTTP — the native API has
// no file transfer facility.
func (c *Client) UploadFile(ctx context.Context, remotePath string, data io.Reader) error {
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
	err := c.restGET(ctx, "/file?name="+path, &files)
	return files, err
}

// RemoveFile deletes a file from the RouterOS filesystem.
// Looks up the file by name to get its .id, then removes by .id.
func (c *Client) RemoveFile(ctx context.Context, path string) error {
	files, err := c.ListFiles(ctx, path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil // already gone
	}
	id, _ := files[0][".id"].(string)
	if id == "" {
		return fmt.Errorf("file %q found but missing .id", path)
	}
	return c.restPOST(ctx, "/file/remove", map[string]string{".id": id}, nil)
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
// Use it to compute directory disk usage without repeated API calls.
type FileUsageIndex struct {
	files []fileUsageEntry
}

type fileUsageEntry struct {
	name string
	size int64
}

// FetchFileUsageIndex fetches /file once and builds a reusable index.
func (c *Client) FetchFileUsageIndex(ctx context.Context) (*FileUsageIndex, error) {
	var allFiles []map[string]interface{}
	if err := c.restGET(ctx, "/file", &allFiles); err != nil {
		return nil, fmt.Errorf("fetching file index: %w", err)
	}
	idx := &FileUsageIndex{files: make([]fileUsageEntry, 0, len(allFiles))}
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
		idx.files = append(idx.files, fileUsageEntry{name: name, size: sz})
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

// ─── Firewall NAT Operations ─────────────────────────────────────────────────

// NatRule represents a RouterOS firewall NAT rule.
type NatRule struct {
	ID           string `json:".id"`
	Chain        string `json:"chain"`
	Action       string `json:"action"`
	Protocol     string `json:"protocol,omitempty"`
	SrcPort      string `json:"src-port,omitempty"`
	DstPort      string `json:"dst-port,omitempty"`
	OutInterface string `json:"out-interface,omitempty"`
	Comment      string `json:"comment,omitempty"`
}

// ListNatRules returns all firewall NAT rules.
func (c *Client) ListNatRules(ctx context.Context) ([]NatRule, error) {
	var rules []NatRule
	if err := c.restGET(ctx, "/ip/firewall/nat", &rules); err != nil {
		return nil, fmt.Errorf("listing NAT rules: %w", err)
	}
	return rules, nil
}

// AddNatRule creates a firewall NAT rule. The rule map supports all RouterOS
// NAT fields including "place-before" to position relative to another rule.
func (c *Client) AddNatRule(ctx context.Context, rule map[string]string) error {
	return c.restPOST(ctx, "/ip/firewall/nat/add", rule, nil)
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
// Looks up the tunnel's .id first, since the native API requires .id for removal.
func (c *Client) DeleteEoIPTunnel(ctx context.Context, name string) error {
	var tunnels []struct {
		ID   string `json:".id"`
		Name string `json:"name"`
	}
	if err := c.restGET(ctx, "/interface/eoip", &tunnels); err != nil {
		return fmt.Errorf("listing EoIP tunnels: %w", err)
	}
	for _, t := range tunnels {
		if t.Name == name {
			return c.restPOST(ctx, "/interface/eoip/remove", map[string]string{".id": t.ID}, nil)
		}
	}
	return nil // already gone
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
// If sizeBytes > 0, the file is created with that exact size; otherwise RouterOS uses its default.
// Returns the .id of the created disk.
func (c *Client) CreateFileDisk(ctx context.Context, filePath string, sizeBytes int64) (string, error) {
	rosPath := "/" + strings.TrimPrefix(filePath, "/")

	params := map[string]string{
		"type":      "file",
		"file-path": rosPath,
	}
	if sizeBytes > 0 {
		params["file-size"] = fmt.Sprintf("%d", sizeBytes)
	}

	err := c.restPOST(ctx, "/disk/add", params, nil)
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
// Used for batch PVC capacity/usage enrichment without repeated API calls.
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

// ─── Native API Transport ───────────────────────────────────────────────────
//
// restGET and restPOST implement the internal transport layer using the
// RouterOS native API (port 8728). Method names are retained from the
// original REST implementation for minimal diff in method bodies.
// GET operations translate to /print commands; POST operations are direct
// command invocations (/add, /remove, /set, /start, /stop, /run).

// restGET runs a /print query on the native API.
// path may include query parameters: "/container/mounts?list=X" becomes
// "/container/mounts/print ?list=X" in native API words.
func (c *Client) restGET(ctx context.Context, path string, result interface{}) error {
	basePath, queryWords := parseRESTPath(path)
	words := append([]string{basePath + "/print"}, queryWords...)

	reply, err := c.exec(ctx, words...)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}

	return decodeReply(reply, result)
}

// restPOST runs an action command on the native API.
// path is the command path (e.g. "/container/add", "/container/remove").
// body is converted to =key=value attribute words.
func (c *Client) restPOST(ctx context.Context, path string, body interface{}, result interface{}) error {
	words := []string{path}
	if body != nil {
		words = append(words, bodyToWords(body)...)
	}

	reply, err := c.exec(ctx, words...)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}

	if result != nil {
		// For /add commands, RouterOS returns the new item ID in !done
		if reply.Done != nil && len(reply.Done.Map) > 0 {
			data, err := json.Marshal(reply.Done.Map)
			if err != nil {
				return err
			}
			return json.Unmarshal(data, result)
		}
		return decodeReply(reply, result)
	}
	return nil
}

// parseRESTPath splits a path with optional query params into a base path
// and native API query words.
// "/container/mounts?list=X" → ("/container/mounts", ["?list=X"])
// "/container" → ("/container", [])
func parseRESTPath(path string) (string, []string) {
	idx := strings.IndexByte(path, '?')
	if idx < 0 {
		return path, nil
	}
	basePath := path[:idx]
	qStr := path[idx+1:]
	var queryWords []string
	for _, kv := range strings.Split(qStr, "&") {
		if kv != "" {
			queryWords = append(queryWords, "?"+kv)
		}
	}
	return basePath, queryWords
}

// bodyToWords converts a request body (map[string]string or map[string]interface{})
// to RouterOS native API attribute words (=key=value).
func bodyToWords(body interface{}) []string {
	var words []string
	switch b := body.(type) {
	case map[string]string:
		for k, v := range b {
			words = append(words, "="+k+"="+v)
		}
	case map[string]interface{}:
		for k, v := range b {
			words = append(words, "="+k+"="+fmt.Sprint(v))
		}
	default:
		// Fallback: JSON round-trip for struct types
		data, err := json.Marshal(body)
		if err != nil {
			return nil
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil
		}
		for k, v := range m {
			words = append(words, "="+k+"="+fmt.Sprint(v))
		}
	}
	sort.Strings(words)
	return words
}

// decodeReply converts native API reply sentences into Go values.
// For slice targets, all !re sentences are decoded.
// For single-value targets (struct, map), only the first sentence is used.
func decodeReply(reply *rosapi.Reply, result interface{}) error {
	rv := reflect.ValueOf(result)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("result must be a non-nil pointer")
	}

	switch rv.Elem().Kind() {
	case reflect.Slice:
		// Slice target — decode all sentences
		items := make([]map[string]string, len(reply.Re))
		for i, re := range reply.Re {
			items[i] = re.Map
		}
		data, err := json.Marshal(items)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	default:
		// Single item — use first sentence
		if len(reply.Re) == 0 {
			return fmt.Errorf("no data returned")
		}
		data, err := json.Marshal(reply.Re[0].Map)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	}
}
