//go:build linux

package driver

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"

	nw "github.com/glennswest/mkube/pkg/network"
)

// Linux implements nw.NetworkDriver using netlink syscalls.
// This driver is used on StormOS and other Linux hosts.
type Linux struct {
	nodeName string
	log      *zap.SugaredLogger
}

// NewLinux returns a NetworkDriver backed by Linux netlink.
func NewLinux(nodeName string, log *zap.SugaredLogger) *Linux {
	return &Linux{
		nodeName: nodeName,
		log:      log.Named("linux-driver"),
	}
}

// ─── Bridge Operations ───────────────────────────────────────────────────────

func (d *Linux) CreateBridge(ctx context.Context, name string, opts nw.BridgeOpts) error {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}
	if opts.MTU > 0 {
		br.LinkAttrs.MTU = opts.MTU
	}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("netlink bridge add %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return fmt.Errorf("netlink bridge up %s: %w", name, err)
	}
	d.log.Infow("bridge created", "name", name)
	return nil
}

func (d *Linux) DeleteBridge(ctx context.Context, name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netlink lookup %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netlink bridge del %s: %w", name, err)
	}
	d.log.Infow("bridge deleted", "name", name)
	return nil
}

func (d *Linux) ListBridges(ctx context.Context) ([]nw.BridgeInfo, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("netlink link list: %w", err)
	}
	var out []nw.BridgeInfo
	for _, l := range links {
		if _, ok := l.(*netlink.Bridge); ok {
			out = append(out, nw.BridgeInfo{
				Name: l.Attrs().Name,
				ID:   fmt.Sprintf("%d", l.Attrs().Index),
			})
		}
	}
	return out, nil
}

// ─── Port Operations ─────────────────────────────────────────────────────────

func (d *Linux) CreatePort(ctx context.Context, name, address, gateway string) error {
	peerName := name + "-p"
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("netlink veth add %s: %w", name, err)
	}

	// Parse and add address
	addr, err := netlink.ParseAddr(address)
	if err != nil {
		_ = netlink.LinkDel(veth)
		return fmt.Errorf("parsing address %s: %w", address, err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netlink lookup %s after create: %w", name, err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil {
		_ = netlink.LinkDel(veth)
		return fmt.Errorf("netlink addr add %s on %s: %w", address, name, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		_ = netlink.LinkDel(veth)
		return fmt.Errorf("netlink link up %s: %w", name, err)
	}

	d.log.Infow("port created", "name", name, "address", address)
	return nil
}

func (d *Linux) DeletePort(ctx context.Context, name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netlink lookup %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netlink del %s: %w", name, err)
	}
	d.log.Infow("port deleted", "name", name)
	return nil
}

func (d *Linux) AttachPort(ctx context.Context, bridge, port string) error {
	br, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netlink lookup bridge %s: %w", bridge, err)
	}
	p, err := netlink.LinkByName(port)
	if err != nil {
		return fmt.Errorf("netlink lookup port %s: %w", port, err)
	}
	if err := netlink.LinkSetMaster(p, br); err != nil {
		return fmt.Errorf("netlink set master %s -> %s: %w", port, bridge, err)
	}
	d.log.Infow("port attached", "bridge", bridge, "port", port)
	return nil
}

func (d *Linux) SetPortComment(_ context.Context, _, _ string) error {
	return nil // no comment/label support on this backend
}

func (d *Linux) DetachPort(ctx context.Context, bridge, port string) error {
	p, err := netlink.LinkByName(port)
	if err != nil {
		return fmt.Errorf("netlink lookup port %s: %w", port, err)
	}
	if err := netlink.LinkSetNoMaster(p); err != nil {
		return fmt.Errorf("netlink set no master %s: %w", port, err)
	}
	d.log.Infow("port detached", "bridge", bridge, "port", port)
	return nil
}

func (d *Linux) ListPorts(ctx context.Context) ([]nw.PortInfo, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("netlink link list: %w", err)
	}
	var out []nw.PortInfo
	for _, l := range links {
		if _, ok := l.(*netlink.Veth); ok {
			pi := nw.PortInfo{Name: l.Attrs().Name}

			// Get address if assigned
			addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
			if err == nil && len(addrs) > 0 {
				pi.Address = addrs[0].IPNet.String()
			}

			// Get bridge master
			if l.Attrs().MasterIndex > 0 {
				master, err := netlink.LinkByIndex(l.Attrs().MasterIndex)
				if err == nil {
					pi.Bridge = master.Attrs().Name
				}
			}

			out = append(out, pi)
		}
	}
	return out, nil
}

// ─── VLAN Operations ─────────────────────────────────────────────────────────

func (d *Linux) SetPortVLAN(ctx context.Context, port string, vid int, tagged bool) error {
	link, err := netlink.LinkByName(port)
	if err != nil {
		return fmt.Errorf("netlink lookup %s: %w", port, err)
	}
	pvid := !tagged // untagged ports get PVID
	untagged := !tagged
	if err := netlink.BridgeVlanAdd(link, uint16(vid), pvid, untagged, false, false); err != nil {
		return fmt.Errorf("netlink bridge vlan add vid=%d on %s: %w", vid, port, err)
	}
	d.log.Infow("VLAN set", "port", port, "vid", vid, "tagged", tagged)
	return nil
}

func (d *Linux) RemovePortVLAN(ctx context.Context, port string, vid int) error {
	link, err := netlink.LinkByName(port)
	if err != nil {
		return fmt.Errorf("netlink lookup %s: %w", port, err)
	}
	if err := netlink.BridgeVlanDel(link, uint16(vid), false, false, false, false); err != nil {
		return fmt.Errorf("netlink bridge vlan del vid=%d on %s: %w", vid, port, err)
	}
	d.log.Infow("VLAN removed", "port", port, "vid", vid)
	return nil
}

// ─── Tunnel Operations ───────────────────────────────────────────────────────

func (d *Linux) CreateTunnel(ctx context.Context, name string, spec nw.TunnelSpec) error {
	switch spec.Type {
	case "vxlan":
		return d.createVXLAN(name, spec)
	default:
		return fmt.Errorf("tunnel type %q not supported", spec.Type)
	}
}

func (d *Linux) createVXLAN(name string, spec nw.TunnelSpec) error {
	localIP := net.ParseIP(spec.LocalIP)
	remoteIP := net.ParseIP(spec.RemoteIP)
	if localIP == nil || remoteIP == nil {
		return fmt.Errorf("invalid tunnel IPs: local=%s remote=%s", spec.LocalIP, spec.RemoteIP)
	}

	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		VxlanId:   spec.VNI,
		SrcAddr:   localIP,
		Group:     remoteIP,
		Port:      4789,
	}
	if err := netlink.LinkAdd(vxlan); err != nil {
		return fmt.Errorf("netlink vxlan add %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(vxlan); err != nil {
		return fmt.Errorf("netlink vxlan up %s: %w", name, err)
	}
	d.log.Infow("VXLAN tunnel created", "name", name, "vni", spec.VNI)
	return nil
}

func (d *Linux) DeleteTunnel(ctx context.Context, name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("netlink lookup %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netlink del %s: %w", name, err)
	}
	d.log.Infow("tunnel deleted", "name", name)
	return nil
}

// ─── Introspection ───────────────────────────────────────────────────────────

func (d *Linux) NodeName() string {
	return d.nodeName
}

func (d *Linux) Capabilities() nw.DriverCapabilities {
	return nw.DriverCapabilities{
		VLANs:   true,
		Tunnels: true,
		ACLs:    false,
	}
}

// Ensure Linux implements NetworkDriver at compile time.
var _ nw.NetworkDriver = (*Linux)(nil)
