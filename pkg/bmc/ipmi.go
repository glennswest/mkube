package bmc

import (
	"context"
	"fmt"

	"github.com/bougou/go-ipmi"
)

// ipmiClient wraps bougou/go-ipmi for IPMI LAN+ (RMCP+) communication.
type ipmiClient struct {
	client *ipmi.Client
	creds  Credentials
}

// NewClient creates a new IPMI BMC client using IPMI v2.0 (LANPLUS).
func NewClient(creds Credentials) (BMC, error) {
	port := creds.Port
	if port == 0 {
		port = 623
	}

	c, err := ipmi.NewClient(creds.Address, port, creds.Username, creds.Password)
	if err != nil {
		return nil, fmt.Errorf("creating IPMI client for %s: %w", creds.Address, err)
	}

	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connecting to IPMI at %s:%d: %w", creds.Address, port, err)
	}

	return &ipmiClient{client: c, creds: creds}, nil
}

func (ic *ipmiClient) SetBootDevice(ctx context.Context, dev BootDevice) error {
	var selector ipmi.BootDeviceSelector
	switch dev {
	case BootDevicePXE:
		selector = ipmi.BootDeviceSelectorForcePXE
	case BootDeviceDisk:
		selector = ipmi.BootDeviceSelectorForceHardDrive
	default:
		return fmt.Errorf("unsupported boot device: %v", dev)
	}

	// One-time boot override (persist=false, legacy BIOS boot type).
	// The BIOS will use this device for the next boot only, then revert
	// to the normal boot order.
	err := ic.client.SetBootDevice(ctx, selector, ipmi.BIOSBootTypeLegacy, false)
	if err != nil {
		return fmt.Errorf("set boot device %s on %s: %w", dev, ic.creds.Address, err)
	}
	return nil
}

func (ic *ipmiClient) Power(ctx context.Context, action PowerAction) error {
	var control ipmi.ChassisControl
	switch action {
	case PowerOn:
		control = ipmi.ChassisControlPowerUp
	case PowerOff:
		control = ipmi.ChassisControlPowerDown
	case PowerReset:
		control = ipmi.ChassisControlHardReset
	default:
		return fmt.Errorf("unsupported power action: %v", action)
	}

	_, err := ic.client.ChassisControl(ctx, control)
	if err != nil {
		return fmt.Errorf("chassis control %s on %s: %w", action, ic.creds.Address, err)
	}
	return nil
}

func (ic *ipmiClient) GetPowerStatus(ctx context.Context) (bool, error) {
	resp, err := ic.client.GetChassisStatus(ctx)
	if err != nil {
		return false, fmt.Errorf("get chassis status from %s: %w", ic.creds.Address, err)
	}
	return resp.PowerIsOn, nil
}

func (ic *ipmiClient) Close() error {
	return ic.client.Close(context.Background())
}
