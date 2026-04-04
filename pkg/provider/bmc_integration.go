package provider

import (
	"context"

	"github.com/glennswest/mkube/pkg/bmc"
	"github.com/glennswest/mkube/pkg/store"
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

