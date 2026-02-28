package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/glennswest/mkube/pkg/proxmox"
)

// ProxmoxRuntime adapts proxmox.Client to the ContainerRuntime interface.
// Proxmox requires mounts to be specified at container creation time, so
// CreateMount() stages mounts in a per-container accumulator that
// CreateContainer() drains.
type ProxmoxRuntime struct {
	client  *proxmox.Client
	storage string // template storage, e.g. "local"
	rootFS  string // rootfs storage spec, e.g. "local-lvm:8"
	unpriv  bool   // unprivileged containers
	features string // e.g. "nesting=1"

	mu     sync.Mutex
	mounts map[string][]mountEntry // container name → pending mounts
}

type mountEntry struct {
	src string
	dst string
}

// NewProxmoxRuntime wraps a proxmox.Client as a ContainerRuntime.
func NewProxmoxRuntime(client *proxmox.Client, storage, rootFSStorage, rootFSSize string, unprivileged bool, features string) *ProxmoxRuntime {
	rootFS := fmt.Sprintf("%s:%s", rootFSStorage, rootFSSize)
	return &ProxmoxRuntime{
		client:   client,
		storage:  storage,
		rootFS:   rootFS,
		unpriv:   unprivileged,
		features: features,
		mounts:   make(map[string][]mountEntry),
	}
}

func (p *ProxmoxRuntime) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	// Allocate a VMID for this container
	vmid, err := p.client.VMIDs().Allocate(spec.Name)
	if err != nil {
		return fmt.Errorf("allocating VMID for %s: %w", spec.Name, err)
	}

	// Build net0 string from spec
	net0 := spec.Interface // pre-built net0 string from network manager
	if net0 == "" {
		net0 = "name=eth0,bridge=vmbr0"
	}

	// Determine memory (default 512 MB)
	memory := 512
	if spec.MemoryLimit != "" {
		if mb, err := parseMemoryMB(spec.MemoryLimit); err == nil {
			memory = mb
		}
	}

	// Determine cores (default 1)
	cores := 1
	if spec.CPULimit != "" {
		if c, err := strconv.Atoi(spec.CPULimit); err == nil && c > 0 {
			cores = c
		}
	}

	// Build template reference
	ostemplate := spec.Image // expects "local:vztmpl/filename.tar.gz"

	// Determine hostname
	hostname := spec.Hostname
	if hostname == "" {
		hostname = spec.Name
	}

	createSpec := proxmox.LXCCreateSpec{
		VMID:         vmid,
		Hostname:     hostname,
		OSTemplate:   ostemplate,
		Net0:         net0,
		Memory:       memory,
		Swap:         0,
		Cores:        cores,
		RootFS:       p.rootFS,
		Unprivileged: p.unpriv,
		Features:     p.features,
		Nameserver:   spec.DNS,
		OnBoot:       spec.StartOnBoot == "true",
		Start:        false,
		MountPoints:  make(map[int]string),
	}

	// Drain accumulated mounts
	p.mu.Lock()
	if mounts, ok := p.mounts[spec.Name]; ok {
		for i, m := range mounts {
			createSpec.MountPoints[i] = fmt.Sprintf("%s,mp=%s", m.src, m.dst)
		}
		delete(p.mounts, spec.Name)
	}
	p.mu.Unlock()

	// Also add inline volumes from spec
	for i, vol := range spec.Volumes {
		idx := len(createSpec.MountPoints) + i
		mp := fmt.Sprintf("%s,mp=%s", vol.Source, vol.Target)
		if vol.ReadOnly {
			mp += ",ro=1"
		}
		createSpec.MountPoints[idx] = mp
	}

	return p.client.CreateContainer(ctx, createSpec)
}

func (p *ProxmoxRuntime) StartContainer(ctx context.Context, id string) error {
	vmid, err := p.resolveVMID(id)
	if err != nil {
		return err
	}
	return p.client.StartContainer(ctx, vmid)
}

func (p *ProxmoxRuntime) StopContainer(ctx context.Context, id string) error {
	vmid, err := p.resolveVMID(id)
	if err != nil {
		return err
	}
	return p.client.StopContainer(ctx, vmid)
}

func (p *ProxmoxRuntime) RemoveContainer(ctx context.Context, id string) error {
	vmid, err := p.resolveVMID(id)
	if err != nil {
		return err
	}
	err = p.client.RemoveContainer(ctx, vmid)
	if err != nil {
		return err
	}
	// Release the VMID allocation
	name, _ := p.client.VMIDs().LookupName(vmid)
	if name != "" {
		p.client.VMIDs().Release(name)
	}
	return nil
}

func (p *ProxmoxRuntime) GetContainer(ctx context.Context, name string) (*Container, error) {
	vmid, ok := p.client.VMIDs().Lookup(name)
	if !ok {
		return nil, ErrNotFound
	}

	status, err := p.client.GetContainerStatus(ctx, vmid)
	if err != nil {
		return nil, err
	}

	config, err := p.client.GetContainerConfig(ctx, vmid)
	if err != nil {
		return nil, err
	}

	return pveContainerToRuntime(status, config), nil
}

func (p *ProxmoxRuntime) ListContainers(ctx context.Context) ([]Container, error) {
	containers, err := p.client.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Container, 0, len(containers))
	for _, ct := range containers {
		// Only include containers in our VMID range
		if !p.client.VMIDs().InRange(ct.VMID) {
			continue
		}

		c := Container{
			ID:     strconv.Itoa(ct.VMID),
			Name:   ct.Name,
			Status: ct.Status,
		}
		out = append(out, c)
	}
	return out, nil
}

func (p *ProxmoxRuntime) GetLogs(ctx context.Context, name string) ([]LogEntry, error) {
	vmid, ok := p.client.VMIDs().Lookup(name)
	if !ok {
		return nil, ErrNotFound
	}

	logs, err := p.client.GetContainerLogs(ctx, vmid)
	if err != nil {
		return nil, err
	}

	out := make([]LogEntry, len(logs))
	for i, l := range logs {
		out[i] = LogEntry{
			Message: l.Text,
		}
	}
	return out, nil
}

func (p *ProxmoxRuntime) GetSystemResource(ctx context.Context) (*SystemResource, error) {
	status, err := p.client.GetNodeStatus(ctx)
	if err != nil {
		return nil, err
	}

	containers, err := p.client.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	var running uint32
	for _, ct := range containers {
		if ct.Status == "running" {
			running++
		}
	}

	return &SystemResource{
		Hostname:      "proxmox",
		Uptime:        fmt.Sprintf("%ds", status.Uptime),
		CPUCount:      strconv.Itoa(status.CPUInfo.Cores),
		CPULoad:       fmt.Sprintf("%.0f%%", status.CPU*100),
		FreeMemory:    strconv.FormatInt(status.Memory.Free, 10),
		TotalMemory:   strconv.FormatInt(status.Memory.Total, 10),
		Architecture:  "x86_64",
		BoardName:     status.CPUInfo.Model,
		Version:       status.PVEVersion,
		Platform:      "Proxmox VE",
		DiskTotal:     uint64(status.RootFS.Total),
		DiskAvailable: uint64(status.RootFS.Avail),
		WorkloadCount: uint32(len(containers)),
		RunningCount:  running,
	}, nil
}

func (p *ProxmoxRuntime) UploadFile(ctx context.Context, path string, data io.Reader) error {
	// Upload as a template to Proxmox storage
	filename := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		filename = path[idx+1:]
	}
	return p.client.UploadTemplate(ctx, p.storage, filename, data)
}

func (p *ProxmoxRuntime) RemoveFile(ctx context.Context, path string) error {
	// Remove template from storage
	return p.client.RemoveTemplate(ctx, p.storage, path)
}

// CreateMount stages a mount for the next CreateContainer call.
// Proxmox requires all mounts to be specified at container creation time.
func (p *ProxmoxRuntime) CreateMount(_ context.Context, name, src, dst string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.mounts[name] = append(p.mounts[name], mountEntry{src: src, dst: dst})
	return nil
}

func (p *ProxmoxRuntime) RemoveMountsByList(_ context.Context, _ string) error {
	// Mounts are destroyed with the container on Proxmox
	return nil
}

func (p *ProxmoxRuntime) Backend() string {
	return "proxmox"
}

func (p *ProxmoxRuntime) Close() error {
	return p.client.Close()
}

// Client returns the underlying proxmox.Client for direct access.
func (p *ProxmoxRuntime) Client() *proxmox.Client {
	return p.client
}

// EnsureTemplate converts an OCI image to a Proxmox template and uploads it.
// Returns the template reference (e.g. "local:vztmpl/filename.tar.gz").
func (p *ProxmoxRuntime) EnsureTemplate(ctx context.Context, imageRef string, log interface{ Infow(string, ...interface{}) }) (string, error) {
	converter := proxmox.NewTemplateConverter(nil)
	data, filename, err := converter.ConvertToTemplate(ctx, imageRef)
	if err != nil {
		return "", err
	}

	// Read into buffer for upload
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, data); err != nil {
		return "", fmt.Errorf("buffering template: %w", err)
	}

	if err := p.client.UploadTemplate(ctx, p.storage, filename, &buf); err != nil {
		return "", fmt.Errorf("uploading template %s: %w", filename, err)
	}

	return fmt.Sprintf("%s:vztmpl/%s", p.storage, filename), nil
}

// resolveVMID converts a string ID (VMID or name) to an integer VMID.
func (p *ProxmoxRuntime) resolveVMID(id string) (int, error) {
	// Try as integer first
	if vmid, err := strconv.Atoi(id); err == nil {
		return vmid, nil
	}
	// Try as container name
	if vmid, ok := p.client.VMIDs().Lookup(id); ok {
		return vmid, nil
	}
	return 0, fmt.Errorf("cannot resolve VMID for %q", id)
}

// pveContainerToRuntime converts Proxmox container status + config to runtime.Container.
func pveContainerToRuntime(status *proxmox.LXCContainer, config *proxmox.LXCConfig) *Container {
	ct := &Container{
		ID:       strconv.Itoa(status.VMID),
		Name:     status.Name,
		Status:   status.Status,
		Hostname: config.Hostname,
		DNS:      config.Nameserver,
	}

	// Extract IP from net0 config
	if config.Net0 != "" {
		ct.Interface = config.Net0
		if ip := extractNetIP(config.Net0); ip != "" {
			ct.DNS = ip // DNS field doubles as IP in the runtime model
		}
	}

	if config.OnBoot == 1 {
		ct.StartOnBoot = "true"
	}

	return ct
}

// extractNetIP extracts the IP address from a Proxmox net config string.
// Format: "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1"
func extractNetIP(netConfig string) string {
	for _, part := range strings.Split(netConfig, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && kv[0] == "ip" {
			// Strip CIDR mask
			ip := kv[1]
			if idx := strings.Index(ip, "/"); idx >= 0 {
				ip = ip[:idx]
			}
			return ip
		}
	}
	return ""
}

// parseMemoryMB parses a memory limit string (e.g. "512Mi", "1Gi", "256") to MB.
func parseMemoryMB(s string) (int, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Gi") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, "Gi"), 64)
		if err != nil {
			return 0, err
		}
		return int(val * 1024), nil
	}
	if strings.HasSuffix(s, "Mi") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, "Mi"), 64)
		if err != nil {
			return 0, err
		}
		return int(val), nil
	}
	return strconv.Atoi(s)
}

// Ensure ProxmoxRuntime implements ContainerRuntime at compile time.
var _ ContainerRuntime = (*ProxmoxRuntime)(nil)
