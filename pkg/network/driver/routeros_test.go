package driver

import (
	"context"
	"strings"
	"testing"

	rosapi "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/routeros"
)

// mockExec creates an exec function that handles the standard RouterOS
// commands used by the network driver.
func mockExec(t *testing.T) func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
	t.Helper()
	return func(ctx context.Context, words ...string) (*rosapi.Reply, error) {
		cmd := words[0]
		switch cmd {
		case "/interface/bridge/print":
			return &rosapi.Reply{
				Re: []*proto.Sentence{
					{Word: "!re", Map: map[string]string{".id": "*1", "name": "bridge"}},
					{Word: "!re", Map: map[string]string{".id": "*2", "name": "containers"}},
				},
				Done: &proto.Sentence{Word: "!done"},
			}, nil
		case "/interface/veth/print":
			return &rosapi.Reply{
				Re: []*proto.Sentence{
					{Word: "!re", Map: map[string]string{".id": "*10", "name": "veth-app1", "address": "192.168.200.2/24", "gateway": "192.168.200.1"}},
				},
				Done: &proto.Sentence{Word: "!done"},
			}, nil
		case "/interface/veth/add":
			// Verify name is present
			hasName := false
			for _, w := range words[1:] {
				if strings.HasPrefix(w, "=name=") {
					hasName = true
				}
			}
			if !hasName {
				return nil, &rosapi.DeviceError{Sentence: &proto.Sentence{Map: map[string]string{"message": "missing name"}}}
			}
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/veth/remove":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/bridge/port/print":
			return &rosapi.Reply{
				Re:   []*proto.Sentence{},
				Done: &proto.Sentence{Word: "!done"},
			}, nil
		case "/interface/bridge/port/add":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/bridge/add":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/bridge/remove":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/eoip/add":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		case "/interface/eoip/print":
			return &rosapi.Reply{
				Re:   []*proto.Sentence{},
				Done: &proto.Sentence{Word: "!done"},
			}, nil
		case "/interface/eoip/remove":
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		default:
			t.Errorf("unexpected command: %v", words)
			return &rosapi.Reply{Done: &proto.Sentence{Word: "!done"}}, nil
		}
	}
}

func newTestDriver(t *testing.T) *RouterOS {
	t.Helper()
	cfg := config.RouterOSConfig{
		Address:  "192.168.200.1:8728",
		User:     "admin",
		Password: "test",
		RESTURL:  "https://192.168.200.1/rest",
	}
	client := routeros.NewClientForTest(cfg, mockExec(t))
	log := zap.NewNop().Sugar()
	return NewRouterOS(client, "test-node", log)
}

func TestListBridges(t *testing.T) {
	d := newTestDriver(t)

	bridges, err := d.ListBridges(context.Background())
	if err != nil {
		t.Fatalf("ListBridges: %v", err)
	}
	if len(bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %d", len(bridges))
	}
	if bridges[0].Name != "bridge" {
		t.Errorf("expected first bridge 'bridge', got %q", bridges[0].Name)
	}
	if bridges[1].Name != "containers" {
		t.Errorf("expected second bridge 'containers', got %q", bridges[1].Name)
	}
}

func TestListPorts(t *testing.T) {
	d := newTestDriver(t)

	ports, err := d.ListPorts(context.Background())
	if err != nil {
		t.Fatalf("ListPorts: %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(ports))
	}
	if ports[0].Name != "veth-app1" {
		t.Errorf("expected port 'veth-app1', got %q", ports[0].Name)
	}
	if ports[0].Address != "192.168.200.2/24" {
		t.Errorf("expected address '192.168.200.2/24', got %q", ports[0].Address)
	}
}

func TestCreateAndDeletePort(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	if err := d.CreatePort(ctx, "veth-test", "10.0.0.2/24", "10.0.0.1"); err != nil {
		t.Fatalf("CreatePort: %v", err)
	}

	if err := d.DeletePort(ctx, "veth-test"); err != nil {
		t.Fatalf("DeletePort: %v", err)
	}
}

func TestAttachPort(t *testing.T) {
	d := newTestDriver(t)

	if err := d.AttachPort(context.Background(), "containers", "veth-test"); err != nil {
		t.Fatalf("AttachPort: %v", err)
	}
}

func TestDetachPortNoOp(t *testing.T) {
	d := newTestDriver(t)

	// DetachPort is a no-op on RouterOS
	if err := d.DetachPort(context.Background(), "containers", "veth-test"); err != nil {
		t.Fatalf("DetachPort should be no-op: %v", err)
	}
}

func TestCreateAndDeleteBridge(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	if err := d.CreateBridge(ctx, "bridge-g12", network.BridgeOpts{}); err != nil {
		t.Fatalf("CreateBridge: %v", err)
	}

	if err := d.DeleteBridge(ctx, "bridge-g12"); err != nil {
		t.Fatalf("DeleteBridge: %v", err)
	}
}

func TestUnsupportedOperations(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	// CreateTunnel with unsupported type should fail
	if err := d.CreateTunnel(ctx, "tun0", network.TunnelSpec{Type: "vxlan"}); err == nil {
		t.Error("CreateTunnel with unsupported type should return error")
	}
}

func TestNodeNameAndCapabilities(t *testing.T) {
	d := newTestDriver(t)

	if d.NodeName() != "test-node" {
		t.Errorf("expected node name 'test-node', got %q", d.NodeName())
	}

	caps := d.Capabilities()
	if !caps.VLANs {
		t.Error("RouterOS driver should support VLANs")
	}
	if !caps.Tunnels {
		t.Error("RouterOS driver should support Tunnels (EoIP)")
	}
	if caps.ACLs {
		t.Errorf("RouterOS driver should not support ACLs, got %+v", caps)
	}
}
