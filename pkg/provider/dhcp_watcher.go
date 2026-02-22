package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dhcpLease is the JSON structure from microdns DHCP lease API.
type dhcpLease struct {
	IP      string `json:"ip"`
	IPAddr  string `json:"ip_addr"`  // microdns field name
	MAC     string `json:"mac"`
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

// microdnsLeaseEvent is the JSON structure published by microdns to NATS.
type microdnsLeaseEvent struct {
	Type       string `json:"type"`        // "LeaseCreated" or "LeaseReleased"
	InstanceID string `json:"instance_id"`
	IPAddr     string `json:"ip_addr"`
	MACAddr    string `json:"mac_addr"`
	Hostname   string `json:"hostname"`
	PoolID     string `json:"pool_id"`
	Timestamp  string `json:"timestamp"`
}

// RunDHCPWatcher subscribes to NATS lease events for instant discovery and
// polls the microdns DHCP lease API as a fallback consistency check.
func (p *MicroKubeProvider) RunDHCPWatcher(ctx context.Context) {
	cfg := p.deps.Config.BMH
	if cfg.DHCPLeaseURL == "" {
		p.deps.Logger.Info("BMH DHCP watcher disabled (no dhcpLeaseURL configured)")
		return
	}

	log := p.deps.Logger.Named("dhcp-watcher")

	// Start NATS subscription for instant lease events
	p.startDHCPSubscription(ctx)

	// Fallback poll interval — 5 minutes for consistency checks
	interval := 5 * time.Minute
	log.Infow("DHCP watcher starting", "url", cfg.DHCPLeaseURL, "poll_interval", interval)

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

// startDHCPSubscription subscribes to microdns NATS lease events.
// Safe to call when store is nil (no-op); will be retried from SetStore.
func (p *MicroKubeProvider) startDHCPSubscription(ctx context.Context) {
	if p.deps.Store == nil {
		return
	}

	log := p.deps.Logger.Named("dhcp-watcher")

	_, err := p.deps.Store.Subscribe("microdns.*.leases", func(msg *nats.Msg) {
		var evt microdnsLeaseEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			log.Warnw("failed to unmarshal lease event", "error", err)
			return
		}

		if evt.Type != "LeaseCreated" {
			return
		}

		log.Infow("NATS lease event received",
			"type", evt.Type,
			"ip", evt.IPAddr,
			"mac", evt.MACAddr,
			"hostname", evt.Hostname,
		)

		p.processDHCPLease(ctx, evt.IPAddr, evt.MACAddr)
	})
	if err != nil {
		log.Warnw("failed to subscribe to DHCP lease events", "error", err)
		return
	}

	log.Info("DHCP NATS subscription started on microdns.*.leases")
}

// processDHCPLease handles a single DHCP lease, creating a BMH if the IP
// is on the 192.168.11.x discovery network. Called by both NATS events and
// the polling reconciler.
func (p *MicroKubeProvider) processDHCPLease(ctx context.Context, ip, mac string) {
	cfg := p.deps.Config.BMH
	log := p.deps.Logger.Named("dhcp-watcher")

	if ip == "" || mac == "" {
		return
	}

	// Only handle 192.168.11.x
	if !strings.HasPrefix(ip, "192.168.11.") {
		return
	}

	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return
	}
	lastOctet, err := strconv.Atoi(parts[3])
	if err != nil {
		return
	}

	// port = lastOctet - 9; server1=.10, server8=.17
	port := lastOctet - 9
	if port < 1 || port > 8 {
		return
	}

	hostname := fmt.Sprintf("server%d", port)
	key := "g11/" + hostname

	if _, exists := p.bareMetalHosts[key]; exists {
		return
	}

	log.Infow("auto-discovered server from DHCP", "name", hostname, "ip", ip, "ipmi_mac", mac)

	bmh := &BareMetalHost{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "BareMetalHost"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              hostname,
			Namespace:         "g11",
			CreationTimestamp: metav1.Now(),
		},
		Spec: BMHSpec{
			Image: "localboot",
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

	// Configure IPMI in pxemanager
	if err := pxeConfigureIPMI(ctx, cfg.PXEManagerURL, hostname, ip, "ADMIN", "ADMIN"); err != nil {
		log.Warnw("failed to configure IPMI for discovered host", "name", hostname, "error", err)
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
		p.processDHCPLease(ctx, lease.getIP(), lease.getMAC())
	}
}
