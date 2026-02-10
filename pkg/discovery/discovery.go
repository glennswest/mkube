package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/dns"
	"github.com/glenneth/microkube/pkg/lifecycle"
	"github.com/glenneth/microkube/pkg/routeros"
)

// Container represents a discovered container with its full network context.
type Container struct {
	Name      string
	Status    string
	IP        string // without CIDR mask
	CIDR      string // with mask, e.g. "192.168.10.5/24"
	Bridge    string
	Gateway   string
	Interface string // veth name
	RootDir   string
	Image     string // tarball path
	Hostname  string
	DNS       string
	StartBoot bool
	IsDNS     bool     // true if this container is a MicroDNS instance
	DNSZones  []string // zone names if IsDNS
}

// Network represents a discovered network (bridge + subnet).
type Network struct {
	Bridge  string
	CIDR    string // e.g. "192.168.10.0/24"
	Gateway string // bridge IP
}

// Inventory holds the complete discovered state of the RouterOS device.
type Inventory struct {
	Containers []Container
	Networks   []Network
	System     *routeros.SystemResource
}

// Discover queries a RouterOS device and builds a full inventory of containers,
// networks, and system info. It probes running containers for MicroDNS API.
func Discover(ctx context.Context, ros *routeros.Client, log *zap.SugaredLogger) (*Inventory, error) {
	log.Info("starting device discovery")

	// Query RouterOS for all infrastructure data in sequence
	// (REST API doesn't handle parallel well on some devices)
	containers, err := ros.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	veths, err := ros.ListVeths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing veths: %w", err)
	}

	bridgePorts, err := ros.ListBridgePorts(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing bridge ports: %w", err)
	}

	ipAddrs, err := ros.ListIPAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing IP addresses: %w", err)
	}

	sysResource, err := ros.GetSystemResource(ctx)
	if err != nil {
		log.Warnw("failed to get system resource", "error", err)
	}

	// Build lookup maps
	vethByName := make(map[string]routeros.NetworkInterface, len(veths))
	for _, v := range veths {
		vethByName[v.Name] = v
	}

	vethToBridge := make(map[string]string, len(bridgePorts))
	for _, bp := range bridgePorts {
		vethToBridge[bp.Interface] = bp.Bridge
	}

	// Build bridge gateway map from IP addresses on bridges
	bridgeGateway := make(map[string]string)
	for _, addr := range ipAddrs {
		// IP addresses on bridges are typically the gateway
		if _, exists := bridgeGateway[addr.Interface]; !exists {
			ip, _, err := net.ParseCIDR(addr.Address)
			if err == nil {
				bridgeGateway[addr.Interface] = ip.String()
			}
		}
	}

	// Discover networks from bridge IPs
	networkMap := make(map[string]*Network)
	for _, addr := range ipAddrs {
		_, ipNet, err := net.ParseCIDR(addr.Address)
		if err != nil {
			continue
		}
		key := ipNet.String()
		if _, exists := networkMap[key]; !exists {
			ip, _, _ := net.ParseCIDR(addr.Address)
			networkMap[key] = &Network{
				Bridge:  addr.Interface,
				CIDR:    key,
				Gateway: ip.String(),
			}
		}
	}

	// Build discovered containers
	var discovered []Container
	for _, ct := range containers {
		status := "stopped"
		if ct.IsRunning() {
			status = "running"
		}
		dc := Container{
			Name:      ct.Name,
			Status:    status,
			Interface: ct.Interface,
			RootDir:   ct.RootDir,
			Image:     ct.Image,
			Hostname:  ct.Hostname,
			DNS:       ct.DNS,
			StartBoot: ct.StartOnBoot == "true",
		}

		// Map container → veth → IP/bridge
		if v, ok := vethByName[ct.Interface]; ok {
			dc.CIDR = v.Address
			dc.Gateway = v.Gateway
			if v.Address != "" {
				ip, _, err := net.ParseCIDR(v.Address)
				if err == nil {
					dc.IP = ip.String()
				} else {
					dc.IP = v.Address
				}
			}
		}
		if bridge, ok := vethToBridge[ct.Interface]; ok {
			dc.Bridge = bridge
		}

		discovered = append(discovered, dc)
	}

	// Probe running containers for MicroDNS API
	probe := &http.Client{Timeout: 3 * time.Second}
	for i := range discovered {
		if discovered[i].Status != "running" || discovered[i].IP == "" {
			continue
		}
		zones := probeMicroDNS(ctx, probe, discovered[i].IP)
		if len(zones) > 0 {
			discovered[i].IsDNS = true
			for _, z := range zones {
				discovered[i].DNSZones = append(discovered[i].DNSZones, z.Name)
			}
			log.Infow("found MicroDNS instance",
				"container", discovered[i].Name,
				"ip", discovered[i].IP,
				"zones", discovered[i].DNSZones,
				"bridge", discovered[i].Bridge,
			)
		}
	}

	var networks []Network
	for _, n := range networkMap {
		networks = append(networks, *n)
	}

	inv := &Inventory{
		Containers: discovered,
		Networks:   networks,
		System:     sysResource,
	}

	log.Infow("discovery complete",
		"containers", len(discovered),
		"networks", len(networks),
	)
	return inv, nil
}

// probeMicroDNS tries to reach the MicroDNS API at ip:8080/api/v1/zones.
func probeMicroDNS(ctx context.Context, client *http.Client, ip string) []dns.Zone {
	url := fmt.Sprintf("http://%s:8080/api/v1/zones", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var zones []dns.Zone
	if err := json.NewDecoder(resp.Body).Decode(&zones); err != nil {
		return nil
	}
	return zones
}

// EnrichNetworks updates network definitions with discovered inventory.
// - Fills in missing DNS config for networks that have a MicroDNS container
// - Creates new NetworkDef entries for discovered networks not in config
// - Skips the microkube container itself
func EnrichNetworks(networks []config.NetworkDef, inv *Inventory, selfName string, log *zap.SugaredLogger) []config.NetworkDef {
	result := make([]config.NetworkDef, len(networks))
	copy(result, networks)

	// Index existing networks by CIDR for matching
	cidrToIdx := make(map[string]int)
	for i, n := range result {
		_, ipNet, err := net.ParseCIDR(n.CIDR)
		if err == nil {
			cidrToIdx[ipNet.String()] = i
		}
	}

	// Process DNS containers
	for _, ct := range inv.Containers {
		if !ct.IsDNS || ct.Name == selfName {
			continue
		}

		// Determine subnet from container CIDR
		subnet := ""
		if ct.CIDR != "" {
			_, ipNet, err := net.ParseCIDR(ct.CIDR)
			if err == nil {
				subnet = ipNet.String()
			}
		}
		if subnet == "" {
			continue
		}

		endpoint := fmt.Sprintf("http://%s:8080", ct.IP)
		zone := ""
		if len(ct.DNSZones) > 0 {
			zone = ct.DNSZones[0]
		}

		if idx, ok := cidrToIdx[subnet]; ok {
			// Existing network — fill in DNS if empty
			if result[idx].DNS.Endpoint == "" {
				result[idx].DNS.Endpoint = endpoint
				result[idx].DNS.Server = ct.IP
				result[idx].DNS.Zone = zone
				log.Infow("auto-configured DNS for network",
					"network", result[idx].Name,
					"dns_ip", ct.IP,
					"zone", zone,
				)
			}
		} else {
			// New network — create a NetworkDef
			name := deriveName(ct, zone)
			gateway := ct.Gateway
			if gateway == "" {
				// Try to find bridge gateway from inventory
				for _, n := range inv.Networks {
					if n.CIDR == subnet {
						gateway = n.Gateway
						break
					}
				}
			}

			nd := config.NetworkDef{
				Name:    name,
				Bridge:  ct.Bridge,
				CIDR:    subnet,
				Gateway: gateway,
				DNS: config.DNSConfig{
					Endpoint: endpoint,
					Zone:     zone,
					Server:   ct.IP,
				},
			}
			log.Infow("discovered new network",
				"name", name,
				"bridge", nd.Bridge,
				"cidr", subnet,
				"gateway", gateway,
				"dns", ct.IP,
				"zone", zone,
			)
			result = append(result, nd)
			cidrToIdx[subnet] = len(result) - 1
		}
	}

	// Also add networks that have no DNS but were discovered
	for _, net := range inv.Networks {
		if _, exists := cidrToIdx[net.CIDR]; !exists {
			name := net.Bridge
			if name == "" {
				name = fmt.Sprintf("net-%s", strings.ReplaceAll(net.CIDR, "/", "-"))
			}
			nd := config.NetworkDef{
				Name:    name,
				Bridge:  net.Bridge,
				CIDR:    net.CIDR,
				Gateway: net.Gateway,
			}
			log.Infow("discovered network (no DNS)",
				"name", name,
				"bridge", net.Bridge,
				"cidr", net.CIDR,
			)
			result = append(result, nd)
			cidrToIdx[net.CIDR] = len(result) - 1
		}
	}

	return result
}

// InferProbes generates default probes from a discovered container's service info.
// MicroDNS instances get an HTTP liveness probe; other containers get no auto-probes.
func InferProbes(ct Container) *lifecycle.ProbeSet {
	if ct.IsDNS && ct.IP != "" {
		return &lifecycle.ProbeSet{
			Liveness: &lifecycle.ProbeConfig{
				Type:             "http",
				Path:             "/api/v1/zones",
				Port:             8080,
				PeriodSeconds:    30,
				TimeoutSeconds:   3,
				FailureThreshold: 3,
				SuccessThreshold: 1,
			},
			Startup: &lifecycle.ProbeConfig{
				Type:                "http",
				Path:                "/api/v1/zones",
				Port:                8080,
				InitialDelaySeconds: 5,
				PeriodSeconds:       3,
				TimeoutSeconds:      3,
				FailureThreshold:    10,
				SuccessThreshold:    1,
			},
		}
	}
	return nil
}

// BuildLifecycleUnits converts discovered containers into lifecycle.ContainerUnit
// entries for keepalive and auto-probe registration.
func BuildLifecycleUnits(inv *Inventory) []lifecycle.ContainerUnit {
	var units []lifecycle.ContainerUnit
	for _, ct := range inv.Containers {
		if !ct.StartBoot {
			continue
		}
		unit := lifecycle.ContainerUnit{
			Name:          ct.Name,
			ContainerIP:   ct.IP,
			RestartPolicy: "Always",
			StartOnBoot:   true,
			Managed:       false,
			Probes:        InferProbes(ct),
		}
		units = append(units, unit)
	}
	return units
}

// deriveName picks a network name from zone or bridge info.
func deriveName(ct Container, zone string) string {
	if zone != "" {
		// Use zone name without TLD: "gt.lo" → "gt", "g10.lo" → "g10"
		name := strings.TrimSuffix(zone, ".lo")
		name = strings.TrimSuffix(name, ".")
		if name != "" {
			return name
		}
	}
	if ct.Bridge != "" {
		return ct.Bridge
	}
	return fmt.Sprintf("net-%s", ct.IP)
}
