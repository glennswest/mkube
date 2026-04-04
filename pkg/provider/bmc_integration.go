package provider

import (
	"context"
	"time"

	"github.com/glennswest/mkube/pkg/bmc"
	"github.com/glennswest/mkube/pkg/store"
)

const (
	// installImageGracePeriod is how long after lastBoot before the reconcile
	// loop auto-switches an install image to localboot. This gives iPXE time
	// to complete both DHCP rounds and start sanboot before the reservation
	// changes.
	installImageGracePeriod = 2 * time.Minute
)

// initBMCController creates and returns the BMC controller with provider callbacks.
func (p *MicroKubeProvider) initBMCController(s *store.Store) *bmc.Controller {
	if p.deps.Logger == nil {
		return nil
	}
	cb := bmc.Callbacks{
		OnInstallBooted: p.bmcInstallBootedCallback,
		OnPowerChanged:  p.bmcPowerChangedCallback,
	}
	log := p.deps.Logger.Named("bmc")
	return bmc.NewController(s, cb, nil, log)
}

// RunBMCController starts the BMC power controller goroutine.
func (p *MicroKubeProvider) RunBMCController(ctx context.Context) {
	if p.bmcController == nil {
		return
	}
	p.bmcController.Run(ctx)
}

// bmcInstallBootedCallback is called by the BMC controller after an install boot
// completes (DHCP lease detected or timeout). It switches the BMH to localboot
// and re-syncs DHCP so the server boots from disk on next reboot.
func (p *MicroKubeProvider) bmcInstallBootedCallback(ctx context.Context, bmhKey string) {
	log := p.deps.Logger.With("bmh", bmhKey)

	err := p.updateBMHFields(ctx, bmhKey, func(bmh *BareMetalHost) {
		bmh.Spec.Image = "localboot"
	})
	if err != nil {
		log.Errorw("BMC install-booted callback: failed to set localboot", "error", err)
		return
	}

	log.Infow("BMC install-booted: image switched to localboot")

	// Re-sync DHCP reservations so the server gets disk-boot DHCP options
	bmh, ok := p.bareMetalHosts.Get(bmhKey)
	if ok {
		p.syncBMHToNetwork(ctx, bmh, bmh.Spec.Network, bmh.Spec.BMC.Network,
			firstNonEmpty(bmh.Spec.Hostname, bmh.Name), bmh.Spec.IP)
	}
}

// bmcPowerChangedCallback updates status.PoweredOn after a successful IPMI power operation.
func (p *MicroKubeProvider) bmcPowerChangedCallback(ctx context.Context, bmhKey string, poweredOn bool) {
	_ = p.updateBMHFields(ctx, bmhKey, func(bmh *BareMetalHost) {
		bmh.Status.PoweredOn = poweredOn
	})
}

// reconcileInstallImages checks for BMHs stuck on install images and switches
// them to localboot after the grace period. This is the durable fallback for
// the lease-watcher goroutine which doesn't survive mkube restarts.
func (p *MicroKubeProvider) reconcileInstallImages(ctx context.Context) {
	now := time.Now()
	for key, bmh := range p.bareMetalHosts.Snapshot() {
		if bmh.Spec.Online == nil || !*bmh.Spec.Online || !bmh.Status.PoweredOn {
			continue
		}
		if !bmc.IsInstallImage(bmh.Spec.Image) {
			continue
		}
		if bmh.Status.LastBoot == "" {
			continue
		}
		lastBoot, err := time.Parse(time.RFC3339, bmh.Status.LastBoot)
		if err != nil {
			continue
		}
		if now.Sub(lastBoot) < installImageGracePeriod {
			continue
		}

		p.deps.Logger.Infow("reconcile: auto-switching install image to localboot",
			"bmh", bmh.Name, "image", bmh.Spec.Image, "lastBoot", bmh.Status.LastBoot)

		if err := p.updateBMHFields(ctx, key, func(b *BareMetalHost) {
			b.Spec.Image = "localboot"
		}); err != nil {
			p.deps.Logger.Errorw("reconcile: failed to switch to localboot",
				"bmh", bmh.Name, "error", err)
			continue
		}

		// Set IPMI boot device to disk (persistent) so the server boots
		// from disk on every reboot, not just the next one.
		if p.bmcController != nil && bmh.Spec.BMC.Address != "" {
			creds := bmc.Credentials{
				Address:  bmh.Spec.BMC.Address,
				Username: bmh.Spec.BMC.Username,
				Password: bmh.Spec.BMC.Password,
			}
			p.bmcController.SetPersistentDiskBoot(ctx, creds, bmh.Name)
		}

		// Re-sync DHCP so next boot gets localboot config
		updated, ok := p.bareMetalHosts.Get(key)
		if ok {
			p.syncBMHToNetwork(ctx, updated, updated.Spec.Network, updated.Spec.BMC.Network,
				firstNonEmpty(updated.Spec.Hostname, updated.Name), updated.Spec.IP)
		}
	}
}

// enqueueBMCPowerEvent sends a power event to the BMC controller for a BMH
// that has a BMC address configured.
func (p *MicroKubeProvider) enqueueBMCPowerEvent(bmh *BareMetalHost, wasOnline, isOnline bool) {
	if p.bmcController == nil || bmh.Spec.BMC.Address == "" {
		return
	}

	var action bmc.PowerAction
	if isOnline {
		action = bmc.PowerOn
	} else {
		action = bmc.PowerOff
	}

	creds := bmc.Credentials{
		Address:  bmh.Spec.BMC.Address,
		Username: bmh.Spec.BMC.Username,
		Password: bmh.Spec.BMC.Password,
	}

	key := bmh.Namespace + "/" + bmh.Name

	p.bmcController.Enqueue(bmc.PowerEvent{
		BMHKey:    key,
		BMHName:   bmh.Name,
		Creds:     creds,
		BootMAC:   bmh.Spec.BootMACAddress,
		Image:     bmh.Spec.Image,
		Action:    action,
		IsOnline:  isOnline,
		WasOnline: wasOnline,
	})

	p.deps.Logger.Infow("BMC power event enqueued",
		"bmh", bmh.Name,
		"action", action,
		"image", bmh.Spec.Image,
		"bmcAddress", bmh.Spec.BMC.Address,
	)
}

