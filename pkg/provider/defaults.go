package provider

import (
	"fmt"
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
	cms := []*corev1.ConfigMap{}

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

		// NATS messaging section for DHCP event pipeline
		// Embedded NATS sets URL to nats://0.0.0.0:4222 — not reachable from
		// external containers. Always use the NATS container IP.
		natsPort := cfg.NATS.Port
		if natsPort == 0 {
			natsPort = 4222
		}
		natsURL := cfg.NATS.URL
		if natsURL == "" || strings.Contains(natsURL, "0.0.0.0") || strings.Contains(natsURL, "127.0.0.1") {
			natsURL = fmt.Sprintf("nats://192.168.200.10:%d", natsPort)
		}
		messagingSection := fmt.Sprintf(`
[messaging]
backend = "nats"
topic_prefix = "microdns"
url = %q
`, natsURL)

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
path = "/data/microdns.redb"

[logging]
level = "info"
format = "text"
%s%s`, net.Name, dnsMode, net.DNS.Zone, dhcpSection, messagingSection)

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
	// Check Network CRDs — SafeMap handles its own locking.
	for _, net := range p.networks.Values() {
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

// networkHasDHCPFromSnapshot is the lock-free variant of networkHasDHCP for
// use in background goroutines that already hold a networks snapshot.
func (p *MicroKubeProvider) networkHasDHCPFromSnapshot(name string, netsSnap []*Network) bool {
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
	for _, net := range netsSnap {
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
