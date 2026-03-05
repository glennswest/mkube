package provider

import (
	"context"
	"fmt"

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

// natsURL returns the NATS URL for microdns messaging.
// Prefers the configured URL; falls back to embedded NATS on the NATS container IP.
func (p *MicroKubeProvider) natsURL() string {
	if p.deps.Config.NATS.URL != "" {
		return p.deps.Config.NATS.URL
	}
	// For embedded NATS, build URL from known NATS container IP
	port := p.deps.Config.NATS.Port
	if port == 0 {
		port = 4222
	}
	// Embedded NATS runs on the same host; NATS container is at 192.168.200.10
	// but microdns containers need to reach it at the NATS container IP.
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
