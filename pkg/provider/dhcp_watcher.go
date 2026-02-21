package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dhcpLease is the JSON structure from microdns DHCP lease API.
type dhcpLease struct {
	IP     string `json:"ip"`
	IPAddr string `json:"ip_addr"`   // microdns field name
	MAC    string `json:"mac"`
	MACAddr string `json:"mac_addr"` // microdns field name
}

func (l dhcpLease) getIP() string {
	if l.IPAddr != "" {
		return l.IPAddr
	}
	return l.IP
}

func (l dhcpLease) getMAC() string {
	if l.MACAddr != "" {
		return l.MACAddr
	}
	return l.MAC
}

// RunDHCPWatcher polls the microdns DHCP lease API and auto-creates BMH objects
// for discovered servers on the g11 network.
func (p *MicroKubeProvider) RunDHCPWatcher(ctx context.Context) {
	cfg := p.deps.Config.BMH
	if cfg.DHCPLeaseURL == "" {
		p.deps.Logger.Info("BMH DHCP watcher disabled (no dhcpLeaseURL configured)")
		return
	}

	interval := time.Duration(cfg.WatchInterval) * time.Second
	if interval < 10*time.Second {
		interval = 30 * time.Second
	}

	log := p.deps.Logger.Named("dhcp-watcher")
	log.Infow("DHCP watcher starting", "url", cfg.DHCPLeaseURL, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("DHCP watcher stopping")
			return
		case <-ticker.C:
			p.reconcileDHCPLeases(ctx)
		}
	}
}

func (p *MicroKubeProvider) reconcileDHCPLeases(ctx context.Context) {
	cfg := p.deps.Config.BMH
	log := p.deps.Logger.Named("dhcp-watcher")

	req, err := http.NewRequestWithContext(ctx, "GET", cfg.DHCPLeaseURL+"/api/v1/leases", nil)
	if err != nil {
		log.Warnw("failed to create lease request", "error", err)
		return
	}

	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		log.Debugw("DHCP lease fetch failed", "error", err)
		return
	}
	defer resp.Body.Close()

	var leases []dhcpLease
	if err := json.NewDecoder(resp.Body).Decode(&leases); err != nil {
		log.Warnw("failed to decode leases", "error", err)
		return
	}

	for _, lease := range leases {
		ip := lease.getIP()
		mac := lease.getMAC()
		if ip == "" || mac == "" {
			continue
		}

		// Only handle 192.168.11.x
		if !strings.HasPrefix(ip, "192.168.11.") {
			continue
		}

		parts := strings.Split(ip, ".")
		if len(parts) != 4 {
			continue
		}
		lastOctet, err := strconv.Atoi(parts[3])
		if err != nil {
			continue
		}

		// port = lastOctet - 9; server1=.10, server8=.17
		port := lastOctet - 9
		if port < 1 || port > 8 {
			continue
		}

		hostname := fmt.Sprintf("server%d", port)
		key := "g11/" + hostname

		if _, exists := p.bareMetalHosts[key]; exists {
			continue
		}

		// The g11 lease is the IPMI interface. Look up the boot MAC from
		// g10 network DHCP reservations in config (NIC-A = primary boot NIC).
		bootMAC := ""
		for _, net := range p.deps.Config.Networks {
			if net.Name != "g10" {
				continue
			}
			for _, r := range net.DNS.DHCP.Reservations {
				if r.Hostname == hostname {
					bootMAC = r.MAC
					break
				}
			}
		}
		if bootMAC == "" {
			bootMAC = mac // fallback to IPMI MAC if no g10 reservation found
		}

		log.Infow("auto-discovered server from DHCP", "name", hostname, "ip", ip, "ipmi_mac", mac, "boot_mac", bootMAC)

		bmh := &BareMetalHost{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"},
			ObjectMeta: metav1.ObjectMeta{
				Name:              hostname,
				Namespace:         "g11",
				CreationTimestamp: metav1.Now(),
			},
			Spec: BMHSpec{
				BootMACAddress: bootMAC,
				Image:          "localboot",
				BMC: BMCDetails{
					Address:  ip,
					Username: "ADMIN",
					Password: "ADMIN",
				},
			},
			Status: BMHStatus{
				Phase: "Discovered",
				IP:    ip,
			},
		}

		p.bareMetalHosts[key] = bmh

		// Persist to NATS
		if p.deps.Store != nil && p.deps.Store.BareMetalHosts != nil {
			storeKey := "g11." + hostname
			if _, err := p.deps.Store.BareMetalHosts.PutJSON(ctx, storeKey, bmh); err != nil {
				log.Warnw("failed to persist discovered BMH", "name", hostname, "error", err)
			}
		}

		// Register in pxemanager (skip if already registered from populate script)
		if err := pxeRegisterHost(ctx, cfg.PXEManagerURL, bootMAC, hostname, "localboot"); err != nil {
			log.Warnw("failed to register discovered host in pxemanager", "name", hostname, "error", err)
		}
	}
}
