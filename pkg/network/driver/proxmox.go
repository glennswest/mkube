package driver

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/proxmox"
)

// Proxmox implements network.NetworkDriver for Proxmox VE nodes.
// Proxmox bridges are pre-created (vmbr0, vmbr1, etc.). Container networking
// is embedded in the container config (net0 param), not as separate veth objects.
type Proxmox struct {
	client   *proxmox.Client
	nodeName string
	log      *zap.SugaredLogger
}

// NewProxmox returns a NetworkDriver backed by a Proxmox REST API client.
func NewProxmox(client *proxmox.Client, nodeName string, log *zap.SugaredLogger) *Proxmox {
	return &Proxmox{
		client:   client,
		nodeName: nodeName,
		log:      log.Named("pve-driver"),
	}
}

// ── Bridge Operations ────────────────────────────────────────────────────────
// Proxmox bridges (vmbr*) are pre-created in the node's network config.

func (d *Proxmox) CreateBridge(_ context.Context, _ string, _ network.BridgeOpts) error {
	return network.ErrNotSupported
}

func (d *Proxmox) DeleteBridge(_ context.Context, _ string) error {
	return network.ErrNotSupported
}

func (d *Proxmox) ListBridges(ctx context.Context) ([]network.BridgeInfo, error) {
	ifaces, err := d.client.ListNetworkInterfaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing network interfaces: %w", err)
	}

	var bridges []network.BridgeInfo
	for _, iface := range ifaces {
		if iface.Type == "bridge" {
			bridges = append(bridges, network.BridgeInfo{
				Name: iface.Name,
				ID:   iface.Name,
			})
		}
	}
	return bridges, nil
}

// ── Port Operations ──────────────────────────────────────────────────────────
// Proxmox container networking is specified as part of the container config
// (net0=name=eth0,bridge=vmbr0,ip=...). Ports are not separate objects.

func (d *Proxmox) CreatePort(_ context.Context, name, address, gateway string) error {
	// No-op: networking is embedded in container spec via net0 param
	d.log.Debugw("port creation is a no-op on Proxmox", "name", name, "address", address)
	return nil
}

func (d *Proxmox) DeletePort(_ context.Context, name string) error {
	// No-op: port is destroyed with the container
	d.log.Debugw("port deletion is a no-op on Proxmox", "name", name)
	return nil
}

func (d *Proxmox) AttachPort(_ context.Context, _, _ string) error {
	// No-op: bridge assignment is in the container net0 config
	return nil
}

func (d *Proxmox) DetachPort(_ context.Context, _, _ string) error {
	return nil
}

func (d *Proxmox) ListPorts(ctx context.Context) ([]network.PortInfo, error) {
	// List containers and extract their net configs
	containers, err := d.client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers for ports: %w", err)
	}

	var ports []network.PortInfo
	for _, ct := range containers {
		if !d.client.VMIDs().InRange(ct.VMID) {
			continue
		}
		cfg, err := d.client.GetContainerConfig(ctx, ct.VMID)
		if err != nil {
			d.log.Warnw("failed to get container config for port listing", "vmid", ct.VMID, "error", err)
			continue
		}
		if cfg.Net0 != "" {
			pi := parseNetConfig(cfg.Net0)
			pi.Name = fmt.Sprintf("ct%d-eth0", ct.VMID)
			ports = append(ports, pi)
		}
	}
	return ports, nil
}

// ── VLAN Operations ──────────────────────────────────────────────────────────

func (d *Proxmox) SetPortVLAN(_ context.Context, _ string, _ int, _ bool) error {
	// Proxmox VLAN tagging is via the net config's "tag" parameter.
	// This would require updating the container config.
	return network.ErrNotSupported
}

func (d *Proxmox) RemovePortVLAN(_ context.Context, _ string, _ int) error {
	return network.ErrNotSupported
}

// ── Tunnel Operations ────────────────────────────────────────────────────────

func (d *Proxmox) CreateTunnel(_ context.Context, _ string, _ network.TunnelSpec) error {
	return network.ErrNotSupported
}

func (d *Proxmox) DeleteTunnel(_ context.Context, _ string) error {
	return network.ErrNotSupported
}

// ── Introspection ────────────────────────────────────────────────────────────

func (d *Proxmox) NodeName() string {
	return d.nodeName
}

func (d *Proxmox) Capabilities() network.DriverCapabilities {
	return network.DriverCapabilities{
		VLANs:   false, // VLAN tagging managed via container config, not bridge operations
		Tunnels: false,
		ACLs:    false,
	}
}

// parseNetConfig extracts IP, gateway, and bridge from a Proxmox net config string.
// Format: "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1"
func parseNetConfig(netConfig string) network.PortInfo {
	var pi network.PortInfo
	for _, part := range splitNetConfig(netConfig) {
		kv := splitKV(part)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ip":
			pi.Address = kv[1]
		case "gw":
			pi.Gateway = kv[1]
		case "bridge":
			pi.Bridge = kv[1]
		}
	}
	return pi
}

func splitNetConfig(s string) []string {
	return splitBy(s, ',')
}

func splitKV(s string) []string {
	return splitBy(s, '=')
}

func splitBy(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// Ensure Proxmox implements NetworkDriver at compile time.
var _ network.NetworkDriver = (*Proxmox)(nil)
