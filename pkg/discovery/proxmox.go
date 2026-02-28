package discovery

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/proxmox"
)

// DiscoverProxmox queries a Proxmox VE node and builds an inventory of
// LXC containers managed by mkube (within the configured VMID range).
func DiscoverProxmox(ctx context.Context, client *proxmox.Client, log *zap.SugaredLogger) (*Inventory, error) {
	log.Info("starting proxmox discovery")

	containers, err := client.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing LXC containers: %w", err)
	}

	var discovered []Container
	for _, ct := range containers {
		// Only include containers in our managed VMID range
		if !client.VMIDs().InRange(ct.VMID) {
			continue
		}

		// Get container config for network details
		cfg, err := client.GetContainerConfig(ctx, ct.VMID)
		if err != nil {
			log.Warnw("failed to get container config", "vmid", ct.VMID, "error", err)
			continue
		}

		// Register in VMID allocator
		client.VMIDs().MarkUsed(ct.VMID, ct.Name)

		dc := Container{
			Name:      ct.Name,
			Status:    ct.Status,
			Hostname:  cfg.Hostname,
			StartBoot: cfg.OnBoot == 1,
		}

		// Extract network info from net0 config
		if cfg.Net0 != "" {
			parts := parseProxmoxNet(cfg.Net0)
			dc.IP = parts.ip
			dc.CIDR = parts.cidr
			dc.Bridge = parts.bridge
			dc.Gateway = parts.gateway
		}

		dc.DNS = cfg.Nameserver

		discovered = append(discovered, dc)
	}

	// Get node status for system info
	nodeStatus, err := client.GetNodeStatus(ctx)
	if err != nil {
		log.Warnw("failed to get node status", "error", err)
	}

	// Discover network interfaces (bridges)
	ifaces, err := client.ListNetworkInterfaces(ctx)
	if err != nil {
		log.Warnw("failed to list network interfaces", "error", err)
	}

	var networks []Network
	for _, iface := range ifaces {
		if iface.Type == "bridge" && iface.CIDR != "" {
			networks = append(networks, Network{
				Bridge:  iface.Name,
				CIDR:    iface.CIDR,
				Gateway: iface.Address,
			})
		}
	}

	inv := &Inventory{
		Containers: discovered,
		Networks:   networks,
	}

	if nodeStatus != nil {
		log.Infow("proxmox node info",
			"version", nodeStatus.PVEVersion,
			"cpu_cores", nodeStatus.CPUInfo.Cores,
			"memory_total", nodeStatus.Memory.Total,
		)
	}

	log.Infow("proxmox discovery complete",
		"containers", len(discovered),
		"networks", len(networks),
	)
	return inv, nil
}

// proxmoxNetParts holds parsed net0 config fields.
type proxmoxNetParts struct {
	ip      string
	cidr    string
	bridge  string
	gateway string
}

// parseProxmoxNet parses a Proxmox net config string.
// Format: "name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1"
func parseProxmoxNet(netConfig string) proxmoxNetParts {
	var p proxmoxNetParts
	for _, part := range strings.Split(netConfig, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ip":
			p.cidr = kv[1]
			if idx := strings.Index(kv[1], "/"); idx >= 0 {
				p.ip = kv[1][:idx]
			} else {
				p.ip = kv[1]
			}
		case "gw":
			p.gateway = kv[1]
		case "bridge":
			p.bridge = kv[1]
		}
	}
	return p
}

// BuildLifecycleUnitsFromProxmox converts discovered Proxmox containers
// into lifecycle.ContainerUnit entries for keepalive and probe tracking.
func BuildLifecycleUnitsFromProxmox(inv *Inventory) []lifecycle.ContainerUnit {
	var units []lifecycle.ContainerUnit
	for _, ct := range inv.Containers {
		if ct.Status != "running" {
			continue
		}
		unit := lifecycle.ContainerUnit{
			Name:          ct.Name,
			ContainerIP:   ct.IP,
			RestartPolicy: "Always",
			StartOnBoot:   ct.StartBoot,
			Managed:       false,
			Probes:        InferProbes(ct),
		}
		units = append(units, unit)
	}
	return units
}

// BuildProxmoxNetConfig builds a Proxmox-format net0 string from network params.
func BuildProxmoxNetConfig(bridge, ip, cidrMask, gateway string) string {
	parts := []string{
		"name=eth0",
		fmt.Sprintf("bridge=%s", bridge),
	}
	if ip != "" {
		ipWithMask := ip
		if cidrMask != "" && !strings.Contains(ip, "/") {
			ipWithMask = ip + "/" + cidrMask
		}
		parts = append(parts, fmt.Sprintf("ip=%s", ipWithMask))
	}
	if gateway != "" {
		parts = append(parts, fmt.Sprintf("gw=%s", gateway))
	}
	return strings.Join(parts, ",")
}
