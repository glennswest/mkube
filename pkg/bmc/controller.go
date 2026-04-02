package bmc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/store"
)

const (
	// dhcpLeaseTimeout is how long to wait for a DHCP lease event
	// before falling back to setting bootdev disk anyway.
	dhcpLeaseTimeout = 3 * time.Minute
)

// PowerEvent is sent to the controller to request a power state change.
type PowerEvent struct {
	BMHKey  string      // "namespace/name"
	BMHName string      // just the name
	Creds   Credentials // IPMI credentials
	BootMAC string      // boot MAC address (for DHCP lease matching)
	Image   string      // spec.image value
	Action  PowerAction // On, Off, Reset
	IsOnline  bool      // desired spec.online state
	WasOnline bool      // previous spec.online state
}

// Callbacks allows the controller to update BMH state back in the provider.
type Callbacks struct {
	// OnInstallBooted is called after install PXE boot + DHCP lease detected.
	// It should set spec.image = "localboot" and re-sync DHCP.
	OnInstallBooted func(ctx context.Context, bmhKey string)

	// OnPowerChanged is called after a successful power operation.
	// poweredOn reflects the new physical state.
	OnPowerChanged func(ctx context.Context, bmhKey string, poweredOn bool)
}

// Controller processes power events for bare metal hosts via IPMI.
type Controller struct {
	events    chan PowerEvent
	store     *store.Store
	callbacks Callbacks
	newClient func(Credentials) (BMC, error)
	log       *zap.SugaredLogger

	// activeLeaseWatchers tracks BMH keys with running lease-watch goroutines
	// to avoid duplicates if multiple power-on events arrive in quick succession.
	watcherMu sync.Mutex
	watchers  map[string]context.CancelFunc
}

// NewController creates a BMC power controller.
// If newClient is nil, the real IPMI client constructor is used.
func NewController(s *store.Store, cb Callbacks, newClient func(Credentials) (BMC, error), log *zap.SugaredLogger) *Controller {
	if newClient == nil {
		newClient = func(creds Credentials) (BMC, error) {
			return NewClient(creds)
		}
	}
	return &Controller{
		events:    make(chan PowerEvent, 32),
		store:     s,
		callbacks: cb,
		newClient: newClient,
		log:       log,
		watchers:  make(map[string]context.CancelFunc),
	}
}

// Enqueue sends a power event to the controller for processing.
// Non-blocking: drops the event if the channel is full (logged as warning).
func (c *Controller) Enqueue(evt PowerEvent) {
	select {
	case c.events <- evt:
	default:
		c.log.Warnw("BMC event queue full, dropping event",
			"bmh", evt.BMHName, "action", evt.Action)
	}
}

// Run processes power events until ctx is cancelled. Should be started as a goroutine.
func (c *Controller) Run(ctx context.Context) {
	c.log.Info("BMC controller started")
	for {
		select {
		case <-ctx.Done():
			c.log.Info("BMC controller stopping")
			// Cancel any active lease watchers
			c.watcherMu.Lock()
			for _, cancel := range c.watchers {
				cancel()
			}
			c.watcherMu.Unlock()
			return
		case evt := <-c.events:
			c.handleEvent(ctx, evt)
		}
	}
}

// SetStore updates the NATS store reference (for deferred NATS connection).
func (c *Controller) SetStore(s *store.Store) {
	c.store = s
}

func (c *Controller) handleEvent(ctx context.Context, evt PowerEvent) {
	log := c.log.With("bmh", evt.BMHName, "action", evt.Action, "image", evt.Image)

	if evt.Creds.Address == "" {
		log.Debug("no BMC address configured, skipping IPMI")
		return
	}

	client, err := c.newClient(evt.Creds)
	if err != nil {
		log.Errorw("failed to connect to BMC", "address", evt.Creds.Address, "error", err)
		return
	}
	defer client.Close()

	switch {
	case evt.Action == PowerOff || !evt.IsOnline:
		c.handlePowerOff(ctx, client, evt, log)
	case IsInstallImage(evt.Image):
		c.handleInstallBoot(ctx, client, evt, log)
	default:
		c.handleNormalPowerOn(ctx, client, evt, log)
	}
}

// handlePowerOff sends a chassis power-off command.
func (c *Controller) handlePowerOff(ctx context.Context, client BMC, evt PowerEvent, log *zap.SugaredLogger) {
	if err := client.Power(ctx, PowerOff); err != nil {
		log.Errorw("IPMI power off failed", "error", err)
		return
	}
	log.Infow("IPMI power off sent")
	if c.callbacks.OnPowerChanged != nil {
		c.callbacks.OnPowerChanged(ctx, evt.BMHKey, false)
	}
}

// handleNormalPowerOn powers on (or resets if already on) without boot device override.
func (c *Controller) handleNormalPowerOn(ctx context.Context, client BMC, evt PowerEvent, log *zap.SugaredLogger) {
	powered, err := client.GetPowerStatus(ctx)
	if err != nil {
		log.Warnw("IPMI get power status failed, attempting power on anyway", "error", err)
		powered = false
	}

	if powered {
		if err := client.Power(ctx, PowerReset); err != nil {
			log.Errorw("IPMI power reset failed", "error", err)
			return
		}
		log.Infow("IPMI power reset sent (server was already on)")
	} else {
		if err := client.Power(ctx, PowerOn); err != nil {
			log.Errorw("IPMI power on failed", "error", err)
			return
		}
		log.Infow("IPMI power on sent")
	}

	if c.callbacks.OnPowerChanged != nil {
		c.callbacks.OnPowerChanged(ctx, evt.BMHKey, true)
	}
}

// handleInstallBoot sets PXE boot, powers on, then watches for DHCP lease
// to switch to disk boot after the installer has started.
func (c *Controller) handleInstallBoot(ctx context.Context, client BMC, evt PowerEvent, log *zap.SugaredLogger) {
	// Step 1: Set boot device to PXE
	if err := client.SetBootDevice(ctx, BootDevicePXE); err != nil {
		log.Errorw("IPMI set boot device PXE failed", "error", err)
		return
	}
	log.Infow("IPMI boot device set to PXE")

	// Step 2: Power on (or reset if already on)
	powered, err := client.GetPowerStatus(ctx)
	if err != nil {
		log.Warnw("IPMI get power status failed, attempting power on", "error", err)
		powered = false
	}

	if powered {
		if err := client.Power(ctx, PowerReset); err != nil {
			log.Errorw("IPMI power reset failed", "error", err)
			return
		}
		log.Infow("IPMI power reset sent for install boot")
	} else {
		if err := client.Power(ctx, PowerOn); err != nil {
			log.Errorw("IPMI power on failed", "error", err)
			return
		}
		log.Infow("IPMI power on sent for install boot")
	}

	if c.callbacks.OnPowerChanged != nil {
		c.callbacks.OnPowerChanged(ctx, evt.BMHKey, true)
	}

	// Step 3: Start DHCP lease watcher to detect when server has POSTed
	// and consumed the PXE boot flag, then set boot device to disk.
	c.startLeaseWatcher(ctx, evt, log)
}

// startLeaseWatcher subscribes to DHCP lease events and waits for the BMH's
// boot MAC to appear. Once seen (or after timeout), it sets bootdev disk and
// calls the OnInstallBooted callback.
func (c *Controller) startLeaseWatcher(ctx context.Context, evt PowerEvent, log *zap.SugaredLogger) {
	if evt.BootMAC == "" {
		log.Warn("no boot MAC address, cannot watch for DHCP lease — using timeout fallback")
	}

	// Avoid duplicate watchers for the same BMH
	c.watcherMu.Lock()
	if cancel, exists := c.watchers[evt.BMHKey]; exists {
		cancel()
	}
	watchCtx, cancel := context.WithCancel(ctx)
	c.watchers[evt.BMHKey] = cancel
	c.watcherMu.Unlock()

	go func() {
		defer func() {
			c.watcherMu.Lock()
			delete(c.watchers, evt.BMHKey)
			c.watcherMu.Unlock()
		}()

		c.waitForLeaseAndSetDisk(watchCtx, evt, log)
	}()
}

// waitForLeaseAndSetDisk blocks until the BMH's boot MAC gets a DHCP lease,
// then sets the boot device to disk and calls the install-booted callback.
func (c *Controller) waitForLeaseAndSetDisk(ctx context.Context, evt PowerEvent, log *zap.SugaredLogger) {
	leaseDetected := make(chan struct{}, 1)

	// Subscribe to DHCP lease events if we have a store and a boot MAC
	var sub *nats.Subscription
	if c.store != nil && evt.BootMAC != "" {
		normalizedMAC := strings.ToUpper(evt.BootMAC)
		var err error
		sub, err = c.store.Subscribe("mkube.dhcp.*.lease", func(msg *nats.Msg) {
			var dhcpEvt struct {
				Type    string `json:"type"`
				MACAddr string `json:"mac_addr"`
			}
			if err := json.Unmarshal(msg.Data, &dhcpEvt); err != nil {
				return
			}
			if dhcpEvt.Type == "LeaseCreated" && strings.EqualFold(dhcpEvt.MACAddr, normalizedMAC) {
				select {
				case leaseDetected <- struct{}{}:
				default:
				}
			}
		})
		if err != nil {
			log.Warnw("failed to subscribe to DHCP events for lease watch", "error", err)
		}
	}

	// Wait for lease or timeout
	timer := time.NewTimer(dhcpLeaseTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		if sub != nil {
			_ = sub.Unsubscribe()
		}
		return
	case <-leaseDetected:
		log.Infow("DHCP lease detected for boot MAC, setting boot device to disk",
			"mac", evt.BootMAC)
	case <-timer.C:
		log.Warnw("DHCP lease timeout, setting boot device to disk anyway",
			"mac", evt.BootMAC, "timeout", dhcpLeaseTimeout)
	}

	if sub != nil {
		_ = sub.Unsubscribe()
	}

	// Set boot device to disk for next reboot
	client, err := c.newClient(evt.Creds)
	if err != nil {
		log.Errorw("failed to reconnect to BMC for bootdev disk", "error", err)
		return
	}
	defer client.Close()

	if err := client.SetBootDevice(ctx, BootDeviceDisk); err != nil {
		log.Errorw("IPMI set boot device disk failed", "error", err)
		return
	}
	log.Infow("IPMI boot device set to Disk (post-install)")

	// Notify provider to switch image to localboot
	if c.callbacks.OnInstallBooted != nil {
		c.callbacks.OnInstallBooted(ctx, evt.BMHKey)
	}
}
