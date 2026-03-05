package provider

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/config"
)

// generateDefaultConfigMaps creates built-in ConfigMaps derived from the
// running mkube configuration. These are loaded at startup and can be
// overridden by user-supplied ConfigMaps from the boot manifest.
// DNS ConfigMaps now contain only minimal structural TOML — DHCP pools,
// reservations, and forward zones are seeded via microdns REST API.
func generateDefaultConfigMaps(cfg *config.Config) []*corev1.ConfigMap {
	gateway := cfg.DefaultNetwork().Gateway

	// Derive mkube container IP: gateway .1 → mkube .2 (deploy convention)
	mkubeIP := gateway
	if parts := strings.Split(gateway, "."); len(parts) == 4 {
		if n, err := strconv.Atoi(parts[3]); err == nil {
			parts[3] = strconv.Itoa(n + 1)
			mkubeIP = strings.Join(parts, ".")
		}
	}

	consoleConfig := fmt.Sprintf(`cluster_name: %s
listen_port: 9090

mkube:
  base_url: "http://%s:8082"

registry:
  base_url: "http://%s:5000"
`, cfg.NodeName, mkubeIP, mkubeIP)

	cms := []*corev1.ConfigMap{
		{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mkube-console-config",
				Namespace: "infra",
			},
			Data: map[string]string{
				"config.yaml": consoleConfig,
			},
		},
	}

	// Auto-generate minimal DNS ConfigMaps for each network with DNS.
	// These contain only structural config (instance, auth, recursor, API, database).
	// DHCP pools/reservations and forward zones are seeded via REST API.
	for _, net := range cfg.Networks {
		if net.DNS.Zone == "" || net.DNS.Server == "" {
			continue
		}

		// RouterOS containers use "gateway" mode (DHCP via relay, no raw sockets).
		dnsMode := "standalone"
		if cfg.Backend == "" || cfg.Backend == "routeros" {
			dnsMode = "gateway"
		}

		// Determine if DHCP listener is needed
		hasDHCP := net.DNS.DHCP.Enabled && net.DNS.DHCP.ServerNetwork == ""
		if !hasDHCP {
			for _, peer := range cfg.Networks {
				if peer.Name != net.Name && peer.DNS.DHCP.Enabled && peer.DNS.DHCP.ServerNetwork == net.Name {
					hasDHCP = true
					break
				}
			}
		}

		var dhcpSection string
		if hasDHCP {
			reverseZone := ""
			if cidrParts := strings.Split(net.CIDR, "/"); len(cidrParts) == 2 {
				octets := strings.Split(cidrParts[0], ".")
				if len(octets) == 4 {
					reverseZone = fmt.Sprintf("%s.%s.%s.in-addr.arpa", octets[2], octets[1], octets[0])
				}
			}
			dhcpSection = fmt.Sprintf(`
[dhcp.v4]
enabled = true
interface = "eth0"
server_ip = %q
listen_ports = [67]

[dhcp.dns_registration]
enabled = true
forward_zone = %q
reverse_zone_v4 = %q
reverse_zone_v6 = ""
default_ttl = 300
`, net.DNS.Server, net.DNS.Zone, reverseZone)
		}

		toml := fmt.Sprintf(`[instance]
id = "microdns-%s"
mode = "%s"

[dns.auth]
enabled = true
listen = "0.0.0.0:15353"
zones = ["%s"]

[dns.recursor]
enabled = true
listen = "0.0.0.0:53"

[dns.recursor.forward_zones]

[api.rest]
enabled = true
listen = "0.0.0.0:8080"

[database]
path = "./data/microdns.redb"

[logging]
level = "info"
format = "text"
%s`, net.Name, dnsMode, net.DNS.Zone, dhcpSection)

		cms = append(cms, &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dns-config",
				Namespace: net.Name,
			},
			Data: map[string]string{
				"microdns.toml": toml,
			},
		})
	}

	return cms
}

// networkHasDHCP returns true if the named network serves DHCP — either
// locally (DHCP enabled with no serverNetwork) or because another network
// targets it via serverNetwork. Checks both config.yaml networks and
// Network CRDs.
func (p *MicroKubeProvider) networkHasDHCP(name string) bool {
	// Check config.yaml networks
	for _, net := range p.deps.Config.Networks {
		if !net.DNS.DHCP.Enabled {
			continue
		}
		if net.Name == name && net.DNS.DHCP.ServerNetwork == "" {
			return true
		}
		if net.DNS.DHCP.ServerNetwork == name {
			return true
		}
	}
	// Check Network CRDs
	for _, net := range p.networks {
		if !net.Spec.DHCP.Enabled {
			continue
		}
		if net.Name == name && net.Spec.DHCP.ServerNetwork == "" {
			return true
		}
		if net.Spec.DHCP.ServerNetwork == name {
			return true
		}
	}
	return false
}
