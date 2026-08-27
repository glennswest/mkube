package driver

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/stormbase"
	stormdpb "github.com/glennswest/mkube/pkg/stormbase/proto"
)

// StormBase implements network.NetworkDriver for StormBase nodes.
// Networking on StormBase is managed by stormnet (eBPF dataplane), so most
// operations delegate to stormd's gRPC API or are no-ops (handled internally).
type StormBase struct {
	client   *stormbase.Client
	nodeName string
	log      *zap.SugaredLogger
}

// NewStormBase returns a NetworkDriver backed by a stormd gRPC client.
func NewStormBase(client *stormbase.Client, nodeName string, log *zap.SugaredLogger) *StormBase {
	return &StormBase{
		client:   client,
		nodeName: nodeName,
		log:      log.Named("sb-driver"),
	}
}

// ── Bridge Operations ────────────────────────────────────────────────────────
// StormBase uses BPF-based routing, not software bridges. Bridges are a no-op.

func (d *StormBase) CreateBridge(_ context.Context, _ string, _ network.BridgeOpts) error {
	// stormnet handles pod routing via eBPF; no bridge to create.
	return nil
}

func (d *StormBase) DeleteBridge(_ context.Context, _ string) error {
	return nil
}

func (d *StormBase) ListBridges(_ context.Context) ([]network.BridgeInfo, error) {
	// No bridges on StormBase. Return empty list.
	return nil, nil
}

// ── Port Operations ──────────────────────────────────────────────────────────
// Container veths are created by stormd's dataplane.setup_container on
// WorkloadCreate. CreatePort triggers a WorkloadCreate with just the network
// config. In practice, the provider calls CreateContainer which handles
// networking internally. These methods exist for the network manager.

func (d *StormBase) CreatePort(ctx context.Context, name, address, gateway string) error {
	d.log.Infow("creating veth (stormd handles internally)", "name", name, "address", address, "gateway", gateway)
	// stormd creates veths as part of workload lifecycle.
	// The network manager calls this to pre-allocate the veth name + IP.
	// On StormBase, this is a record-keeping operation — the actual veth
	// is created when the workload starts.
	return nil
}

func (d *StormBase) DeletePort(ctx context.Context, name string) error {
	d.log.Infow("deleting veth (stormd handles internally)", "name", name)
	// stormd tears down veths when workloads stop/remove.
	return nil
}

func (d *StormBase) AttachPort(_ context.Context, _, _ string) error {
	// No bridges — BPF routing replaces bridge port attachment.
	return nil
}

func (d *StormBase) DetachPort(_ context.Context, _, _ string) error {
	return nil
}

func (d *StormBase) ListPorts(ctx context.Context) ([]network.PortInfo, error) {
	// List running workloads and extract their network info.
	containers, err := d.client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing workloads for ports: %w", err)
	}
	var ports []network.PortInfo
	for _, ct := range containers {
		if ct.Interface != "" {
			ports = append(ports, network.PortInfo{
				Name:    ct.Interface,
				Address: ct.DNS, // pod IP is in the container record
				// Every interface a StormBase workload reports was created by
				// mkube's own deploy path — there is no foreign-veth case here.
				OwnedByMkube: true,
			})
		}
	}
	return ports, nil
}

// ── VLAN Operations ──────────────────────────────────────────────────────────
// VLANs are managed by MikroTik nodes, not StormBase.

func (d *StormBase) SetPortVLAN(_ context.Context, _ string, _ int, _ bool) error {
	return network.ErrNotSupported
}

func (d *StormBase) RemovePortVLAN(_ context.Context, _ string, _ int) error {
	return network.ErrNotSupported
}

// ── Tunnel Operations ────────────────────────────────────────────────────────
// StormBase uses WireGuard mesh managed via MeshUpdate RPC.

func (d *StormBase) CreateTunnel(ctx context.Context, name string, spec network.TunnelSpec) error {
	if spec.Type != "wireguard" {
		return fmt.Errorf("StormBase only supports wireguard tunnels, got %q", spec.Type)
	}
	// WireGuard mesh is configured via MeshUpdate RPC, not per-tunnel creation.
	// This would be called during cluster mesh setup.
	d.log.Infow("tunnel creation via MeshUpdate", "name", name, "remote", spec.RemoteIP)
	_, err := d.client.GRPCClient().MeshUpdate(ctx, &stormdpb.MeshUpdateRequest{
		Nodes: []*stormdpb.MeshNode{
			{
				NodeId:   name,
				Endpoint: spec.RemoteIP,
				TunnelIp: spec.LocalIP,
			},
		},
	})
	return err
}

func (d *StormBase) DeleteTunnel(ctx context.Context, name string) error {
	d.log.Infow("tunnel deletion (mesh update)", "name", name)
	// Removing a peer from mesh — send update without that node.
	// The full mesh update is orchestrated by mkube, so this is a placeholder.
	return nil
}

// ── Introspection ────────────────────────────────────────────────────────────

func (d *StormBase) NodeName() string {
	return d.nodeName
}

func (d *StormBase) Capabilities() network.DriverCapabilities {
	return network.DriverCapabilities{
		VLANs:   false, // VLAN management stays on MikroTik
		Tunnels: true,  // WireGuard mesh
		ACLs:    true,  // BPF-based network policy
	}
}

// Ensure StormBase implements NetworkDriver at compile time.
var _ network.NetworkDriver = (*StormBase)(nil)
