package network

import (
	"context"
	"errors"
)

// ErrNotSupported is returned when a driver does not support an operation.
var ErrNotSupported = errors.New("operation not supported by this driver")

// NetworkDriver abstracts physical network operations across different backends
// (RouterOS REST API, Linux netlink, etc). The Manager calls these methods
// instead of talking to a specific client directly.
type NetworkDriver interface {
	// Bridge/switch operations
	CreateBridge(ctx context.Context, name string, opts BridgeOpts) error
	DeleteBridge(ctx context.Context, name string) error
	ListBridges(ctx context.Context) ([]BridgeInfo, error)

	// Port operations (veth for containers)
	CreatePort(ctx context.Context, name, address, gateway string) error
	DeletePort(ctx context.Context, name string) error
	AttachPort(ctx context.Context, bridge, port string) error
	DetachPort(ctx context.Context, bridge, port string) error
	ListPorts(ctx context.Context) ([]PortInfo, error)

	// VLAN operations
	SetPortVLAN(ctx context.Context, port string, vid int, tagged bool) error
	RemovePortVLAN(ctx context.Context, port string, vid int) error

	// Tunnel operations
	CreateTunnel(ctx context.Context, name string, spec TunnelSpec) error
	DeleteTunnel(ctx context.Context, name string) error

	// Introspection
	NodeName() string
	Capabilities() DriverCapabilities
}

// DriverCapabilities advertises which optional features a driver supports.
type DriverCapabilities struct {
	VLANs   bool
	Tunnels bool
	ACLs    bool
}

// BridgeOpts are options for CreateBridge.
type BridgeOpts struct {
	VLAN   int               // default PVID, 0 = none
	MTU    int               // 0 = driver default
	Labels map[string]string // arbitrary metadata
}

// BridgeInfo describes a bridge returned by ListBridges.
type BridgeInfo struct {
	Name string
	ID   string // backend-specific ID (RouterOS .id, netlink index, etc.)
}

// PortInfo describes a port/veth returned by ListPorts.
type PortInfo struct {
	Name    string
	Address string // IP/mask or empty
	Gateway string
	Bridge  string // bridge name this port is attached to, if any
	// OwnedByMkube is true only when the driver can prove mkube created this
	// port (the ownership comment on RouterOS). Reapers must not remove a
	// port without it — a foreign veth (sbregistry's builder, a manual probe)
	// is not an orphan, whatever its name looks like (#18, #26).
	OwnedByMkube bool
}

// TunnelSpec describes a tunnel to create.
type TunnelSpec struct {
	Type     string // "vxlan", "wireguard", "gre"
	LocalIP  string
	RemoteIP string
	VNI      int // VXLAN network identifier
}
