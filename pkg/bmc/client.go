package bmc

import (
	"context"
	"strings"
)

// BootDevice represents a BIOS boot device selector.
type BootDevice int

const (
	BootDevicePXE  BootDevice = iota // Network/PXE boot
	BootDeviceDisk                   // Hard drive boot
)

func (d BootDevice) String() string {
	switch d {
	case BootDevicePXE:
		return "PXE"
	case BootDeviceDisk:
		return "Disk"
	default:
		return "Unknown"
	}
}

// PowerAction represents an IPMI chassis power control action.
type PowerAction int

const (
	PowerOn    PowerAction = iota
	PowerOff
	PowerReset
)

func (a PowerAction) String() string {
	switch a {
	case PowerOn:
		return "On"
	case PowerOff:
		return "Off"
	case PowerReset:
		return "Reset"
	default:
		return "Unknown"
	}
}

// Credentials holds IPMI connection parameters.
type Credentials struct {
	Address  string // IPMI IP address or hostname
	Port     int    // IPMI port (default 623)
	Username string
	Password string
}

// BMC is the interface for out-of-band server management via IPMI.
type BMC interface {
	// SetBootDevice sets the next boot device (one-time override, non-persistent).
	SetBootDevice(ctx context.Context, dev BootDevice) error

	// Power sends a chassis power control command.
	Power(ctx context.Context, action PowerAction) error

	// GetPowerStatus returns true if the server is currently powered on.
	GetPowerStatus(ctx context.Context) (bool, error)

	// Close releases the IPMI session.
	Close() error
}

// IsInstallImage returns true if the image name represents an OS install
// (i.e. the server should PXE boot to an installer). Returns false for
// empty, "localboot", and "baremetalservices" (agent-only boot).
func IsInstallImage(image string) bool {
	if image == "" {
		return false
	}
	lower := strings.ToLower(image)
	switch lower {
	case "localboot", "baremetalservices":
		return false
	}
	return true
}
