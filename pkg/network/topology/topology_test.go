package topology

import (
	"context"
	"testing"

	"github.com/glennswest/mkube/pkg/network"
)

// stubDriver is a minimal NetworkDriver for testing topology operations.
type stubDriver struct {
	name string
	caps network.DriverCapabilities
}

func (d *stubDriver) CreateBridge(context.Context, string, network.BridgeOpts) error { return nil }
func (d *stubDriver) DeleteBridge(context.Context, string) error                     { return nil }
func (d *stubDriver) ListBridges(context.Context) ([]network.BridgeInfo, error)      { return nil, nil }
func (d *stubDriver) CreatePort(context.Context, string, string, string) error       { return nil }
func (d *stubDriver) DeletePort(context.Context, string) error                       { return nil }
func (d *stubDriver) AttachPort(context.Context, string, string) error               { return nil }
func (d *stubDriver) DetachPort(context.Context, string, string) error               { return nil }
func (d *stubDriver) ListPorts(context.Context) ([]network.PortInfo, error)          { return nil, nil }
func (d *stubDriver) SetPortComment(context.Context, string, string) error          { return nil }
func (d *stubDriver) SetPortVLAN(context.Context, string, int, bool) error           { return nil }
func (d *stubDriver) RemovePortVLAN(context.Context, string, int) error              { return nil }
func (d *stubDriver) CreateTunnel(context.Context, string, network.TunnelSpec) error { return nil }
func (d *stubDriver) DeleteTunnel(context.Context, string) error                     { return nil }
func (d *stubDriver) NodeName() string                                               { return d.name }
func (d *stubDriver) Capabilities() network.DriverCapabilities                       { return d.caps }

func TestAddAndListNodes(t *testing.T) {
	topo := New()

	err := topo.AddNode(Node{
		Name:    "ros1",
		Address: "192.168.200.1",
		Driver:  &stubDriver{name: "ros1"},
	})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	err = topo.AddNode(Node{
		Name:         "linux1",
		Address:      "10.0.0.5",
		Driver:       &stubDriver{name: "linux1", caps: network.DriverCapabilities{VLANs: true, Tunnels: true}},
		Capabilities: network.DriverCapabilities{VLANs: true, Tunnels: true},
	})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if topo.NodeCount() != 2 {
		t.Errorf("expected 2 nodes, got %d", topo.NodeCount())
	}

	nodes := topo.ListNodes()
	if len(nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(nodes))
	}
}

func TestAddDuplicateNode(t *testing.T) {
	topo := New()

	_ = topo.AddNode(Node{Name: "ros1", Driver: &stubDriver{name: "ros1"}})
	err := topo.AddNode(Node{Name: "ros1", Driver: &stubDriver{name: "ros1"}})
	if err == nil {
		t.Error("expected error for duplicate node")
	}
}

func TestGetNode(t *testing.T) {
	topo := New()
	_ = topo.AddNode(Node{Name: "ros1", Address: "192.168.200.1", Driver: &stubDriver{name: "ros1"}})

	n, err := topo.GetNode("ros1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if n.Address != "192.168.200.1" {
		t.Errorf("expected address 192.168.200.1, got %q", n.Address)
	}

	_, err = topo.GetNode("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent node")
	}
}

func TestRemoveNode(t *testing.T) {
	topo := New()
	_ = topo.AddNode(Node{Name: "ros1", Driver: &stubDriver{name: "ros1"}})

	topo.RemoveNode("ros1")
	if topo.NodeCount() != 0 {
		t.Error("expected 0 nodes after removal")
	}
}

func TestDriverFor(t *testing.T) {
	topo := New()
	drv := &stubDriver{name: "ros1"}
	_ = topo.AddNode(Node{Name: "ros1", Driver: drv})

	got, err := topo.DriverFor("ros1")
	if err != nil {
		t.Fatalf("DriverFor: %v", err)
	}
	if got.NodeName() != "ros1" {
		t.Errorf("expected driver node name 'ros1', got %q", got.NodeName())
	}

	_, err = topo.DriverFor("missing")
	if err == nil {
		t.Error("expected error for missing node")
	}
}

func TestNodesWithCapability(t *testing.T) {
	topo := New()
	_ = topo.AddNode(Node{
		Name:         "ros1",
		Driver:       &stubDriver{name: "ros1"},
		Capabilities: network.DriverCapabilities{},
	})
	_ = topo.AddNode(Node{
		Name:         "linux1",
		Driver:       &stubDriver{name: "linux1"},
		Capabilities: network.DriverCapabilities{VLANs: true, Tunnels: true},
	})
	_ = topo.AddNode(Node{
		Name:         "linux2",
		Driver:       &stubDriver{name: "linux2"},
		Capabilities: network.DriverCapabilities{VLANs: true},
	})

	vlanNodes := topo.NodesWithCapability("vlans")
	if len(vlanNodes) != 2 {
		t.Errorf("expected 2 VLAN-capable nodes, got %d", len(vlanNodes))
	}

	tunnelNodes := topo.NodesWithCapability("tunnels")
	if len(tunnelNodes) != 1 {
		t.Errorf("expected 1 tunnel-capable node, got %d", len(tunnelNodes))
	}

	aclNodes := topo.NodesWithCapability("acls")
	if len(aclNodes) != 0 {
		t.Errorf("expected 0 ACL-capable nodes, got %d", len(aclNodes))
	}
}
