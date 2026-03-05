package driver

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/routeros"
)

// RouterOS implements network.NetworkDriver by wrapping a routeros.Client.
// Bridges on RouterOS are pre-created; CreateBridge/DeleteBridge return
// ErrNotSupported.
type RouterOS struct {
	client   *routeros.Client
	nodeName string
	log      *zap.SugaredLogger
}

// NewRouterOS returns a NetworkDriver backed by a RouterOS REST API client.
func NewRouterOS(client *routeros.Client, nodeName string, log *zap.SugaredLogger) *RouterOS {
	return &RouterOS{
		client:   client,
		nodeName: nodeName,
		log:      log.Named("ros-driver"),
	}
}

// ─── Bridge Operations ───────────────────────────────────────────────────────

func (d *RouterOS) CreateBridge(ctx context.Context, name string, _ network.BridgeOpts) error {
	d.log.Infow("creating bridge", "name", name)
	return d.client.CreateBridge(ctx, name)
}

func (d *RouterOS) DeleteBridge(ctx context.Context, name string) error {
	d.log.Infow("deleting bridge", "name", name)
	return d.client.DeleteBridge(ctx, name)
}

func (d *RouterOS) ListBridges(ctx context.Context) ([]network.BridgeInfo, error) {
	bridges, err := d.client.ListBridges(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]network.BridgeInfo, len(bridges))
	for i, b := range bridges {
		out[i] = network.BridgeInfo{Name: b.Name, ID: b.ID}
	}
	return out, nil
}

// ─── Port Operations ─────────────────────────────────────────────────────────

func (d *RouterOS) CreatePort(ctx context.Context, name, address, gateway string) error {
	return d.client.CreateVeth(ctx, name, address, gateway)
}

func (d *RouterOS) DeletePort(ctx context.Context, name string) error {
	return d.client.RemoveVeth(ctx, name)
}

func (d *RouterOS) AttachPort(ctx context.Context, bridge, port string) error {
	return d.client.AddBridgePort(ctx, bridge, port)
}

func (d *RouterOS) DetachPort(_ context.Context, _, _ string) error {
	// RouterOS removes bridge port assignment when the veth is removed.
	// Explicit detach not needed; return nil for compatibility.
	return nil
}

func (d *RouterOS) ListPorts(ctx context.Context) ([]network.PortInfo, error) {
	veths, err := d.client.ListVeths(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]network.PortInfo, len(veths))
	for i, v := range veths {
		out[i] = network.PortInfo{
			Name:    v.Name,
			Address: v.Address,
			Gateway: v.Gateway,
		}
	}
	return out, nil
}

// ─── VLAN Operations ─────────────────────────────────────────────────────────

func (d *RouterOS) SetPortVLAN(ctx context.Context, port string, vid int, tagged bool) error {
	return d.client.AddBridgeVLAN(ctx, port, vid, tagged, !tagged, !tagged)
}

func (d *RouterOS) RemovePortVLAN(ctx context.Context, port string, vid int) error {
	return d.client.RemoveBridgeVLAN(ctx, port, vid)
}

// ─── Tunnel Operations ───────────────────────────────────────────────────────

// CreateTunnel creates an EoIP tunnel on RouterOS (the closest equivalent to VXLAN).
// RouterOS EoIP uses tunnel-id as the VNI equivalent.
func (d *RouterOS) CreateTunnel(ctx context.Context, name string, spec network.TunnelSpec) error {
	switch spec.Type {
	case "eoip":
		return d.client.CreateEoIPTunnel(ctx, name, spec.LocalIP, spec.RemoteIP, spec.VNI)
	default:
		return fmt.Errorf("RouterOS tunnel type %q not supported (use 'eoip')", spec.Type)
	}
}

func (d *RouterOS) DeleteTunnel(ctx context.Context, name string) error {
	return d.client.DeleteEoIPTunnel(ctx, name)
}

// ─── Introspection ───────────────────────────────────────────────────────────

func (d *RouterOS) NodeName() string {
	return d.nodeName
}

func (d *RouterOS) Capabilities() network.DriverCapabilities {
	return network.DriverCapabilities{
		VLANs:   true,
		Tunnels: true,
		ACLs:    false,
	}
}

// Ensure RouterOS implements NetworkDriver at compile time.
var _ network.NetworkDriver = (*RouterOS)(nil)
