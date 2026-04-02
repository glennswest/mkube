package bmc

import (
	"context"
	"sync"
)

// MockBMC is a test double that records all BMC operations.
type MockBMC struct {
	mu          sync.Mutex
	PoweredOn   bool
	BootDevice  BootDevice
	Calls       []MockCall
	FailNext    error // if non-nil, the next call returns this error then clears it
}

// MockCall records a single BMC method invocation.
type MockCall struct {
	Method string
	Args   []interface{}
}

// NewMock creates a MockBMC in powered-off state with Disk boot device.
func NewMock() *MockBMC {
	return &MockBMC{
		PoweredOn:  false,
		BootDevice: BootDeviceDisk,
	}
}

func (m *MockBMC) record(method string, args ...interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, MockCall{Method: method, Args: args})
	if m.FailNext != nil {
		err := m.FailNext
		m.FailNext = nil
		return err
	}
	return nil
}

func (m *MockBMC) SetBootDevice(ctx context.Context, dev BootDevice) error {
	if err := m.record("SetBootDevice", dev); err != nil {
		return err
	}
	m.mu.Lock()
	m.BootDevice = dev
	m.mu.Unlock()
	return nil
}

func (m *MockBMC) Power(ctx context.Context, action PowerAction) error {
	if err := m.record("Power", action); err != nil {
		return err
	}
	m.mu.Lock()
	switch action {
	case PowerOn:
		m.PoweredOn = true
	case PowerOff:
		m.PoweredOn = false
	case PowerReset:
		m.PoweredOn = true
	}
	m.mu.Unlock()
	return nil
}

func (m *MockBMC) GetPowerStatus(ctx context.Context) (bool, error) {
	if err := m.record("GetPowerStatus"); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.PoweredOn, nil
}

func (m *MockBMC) Close() error {
	return m.record("Close")
}

// CallCount returns the number of times a method was called.
func (m *MockBMC) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// LastBootDevice returns the last boot device that was set.
func (m *MockBMC) LastBootDevice() BootDevice {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.BootDevice
}

// MockClientFactory returns a NewClient-compatible function that always
// returns the given mock. Useful for injecting into the controller.
func MockClientFactory(mock *MockBMC) func(Credentials) (BMC, error) {
	return func(creds Credentials) (BMC, error) {
		return mock, nil
	}
}
