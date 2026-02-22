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

	// Auto-generate DNS recursor ConfigMaps for each network with DNS.
	// Each instance gets forward zones pointing to all peer DNS servers
	// so cross-subnet and external resolution works automatically.
	for _, net := range cfg.Networks {
		if net.DNS.Zone == "" || net.DNS.Server == "" {
			continue
		}

		var fwdZones strings.Builder
		for _, peer := range cfg.Networks {
			if peer.Name == net.Name || peer.DNS.Zone == "" || peer.DNS.Server == "" {
				continue
			}
			fmt.Fprintf(&fwdZones, "    %q = [\"%s:53\"]\n", peer.DNS.Zone, peer.DNS.Server)
		}

		var dhcpSection strings.Builder
		if net.DNS.DHCP.Enabled {
			leaseTime := net.DNS.DHCP.LeaseTime
			if leaseTime == 0 {
				leaseTime = 3600
			}
			fmt.Fprintf(&dhcpSection, "\n[dhcp.v4]\nenabled = true\ninterface = \"eth0\"\n\n")
			fmt.Fprintf(&dhcpSection, "[[dhcp.v4.pools]]\n")
			fmt.Fprintf(&dhcpSection, "range_start = %q\n", net.DNS.DHCP.RangeStart)
			fmt.Fprintf(&dhcpSection, "range_end = %q\n", net.DNS.DHCP.RangeEnd)
			fmt.Fprintf(&dhcpSection, "subnet = %q\n", net.CIDR)
			fmt.Fprintf(&dhcpSection, "gateway = %q\n", net.Gateway)
			fmt.Fprintf(&dhcpSection, "dns = [%q]\n", net.DNS.Server)
			fmt.Fprintf(&dhcpSection, "domain = %q\n", net.DNS.Zone)
			fmt.Fprintf(&dhcpSection, "lease_time_secs = %d\n", leaseTime)
			if net.DNS.DHCP.NextServer != "" {
				fmt.Fprintf(&dhcpSection, "next_server = %q\n", net.DNS.DHCP.NextServer)
			}
			if net.DNS.DHCP.BootFile != "" {
				fmt.Fprintf(&dhcpSection, "boot_file = %q\n", net.DNS.DHCP.BootFile)
			}
			for _, r := range net.DNS.DHCP.Reservations {
				fmt.Fprintf(&dhcpSection, "\n[[dhcp.v4.reservations]]\n")
				fmt.Fprintf(&dhcpSection, "mac = %q\n", r.MAC)
				fmt.Fprintf(&dhcpSection, "ip = %q\n", r.IP)
				if r.Hostname != "" {
					fmt.Fprintf(&dhcpSection, "hostname = %q\n", r.Hostname)
				}
			}
			// Build reverse zone from CIDR: 192.168.11.0/24 -> 11.168.192.in-addr.arpa
			reverseZone := ""
			if cidrParts := strings.Split(net.CIDR, "/"); len(cidrParts) == 2 {
				octets := strings.Split(cidrParts[0], ".")
				if len(octets) == 4 {
					reverseZone = fmt.Sprintf("%s.%s.%s.in-addr.arpa", octets[2], octets[1], octets[0])
				}
			}
			fmt.Fprintf(&dhcpSection, "\n[dhcp.dns_registration]\n")
			fmt.Fprintf(&dhcpSection, "enabled = true\n")
			fmt.Fprintf(&dhcpSection, "forward_zone = %q\n", net.DNS.Zone)
			fmt.Fprintf(&dhcpSection, "reverse_zone_v4 = %q\n", reverseZone)
			fmt.Fprintf(&dhcpSection, "reverse_zone_v6 = \"\"\n")
			fmt.Fprintf(&dhcpSection, "default_ttl = 300\n")
		}

		// TODO: Pass NATS URL to microdns via environment variable on the
		// container (boot-order.yaml) rather than baking it into the TOML.
		// The TOML should only contain DNS/DHCP config. Infrastructure
		// plumbing like NATS URLs must come from the container environment
		// so the config stays portable across different sites/networks.

		toml := fmt.Sprintf(`[instance]
id = "microdns-%s"
mode = "standalone"

[dns.auth]
enabled = true
listen = "0.0.0.0:15353"
zones = ["%s"]

[dns.recursor]
enabled = true
listen = "0.0.0.0:53"

[dns.recursor.forward_zones]
%s
[api.rest]
enabled = true
listen = "0.0.0.0:8080"

[database]
path = "./data/microdns.redb"

[logging]
level = "info"
format = "text"
%s`, net.Name, net.DNS.Zone, fwdZones.String(), dhcpSection.String())

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
