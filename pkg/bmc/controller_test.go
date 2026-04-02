package bmc

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestIsInstallImage(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"", false},
		{"localboot", false},
		{"baremetalservices", false},
		{"Localboot", false},
		{"BaremetalServices", false},
		{"fedora43", true},
		{"ubuntu-24.04", true},
		{"coreos", true},
		{"rhel9", true},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := IsInstallImage(tt.image); got != tt.want {
				t.Errorf("IsInstallImage(%q) = %v, want %v", tt.image, got, tt.want)
			}
		})
	}
}

func TestBootDeviceString(t *testing.T) {
	if s := BootDevicePXE.String(); s != "PXE" {
		t.Errorf("BootDevicePXE.String() = %q, want PXE", s)
	}
	if s := BootDeviceDisk.String(); s != "Disk" {
		t.Errorf("BootDeviceDisk.String() = %q, want Disk", s)
	}
}

func TestPowerActionString(t *testing.T) {
	if s := PowerOn.String(); s != "On" {
		t.Errorf("PowerOn.String() = %q, want On", s)
	}
	if s := PowerOff.String(); s != "Off" {
		t.Errorf("PowerOff.String() = %q, want Off", s)
	}
	if s := PowerReset.String(); s != "Reset" {
		t.Errorf("PowerReset.String() = %q, want Reset", s)
	}
}

func TestMockBMC(t *testing.T) {
	m := NewMock()
	ctx := context.Background()

	// Initial state
	if m.PoweredOn {
		t.Fatal("mock should start powered off")
	}
	if m.BootDevice != BootDeviceDisk {
		t.Fatalf("mock should start with disk boot, got %v", m.BootDevice)
	}

	// Power on
	if err := m.Power(ctx, PowerOn); err != nil {
		t.Fatal(err)
	}
	if !m.PoweredOn {
		t.Fatal("should be powered on after PowerOn")
	}

	// Set boot device
	if err := m.SetBootDevice(ctx, BootDevicePXE); err != nil {
		t.Fatal(err)
	}
	if m.LastBootDevice() != BootDevicePXE {
		t.Fatal("boot device should be PXE")
	}

	// Get power status
	on, err := m.GetPowerStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !on {
		t.Fatal("power status should be on")
	}

	// Power off
	if err := m.Power(ctx, PowerOff); err != nil {
		t.Fatal(err)
	}
	if m.PoweredOn {
		t.Fatal("should be powered off")
	}

	// Check call counts
	if n := m.CallCount("Power"); n != 2 {
		t.Fatalf("expected 2 Power calls, got %d", n)
	}
	if n := m.CallCount("SetBootDevice"); n != 1 {
		t.Fatalf("expected 1 SetBootDevice call, got %d", n)
	}
}

func TestControllerPowerOff(t *testing.T) {
	mock := NewMock()
	mock.PoweredOn = true

	var powerChangedKey string
	var powerChangedOn bool
	var mu sync.Mutex

	cb := Callbacks{
		OnPowerChanged: func(ctx context.Context, bmhKey string, poweredOn bool) {
			mu.Lock()
			powerChangedKey = bmhKey
			powerChangedOn = poweredOn
			mu.Unlock()
		},
	}

	// Use zap nop logger for tests
	log, _ := zapNop()
	c := NewController(nil, cb, MockClientFactory(mock), log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	c.Enqueue(PowerEvent{
		BMHKey:   "default/server1",
		BMHName:  "server1",
		Creds:    Credentials{Address: "10.0.0.1"},
		Action:   PowerOff,
		IsOnline: false,
	})

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	if mock.PoweredOn {
		t.Fatal("server should be powered off")
	}

	mu.Lock()
	if powerChangedKey != "default/server1" {
		t.Fatalf("expected callback for default/server1, got %q", powerChangedKey)
	}
	if powerChangedOn {
		t.Fatal("callback should report powered off")
	}
	mu.Unlock()
}

func TestControllerNormalPowerOn(t *testing.T) {
	mock := NewMock()

	log, _ := zapNop()
	c := NewController(nil, Callbacks{}, MockClientFactory(mock), log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	c.Enqueue(PowerEvent{
		BMHKey:   "default/server1",
		BMHName:  "server1",
		Creds:    Credentials{Address: "10.0.0.1"},
		Image:    "localboot",
		Action:   PowerOn,
		IsOnline: true,
	})

	time.Sleep(100 * time.Millisecond)

	if !mock.PoweredOn {
		t.Fatal("server should be powered on")
	}
	if mock.CallCount("Power") != 1 {
		t.Fatalf("expected 1 Power call, got %d", mock.CallCount("Power"))
	}
	// No SetBootDevice for normal power on
	if mock.CallCount("SetBootDevice") != 0 {
		t.Fatalf("expected 0 SetBootDevice calls for localboot, got %d", mock.CallCount("SetBootDevice"))
	}
}

func TestControllerInstallBoot(t *testing.T) {
	mock := NewMock()

	var mu sync.Mutex
	var installBootedKey string

	cb := Callbacks{
		OnInstallBooted: func(ctx context.Context, bmhKey string) {
			mu.Lock()
			installBootedKey = bmhKey
			mu.Unlock()
		},
	}
	_ = installBootedKey // used by callback goroutine (fires after DHCP timeout)

	log, _ := zapNop()
	// Use a very short timeout for testing by overriding the constant
	c := NewController(nil, cb, MockClientFactory(mock), log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	c.Enqueue(PowerEvent{
		BMHKey:   "default/server1",
		BMHName:  "server1",
		Creds:    Credentials{Address: "10.0.0.1"},
		BootMAC:  "AA:BB:CC:DD:EE:FF",
		Image:    "fedora43",
		Action:   PowerOn,
		IsOnline: true,
	})

	// Wait for initial IPMI operations
	time.Sleep(200 * time.Millisecond)

	// Should have set PXE and powered on
	if mock.CallCount("SetBootDevice") < 1 {
		t.Fatalf("expected at least 1 SetBootDevice call, got %d", mock.CallCount("SetBootDevice"))
	}
	if mock.CallCount("Power") < 1 {
		t.Fatalf("expected at least 1 Power call, got %d", mock.CallCount("Power"))
	}
	if !mock.PoweredOn {
		t.Fatal("server should be powered on")
	}

	// Without NATS subscription, the lease watcher will timeout.
	// The timeout is 3 minutes, so for the test we just verify
	// the initial PXE + power-on sequence is correct.
	// The callback will fire after the timeout in the background goroutine.
}

func TestControllerNoBMCAddress(t *testing.T) {
	mock := NewMock()

	log, _ := zapNop()
	c := NewController(nil, Callbacks{}, MockClientFactory(mock), log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	c.Enqueue(PowerEvent{
		BMHKey:   "default/server1",
		BMHName:  "server1",
		Creds:    Credentials{Address: ""},
		Action:   PowerOn,
		IsOnline: true,
	})

	time.Sleep(100 * time.Millisecond)

	// No IPMI calls should have been made
	if len(mock.Calls) != 0 {
		t.Fatalf("expected 0 calls with empty BMC address, got %d", len(mock.Calls))
	}
}

func TestControllerResetIfAlreadyOn(t *testing.T) {
	mock := NewMock()
	mock.PoweredOn = true

	log, _ := zapNop()
	c := NewController(nil, Callbacks{}, MockClientFactory(mock), log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Run(ctx)

	c.Enqueue(PowerEvent{
		BMHKey:   "default/server1",
		BMHName:  "server1",
		Creds:    Credentials{Address: "10.0.0.1"},
		Image:    "localboot",
		Action:   PowerOn,
		IsOnline: true,
	})

	time.Sleep(100 * time.Millisecond)

	// Should have called GetPowerStatus then Power(Reset)
	if mock.CallCount("GetPowerStatus") != 1 {
		t.Fatalf("expected 1 GetPowerStatus, got %d", mock.CallCount("GetPowerStatus"))
	}
	if mock.CallCount("Power") != 1 {
		t.Fatalf("expected 1 Power call, got %d", mock.CallCount("Power"))
	}
	// Verify it was a reset (server stays on)
	if !mock.PoweredOn {
		t.Fatal("server should still be on after reset")
	}
}

func zapNop() (*zap.SugaredLogger, error) {
	cfg := zap.NewDevelopmentConfig()
	cfg.Level = zap.NewAtomicLevelAt(zap.FatalLevel) // suppress test output
	l, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	return l.Sugar(), nil
}
