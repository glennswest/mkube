package provider

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/glennswest/mkube/pkg/routeros"
	"github.com/glennswest/mkube/pkg/runtime"
)

// getRouterOSClient extracts the underlying routeros.Client from the runtime.
// Returns nil if the backend is not RouterOS.
func (p *MicroKubeProvider) getRouterOSClient() *routeros.Client {
	if ros, ok := p.deps.Runtime.(*runtime.RouterOSRuntime); ok {
		return ros.Client()
	}
	return nil
}

// provisionNetwork creates the physical infrastructure for a new network:
// bridge, gateway IP, and DHCP relay. Only runs on RouterOS backend.
// After provisioning, it deploys managed DNS if configured.
func (p *MicroKubeProvider) provisionNetwork(ctx context.Context, net *Network) {
	log := p.deps.Logger.With("network", net.Name)
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		log.Infow("network provisioning skipped (not RouterOS backend)")
		return
	}

	bridge := net.Spec.Bridge
	if bridge == "" {
		bridge = "bridge-" + net.Name
		net.Spec.Bridge = bridge
	}

	// Check if bridge already exists
	bridges, err := rosClient.ListBridges(ctx)
	if err != nil {
		log.Warnw("failed to list bridges for provisioning", "error", err)
		return
	}
	bridgeExists := false
	for _, b := range bridges {
		if b.Name == bridge {
			bridgeExists = true
			break
		}
	}

	if !bridgeExists {
		// 1. Create bridge
		if err := rosClient.CreateBridge(ctx, bridge); err != nil {
			log.Warnw("failed to create bridge", "bridge", bridge, "error", err)
			return
		}
		log.Infow("created bridge", "bridge", bridge)
	}

	// 2. Assign gateway IP (if not already assigned)
	if net.Spec.Gateway != "" && net.Spec.CIDR != "" {
		gatewayAddr := net.Spec.Gateway + "/" + cidrMask(net.Spec.CIDR)
		addrs, err := rosClient.ListIPAddresses(ctx)
		if err != nil {
			log.Warnw("failed to list IPs for provisioning", "error", err)
		} else {
			alreadyAssigned := false
			for _, a := range addrs {
				if a.Interface == bridge && a.Address == gatewayAddr {
					alreadyAssigned = true
					break
				}
			}
			if !alreadyAssigned {
				if err := rosClient.AddIPAddress(ctx, gatewayAddr, bridge); err != nil {
					log.Warnw("failed to assign gateway IP", "address", gatewayAddr, "bridge", bridge, "error", err)
				} else {
					log.Infow("assigned gateway IP", "address", gatewayAddr, "bridge", bridge)
				}
			}
		}
	}

	// 3. Create DHCP relay (if DHCP enabled and DNS server is on this network)
	if net.Spec.DHCP.Enabled && net.Spec.DNS.Server != "" && net.Spec.Gateway != "" {
		relayName := "relay-" + net.Name
		relays, err := rosClient.ListDHCPRelays(ctx)
		if err != nil {
			log.Warnw("failed to list DHCP relays", "error", err)
		} else {
			relayExists := false
			for _, r := range relays {
				if r.Interface == bridge || r.Name == relayName {
					relayExists = true
					break
				}
			}
			if !relayExists {
				if err := rosClient.AddDHCPRelay(ctx, relayName, bridge, net.Spec.DNS.Server, net.Spec.Gateway); err != nil {
					log.Warnw("failed to create DHCP relay", "relay", relayName, "error", err)
				} else {
					log.Infow("created DHCP relay", "relay", relayName, "interface", bridge, "server", net.Spec.DNS.Server)
				}
			}
		}
	}

	// 3b. Ensure NAT exemption for DHCP relay
	p.ensureDHCPRelayNAT(ctx, net)

	// 4. Mark as provisioned
	net.Spec.Provisioned = true
	if p.deps.Store != nil && p.deps.Store.Networks != nil {
		if _, err := p.deps.Store.Networks.PutJSON(ctx, net.Name, net); err != nil {
			log.Warnw("failed to persist provisioned state", "error", err)
		}
	}

	log.Infow("network provisioned", "bridge", bridge, "provisioned", true)
}

// deprovisionNetwork tears down the physical infrastructure for a network:
// DHCP relay, bridge IP, and bridge. Only runs on RouterOS backend.
func (p *MicroKubeProvider) deprovisionNetwork(ctx context.Context, net *Network) {
	log := p.deps.Logger.With("network", net.Name)
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return
	}

	if !net.Spec.Provisioned {
		return
	}

	bridge := net.Spec.Bridge
	if bridge == "" {
		return
	}

	// 1. Remove DHCP relay
	if err := rosClient.RemoveDHCPRelayByInterface(ctx, bridge); err != nil {
		log.Warnw("failed to remove DHCP relay", "bridge", bridge, "error", err)
	}

	// 2. Remove bridge IP
	if err := rosClient.RemoveIPAddressByInterface(ctx, bridge); err != nil {
		log.Warnw("failed to remove bridge IP", "bridge", bridge, "error", err)
	}

	// 3. Remove bridge
	if err := rosClient.DeleteBridge(ctx, bridge); err != nil {
		log.Warnw("failed to remove bridge", "bridge", bridge, "error", err)
	}

	log.Infow("network deprovisioned", "bridge", bridge)
}

// cidrMask extracts the mask length from a CIDR string (e.g. "192.168.12.0/24" → "24").
func cidrMask(cidr string) string {
	for i := len(cidr) - 1; i >= 0; i-- {
		if cidr[i] == '/' {
			return cidr[i+1:]
		}
	}
	return "24" // default
}

// natsURL returns the NATS URL that external containers (microdns) can use
// to connect to NATS. When embedded NATS is used, the bind address (0.0.0.0)
// is meaningless outside the mkube container — we need the actual NATS
// container or mkube container IP that's reachable from other containers.
func (p *MicroKubeProvider) natsURL() string {
	port := p.deps.Config.NATS.Port
	if port == 0 {
		port = 4222
	}

	// If a URL is configured and not a bind-all address, use it directly
	url := p.deps.Config.NATS.URL
	if url != "" && !strings.Contains(url, "0.0.0.0") && !strings.Contains(url, "127.0.0.1") {
		return url
	}

	// Look up NATS container IP from tracked pods.
	// SafeMap handles its own locking — no external mutex needed.
	var natsIP string
	p.pods.Range(func(_ string, pod *corev1.Pod) bool {
		if pod.Namespace == "gt" && pod.Name == "nats" {
			if ip := pod.Status.PodIP; ip != "" {
				natsIP = ip
				return false // stop iteration
			}
		}
		return true
	})
	if natsIP != "" {
		return fmt.Sprintf("nats://%s:%d", natsIP, port)
	}

	// Fallback: well-known NATS container IP
	return fmt.Sprintf("nats://192.168.200.10:%d", port)
}

// BridgeExists checks if a bridge exists on the RouterOS device.
func (p *MicroKubeProvider) BridgeExists(ctx context.Context, name string) (bool, error) {
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return false, fmt.Errorf("not a RouterOS backend")
	}
	bridges, err := rosClient.ListBridges(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range bridges {
		if b.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ensureDHCPRelayNAT checks that a srcnat accept rule for UDP 67→67 exists
// before any masquerade rule on the network's bridge. Without this, the
// masquerade rewrites the DHCP relay's source port, causing DHCP responses
// to be dropped by conntrack.
func (p *MicroKubeProvider) ensureDHCPRelayNAT(ctx context.Context, net *Network) {
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return
	}
	if !net.Spec.DHCP.Enabled {
		return
	}

	bridge := net.Spec.Bridge
	if bridge == "" {
		return
	}

	log := p.deps.Logger.With("network", net.Name, "bridge", bridge)

	rules, err := rosClient.ListNatRules(ctx)
	if err != nil {
		log.Warnw("failed to list NAT rules for DHCP relay check", "error", err)
		return
	}

	// Check if our exemption rule already exists
	const relayComment = "mkube: DHCP relay NAT exemption"
	for _, r := range rules {
		if r.Chain == "srcnat" && r.Action == "accept" &&
			r.Protocol == "udp" && r.SrcPort == "67" && r.DstPort == "67" &&
			strings.Contains(r.Comment, "mkube: DHCP relay") {
			return // already exists
		}
	}

	// Find the first masquerade rule on this bridge
	var masqueradeID string
	for _, r := range rules {
		if r.Chain == "srcnat" && r.Action == "masquerade" && r.OutInterface == bridge {
			masqueradeID = r.ID
			break
		}
	}

	if masqueradeID == "" {
		// No masquerade on this bridge — no conflict possible
		return
	}

	// Create the accept rule before the masquerade rule
	rule := map[string]string{
		"chain":        "srcnat",
		"action":       "accept",
		"protocol":     "udp",
		"src-port":     "67",
		"dst-port":     "67",
		"out-interface": bridge,
		"comment":      relayComment,
		"place-before": masqueradeID,
	}
	if err := rosClient.AddNatRule(ctx, rule); err != nil {
		log.Warnw("failed to create DHCP relay NAT exemption", "error", err)
		return
	}

	log.Infow("auto-repaired DHCP relay NAT exemption",
		"placedBefore", masqueradeID)
}
