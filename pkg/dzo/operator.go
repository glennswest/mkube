package dzo

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/dns"
	"github.com/glenneth/microkube/pkg/lifecycle"
	"github.com/glenneth/microkube/pkg/network"
	"github.com/glenneth/microkube/pkg/routeros"
)

// Operator manages DNS zones and MicroDNS instances.
type Operator struct {
	cfg        config.DZOConfig
	networks   []config.NetworkDef
	dns        *dns.Client
	ros        *routeros.Client
	netMgr     *network.Manager
	lcMgr      *lifecycle.Manager
	log        *zap.SugaredLogger

	mu    sync.RWMutex
	state *State
}

// NewOperator creates a DZO operator.
func NewOperator(
	cfg config.DZOConfig,
	networks []config.NetworkDef,
	dnsClient *dns.Client,
	rosClient *routeros.Client,
	netMgr *network.Manager,
	lcMgr *lifecycle.Manager,
	log *zap.SugaredLogger,
) *Operator {
	return &Operator{
		cfg:      cfg,
		networks: networks,
		dns:      dnsClient,
		ros:      rosClient,
		netMgr:   netMgr,
		lcMgr:    lcMgr,
		log:      log.Named("dzo"),
		state:    NewState(),
	}
}

// Bootstrap loads persisted state, imports zones from network config,
// and probes MicroDNS endpoints.
func (o *Operator) Bootstrap(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	// 1. Load persisted state
	if err := o.loadState(); err != nil {
		o.log.Warnw("no persisted state, starting fresh", "error", err)
		o.state = NewState()
	}

	// 2. Import zones from network config
	for _, net := range o.networks {
		if net.DNS.Endpoint == "" || net.DNS.Zone == "" {
			continue
		}

		zoneName := net.DNS.Zone

		// Register instance if not known
		instanceName := instanceNameFromEndpoint(net.DNS.Endpoint, net.Name)
		if _, exists := o.state.Instances[instanceName]; !exists {
			o.state.Instances[instanceName] = &MicroDNSInstance{
				Name:     instanceName,
				Network:  net.Name,
				IP:       net.DNS.Server,
				Endpoint: net.DNS.Endpoint,
				Zones:    []string{zoneName},
				Managed:  false,
			}
		}

		// Register zone if not known
		if _, exists := o.state.Zones[zoneName]; !exists {
			o.state.Zones[zoneName] = &Zone{
				Name:     zoneName,
				Endpoint: net.DNS.Endpoint,
				Network:  net.Name,
			}
		}
	}

	// 3. Probe each MicroDNS endpoint and ensure zones exist
	for zoneName, zone := range o.state.Zones {
		zoneID, err := o.dns.EnsureZone(ctx, zone.Endpoint, zoneName)
		if err != nil {
			o.log.Warnw("failed to ensure zone on MicroDNS", "zone", zoneName, "endpoint", zone.Endpoint, "error", err)
			continue
		}
		zone.MicroDNSID = zoneID
		o.log.Infow("zone verified", "zone", zoneName, "id", zoneID, "endpoint", zone.Endpoint)
	}

	// 4. Persist
	if err := o.saveState(); err != nil {
		o.log.Warnw("failed to save state after bootstrap", "error", err)
	}

	o.log.Infow("bootstrap complete",
		"zones", len(o.state.Zones),
		"instances", len(o.state.Instances),
	)
	return nil
}

// ─── Zone CRUD ──────────────────────────────────────────────────────────────

// ListZones returns all zones.
func (o *Operator) ListZones() []*Zone {
	o.mu.RLock()
	defer o.mu.RUnlock()

	zones := make([]*Zone, 0, len(o.state.Zones))
	for _, z := range o.state.Zones {
		zones = append(zones, z)
	}
	return zones
}

// GetZone returns a zone by name.
func (o *Operator) GetZone(name string) (*Zone, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	z, ok := o.state.Zones[name]
	if !ok {
		return nil, fmt.Errorf("zone %q not found", name)
	}
	return z, nil
}

// CreateZoneRequest is the input for creating a zone.
type CreateZoneRequest struct {
	Name      string `json:"name"`      // e.g. "kube.gt.lo"
	Network   string `json:"network"`   // e.g. "gt"
	Dedicated bool   `json:"dedicated"` // true = create new MicroDNS container
}

// CreateZone creates a new zone, optionally with a dedicated MicroDNS container.
func (o *Operator) CreateZone(ctx context.Context, req CreateZoneRequest) (*Zone, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, exists := o.state.Zones[req.Name]; exists {
		return nil, fmt.Errorf("zone %q already exists", req.Name)
	}

	// Find parent zone (the suffix after first dot)
	parent := parentZone(req.Name)

	// Find network config
	netDef := o.findNetwork(req.Network)
	if netDef == nil {
		return nil, fmt.Errorf("network %q not found", req.Network)
	}

	var endpoint string
	var zoneID string

	if req.Dedicated {
		// Create a new MicroDNS container
		inst, err := o.createMicroDNSInstance(ctx, req.Name, req.Network)
		if err != nil {
			return nil, fmt.Errorf("creating dedicated MicroDNS: %w", err)
		}
		endpoint = inst.Endpoint

		// Wait for healthy
		if err := o.waitForMicroDNS(ctx, endpoint, 60*time.Second); err != nil {
			return nil, fmt.Errorf("waiting for MicroDNS at %s: %w", endpoint, err)
		}

		// Create zone on new instance
		id, err := o.dns.EnsureZone(ctx, endpoint, req.Name)
		if err != nil {
			return nil, fmt.Errorf("creating zone on dedicated MicroDNS: %w", err)
		}
		zoneID = id

		// Track zone on instance
		inst.Zones = append(inst.Zones, req.Name)
	} else {
		// Use existing MicroDNS on the network
		endpoint = netDef.DNS.Endpoint
		if endpoint == "" {
			return nil, fmt.Errorf("network %q has no MicroDNS endpoint", req.Network)
		}

		id, err := o.dns.EnsureZone(ctx, endpoint, req.Name)
		if err != nil {
			return nil, fmt.Errorf("creating zone on existing MicroDNS: %w", err)
		}
		zoneID = id

		// Track zone on the existing instance
		for _, inst := range o.state.Instances {
			if inst.Endpoint == endpoint {
				inst.Zones = append(inst.Zones, req.Name)
				break
			}
		}
	}

	zone := &Zone{
		Name:       req.Name,
		Parent:     parent,
		MicroDNSID: zoneID,
		Endpoint:   endpoint,
		Network:    req.Network,
		Dedicated:  req.Dedicated,
	}

	o.state.Zones[req.Name] = zone
	if err := o.saveState(); err != nil {
		o.log.Warnw("failed to save state after zone create", "error", err)
	}

	o.log.Infow("zone created", "zone", req.Name, "dedicated", req.Dedicated, "endpoint", endpoint)
	return zone, nil
}

// DeleteZone removes a zone and its dedicated MicroDNS if applicable.
func (o *Operator) DeleteZone(ctx context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	zone, ok := o.state.Zones[name]
	if !ok {
		return fmt.Errorf("zone %q not found", name)
	}

	// If dedicated, remove the MicroDNS container
	if zone.Dedicated {
		if err := o.removeDedicatedInstance(ctx, zone); err != nil {
			o.log.Warnw("failed to remove dedicated MicroDNS", "zone", name, "error", err)
		}
	}

	// Remove zone from instances
	for _, inst := range o.state.Instances {
		inst.Zones = removeString(inst.Zones, name)
	}

	delete(o.state.Zones, name)
	if err := o.saveState(); err != nil {
		o.log.Warnw("failed to save state after zone delete", "error", err)
	}

	o.log.Infow("zone deleted", "zone", name)
	return nil
}

// ─── ZoneResolver Interface (for namespace.Manager) ─────────────────────────

// GetZoneEndpoint returns the MicroDNS endpoint and zone ID for a given zone name.
// Satisfies namespace.ZoneResolver.
func (o *Operator) GetZoneEndpoint(zoneName string) (endpoint, zoneID string, err error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	zone, ok := o.state.Zones[zoneName]
	if !ok {
		return "", "", fmt.Errorf("zone %q not found", zoneName)
	}
	return zone.Endpoint, zone.MicroDNSID, nil
}

// EnsureZone creates a zone if it doesn't already exist. Idempotent.
// Satisfies namespace.ZoneResolver.
func (o *Operator) EnsureZone(ctx context.Context, zoneName, network string, dedicated bool) error {
	// Check if already exists (read lock)
	o.mu.RLock()
	_, exists := o.state.Zones[zoneName]
	o.mu.RUnlock()

	if exists {
		return nil
	}

	_, err := o.CreateZone(ctx, CreateZoneRequest{
		Name:      zoneName,
		Network:   network,
		Dedicated: dedicated,
	})
	return err
}

// ─── Instance Operations ────────────────────────────────────────────────────

// ListInstances returns all MicroDNS instances.
func (o *Operator) ListInstances() []*MicroDNSInstance {
	o.mu.RLock()
	defer o.mu.RUnlock()

	insts := make([]*MicroDNSInstance, 0, len(o.state.Instances))
	for _, inst := range o.state.Instances {
		insts = append(insts, inst)
	}
	return insts
}

// GetInstance returns a MicroDNS instance by name.
func (o *Operator) GetInstance(name string) (*MicroDNSInstance, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	inst, ok := o.state.Instances[name]
	if !ok {
		return nil, fmt.Errorf("instance %q not found", name)
	}
	return inst, nil
}

// ─── MicroDNS Instance Management ───────────────────────────────────────────

func (o *Operator) createMicroDNSInstance(ctx context.Context, zoneName, networkName string) (*MicroDNSInstance, error) {
	netDef := o.findNetwork(networkName)
	if netDef == nil {
		return nil, fmt.Errorf("network %q not found", networkName)
	}

	// Generate names
	safeName := strings.ReplaceAll(strings.ReplaceAll(zoneName, ".", "-"), "_", "-")
	containerName := fmt.Sprintf("mdns-%s", safeName)
	vethName := fmt.Sprintf("veth-mdns-%s", truncate(safeName, 10))

	// Allocate IP via network manager
	ip, _, _, err := o.netMgr.AllocateInterface(ctx, vethName, containerName, networkName)
	if err != nil {
		return nil, fmt.Errorf("allocating interface: %w", err)
	}

	// Strip CIDR suffix for endpoint
	bareIP := strings.Split(ip, "/")[0]

	image := o.cfg.MicroDNSImage
	if image == "" {
		image = "192.168.200.2:5000/microdns:latest"
	}

	// Create RouterOS container
	spec := routeros.ContainerSpec{
		Name:        containerName,
		Tag:         image,
		Interface:   vethName,
		RootDir:     fmt.Sprintf("/raid1/images/%s", containerName),
		Hostname:    containerName,
		DNS:         netDef.DNS.Server,
		Logging:     "true",
		StartOnBoot: "true",
	}

	if err := o.ros.CreateContainer(ctx, spec); err != nil {
		// Cleanup veth on failure
		_ = o.netMgr.ReleaseInterface(ctx, vethName)
		return nil, fmt.Errorf("creating container: %w", err)
	}

	// Get container ID and start it
	ct, err := o.ros.GetContainer(ctx, containerName)
	if err != nil {
		return nil, fmt.Errorf("getting created container: %w", err)
	}

	if err := o.ros.StartContainer(ctx, ct.ID); err != nil {
		return nil, fmt.Errorf("starting container: %w", err)
	}

	inst := &MicroDNSInstance{
		Name:        containerName,
		Network:     networkName,
		IP:          bareIP,
		Endpoint:    fmt.Sprintf("http://%s:8080", bareIP),
		VethName:    vethName,
		ContainerID: ct.ID,
		Managed:     true,
		Image:       image,
	}

	o.state.Instances[containerName] = inst

	// Register with lifecycle manager
	o.lcMgr.Register(containerName, lifecycle.ContainerUnit{
		Name:          containerName,
		ContainerID:   ct.ID,
		ContainerIP:   bareIP,
		RestartPolicy: "Always",
		StartOnBoot:   true,
		Managed:       true,
		HealthCheck: &lifecycle.HealthCheck{
			Type: "http",
			Path: "/api/v1/zones",
			Port: 8080,
		},
	})

	o.log.Infow("MicroDNS instance created",
		"name", containerName,
		"ip", bareIP,
		"network", networkName,
	)
	return inst, nil
}

func (o *Operator) removeDedicatedInstance(ctx context.Context, zone *Zone) error {
	// Find the instance serving this zone
	var target *MicroDNSInstance
	for _, inst := range o.state.Instances {
		if inst.Endpoint == zone.Endpoint && inst.Managed {
			target = inst
			break
		}
	}
	if target == nil {
		return nil
	}

	// Only remove if this is the last zone on the instance
	remaining := 0
	for _, z := range target.Zones {
		if z != zone.Name {
			remaining++
		}
	}
	if remaining > 0 {
		return nil
	}

	// Stop and remove container
	ct, err := o.ros.GetContainer(ctx, target.Name)
	if err == nil {
		if ct.IsRunning() {
			_ = o.ros.StopContainer(ctx, ct.ID)
		}
		_ = o.ros.RemoveContainer(ctx, ct.ID)
	}

	// Release veth
	if target.VethName != "" {
		_ = o.netMgr.ReleaseInterface(ctx, target.VethName)
	}

	// Unregister from lifecycle
	o.lcMgr.Unregister(target.Name)

	delete(o.state.Instances, target.Name)
	o.log.Infow("dedicated MicroDNS instance removed", "name", target.Name)
	return nil
}

func (o *Operator) waitForMicroDNS(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for MicroDNS at %s", endpoint)
		case <-tick.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/zones", nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// ─── State Persistence ──────────────────────────────────────────────────────

func (o *Operator) loadState() error {
	path := o.cfg.StatePath
	if path == "" {
		path = "/etc/microkube/dzo-state.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing state: %w", err)
	}

	// Ensure maps are initialized
	if state.Zones == nil {
		state.Zones = make(map[string]*Zone)
	}
	if state.Instances == nil {
		state.Instances = make(map[string]*MicroDNSInstance)
	}

	o.state = &state
	o.log.Infow("loaded state", "zones", len(state.Zones), "instances", len(state.Instances))
	return nil
}

func (o *Operator) saveState() error {
	path := o.cfg.StatePath
	if path == "" {
		path = "/etc/microkube/dzo-state.yaml"
	}

	data, err := yaml.Marshal(o.state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing state to %s: %w", path, err)
	}
	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func (o *Operator) findNetwork(name string) *config.NetworkDef {
	for i := range o.networks {
		if o.networks[i].Name == name {
			return &o.networks[i]
		}
	}
	return nil
}

func parentZone(name string) string {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func instanceNameFromEndpoint(endpoint, networkName string) string {
	return fmt.Sprintf("mdns-%s", networkName)
}

func removeString(ss []string, s string) []string {
	out := make([]string, 0, len(ss))
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}
