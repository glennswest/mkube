package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// baremetalMACs is the response from baremetalservices GET /macs endpoint,
// proxied through pxemanager at GET /api/host/baremetal/get-macs.
type baremetalMACs struct {
	Status string            `json:"status"`
	Data   map[string]string `json:"data"` // interface name → MAC address
}

// pxeAsset represents hardware asset data from pxemanager.
type pxeAsset struct {
	MAC          string `json:"mac"`
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
	Serial       string `json:"serial"`
	CPUModel     string `json:"cpu_model"`
	CPUCores     int    `json:"cpu_cores"`
	MemoryMB     int    `json:"memory_mb"`
}

// RunBMHReconciler runs a periodic reconciliation loop for BareMetalHost objects.
// It detects servers that have booted baremetalservices and auto-discovers
// their network interfaces via pxemanager.
func (p *MicroKubeProvider) RunBMHReconciler(ctx context.Context) {
	cfg := p.deps.Config.BMH
	if cfg.PXEManagerURL == "" {
		p.deps.Logger.Info("BMH reconciler disabled (no pxeManagerURL configured)")
		return
	}

	interval := time.Duration(cfg.WatchInterval) * time.Second
	if interval < 10*time.Second {
		interval = 30 * time.Second
	}

	log := p.deps.Logger.Named("bmh-reconciler")
	log.Infow("BMH reconciler starting", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("BMH reconciler stopping")
			return
		case <-ticker.C:
			p.reconcileBMHStates(ctx)
		}
	}
}

// reconcileBMHStates checks BMH objects in transitional states and
// drives them forward through the provisioning workflow.
//
// Workflow: Discovered → (user sets image=baremetalservices, online=true)
// → Provisioning → (server PXE boots, baremetalservices starts)
// → Ready (boot MAC + hardware info populated)
func (p *MicroKubeProvider) reconcileBMHStates(ctx context.Context) {
	log := p.deps.Logger.Named("bmh-reconciler")
	pxeURL := p.deps.Config.BMH.PXEManagerURL

	for key, bmh := range p.bareMetalHosts {
		if bmh.Status.Phase != "Provisioning" {
			continue
		}
		if !strings.Contains(strings.ToLower(bmh.Spec.Image), "baremetalservices") {
			continue
		}

		// Try to reach baremetalservices on the server via pxemanager proxy
		macs, err := pxeBareMetalGetMACs(ctx, pxeURL, bmh.Name)
		if err != nil {
			log.Debugw("baremetalservices not ready", "host", bmh.Name, "error", err)
			continue
		}

		log.Infow("baremetalservices responding", "host", bmh.Name, "macs", macs.Data)

		// Auto-discover: register interfaces in pxemanager's database.
		// This creates interface records (a, b, c) with hostnames like
		// server1.g10.lo, server1b.g10.lo for DNS registration.
		if err := pxeBareMetalAutoDiscover(ctx, pxeURL, bmh.Name); err != nil {
			log.Warnw("auto-discover failed", "host", bmh.Name, "error", err)
		}

		// Determine boot MAC (primary NIC, prefer eth0)
		bootMAC := pickBootMAC(macs.Data)
		if bootMAC != "" {
			bmh.Spec.BootMACAddress = bootMAC
			log.Infow("boot MAC discovered", "host", bmh.Name, "mac", bootMAC)
		}

		// Store all discovered MACs as annotations
		if bmh.Annotations == nil {
			bmh.Annotations = make(map[string]string)
		}
		for ifName, mac := range macs.Data {
			bmh.Annotations["bmh.mkube.io/mac-"+ifName] = mac
		}

		// Fetch hardware asset data (best effort)
		if bootMAC != "" {
			if asset, err := pxeGetAsset(ctx, pxeURL, bootMAC); err == nil && asset.Manufacturer != "" {
				bmh.Annotations["bmh.mkube.io/manufacturer"] = asset.Manufacturer
				bmh.Annotations["bmh.mkube.io/product"] = asset.Product
				bmh.Annotations["bmh.mkube.io/serial"] = asset.Serial
				if asset.CPUModel != "" {
					bmh.Annotations["bmh.mkube.io/cpu"] = fmt.Sprintf("%s (%d cores)", asset.CPUModel, asset.CPUCores)
				}
				if asset.MemoryMB > 0 {
					bmh.Annotations["bmh.mkube.io/memory"] = fmt.Sprintf("%d MB", asset.MemoryMB)
				}
				log.Infow("asset data collected", "host", bmh.Name,
					"manufacturer", asset.Manufacturer, "product", asset.Product)
			}
		}

		bmh.Status.Phase = "Ready"
		bmh.Status.ErrorMessage = ""
		log.Infow("BMH ready", "host", bmh.Name, "bootMAC", bootMAC,
			"interfaces", len(macs.Data))

		p.persistBMH(ctx, key, bmh)
	}
}

// pickBootMAC selects the boot NIC MAC from baremetalservices MAC data.
// Prefers eth0 (primary NIC-A), falls back to first non-IPMI interface.
func pickBootMAC(macs map[string]string) string {
	if mac, ok := macs["eth0"]; ok {
		return mac
	}
	for name, mac := range macs {
		if name != "ipmi" {
			return mac
		}
	}
	return ""
}

// persistBMH saves a BMH to the NATS store.
func (p *MicroKubeProvider) persistBMH(ctx context.Context, key string, bmh *BareMetalHost) {
	if p.deps.Store == nil || p.deps.Store.BareMetalHosts == nil {
		return
	}
	storeKey := strings.Replace(key, "/", ".", 1)
	if _, err := p.deps.Store.BareMetalHosts.PutJSON(ctx, storeKey, bmh); err != nil {
		p.deps.Logger.Warnw("failed to persist BMH update", "key", key, "error", err)
	}
}

// ─── pxemanager baremetalservices client ─────────────────────────────────────

// pxeBareMetalGetMACs queries baremetalservices on a host through pxemanager's proxy.
// Returns the MAC addresses of all network interfaces on the physical server.
func pxeBareMetalGetMACs(ctx context.Context, pxeURL, hostname string) (*baremetalMACs, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/host/baremetal/get-macs?host=%s", pxeURL, hostname), nil)
	if err != nil {
		return nil, err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get-macs: HTTP %d", resp.StatusCode)
	}
	var macs baremetalMACs
	if err := json.NewDecoder(resp.Body).Decode(&macs); err != nil {
		return nil, fmt.Errorf("decoding get-macs: %w", err)
	}
	return &macs, nil
}

// pxeBareMetalAutoDiscover calls baremetalservices on a host to collect
// its MAC addresses, then registers all interfaces in pxemanager's database.
// Interfaces are named a, b, c and get hostnames like server1.g10.lo,
// server1b.g10.lo for automatic DNS registration.
func pxeBareMetalAutoDiscover(ctx context.Context, pxeURL, hostname string) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/host/baremetal/auto-discover?host=%s", pxeURL, hostname), nil)
	if err != nil {
		return err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("auto-discover: HTTP %d", resp.StatusCode)
	}
	return nil
}

// pxeBareMetalResetIPMI resets IPMI configuration on a host through
// baremetalservices, proxied by pxemanager.
func pxeBareMetalResetIPMI(ctx context.Context, pxeURL, hostname string) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/host/baremetal/reset-ipmi?host=%s", pxeURL, hostname), nil)
	if err != nil {
		return err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("reset-ipmi: HTTP %d", resp.StatusCode)
	}
	return nil
}

// pxeGetAsset fetches hardware asset data for a host from pxemanager.
func pxeGetAsset(ctx context.Context, pxeURL, mac string) (*pxeAsset, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/asset?mac=%s", pxeURL, mac), nil)
	if err != nil {
		return nil, err
	}
	resp, err := pxeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get asset: HTTP %d", resp.StatusCode)
	}
	var asset pxeAsset
	if err := json.NewDecoder(resp.Body).Decode(&asset); err != nil {
		return nil, fmt.Errorf("decoding asset: %w", err)
	}
	return &asset, nil
}
