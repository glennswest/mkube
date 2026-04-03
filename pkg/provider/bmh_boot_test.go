package provider

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBMHBootRootPath verifies that syncBMHToNetwork sets the correct
// root_path and ipxe_boot_url on DHCP reservations for each boot type:
//
//   - baremetalservices: no root_path (nil → inherits pool default), no ipxe_boot_url
//   - install images (e.g. rawhideinstall): root_path = specific CDROM iSCSI target
//   - localboot: no root_path, has ipxe_boot_url (iPXE exit script)
func TestBMHBootRootPath(t *testing.T) {
	p, _ := newTestProvider(t)

	// Set up a data network with gateway and DNS
	testNet := &Network{
		ObjectMeta: metav1.ObjectMeta{Name: "g10"},
		Spec: NetworkSpec{
			Type:    NetworkTypeData,
			Gateway: "192.168.10.1",
			CIDR:    "192.168.10.0/24",
			DNS: NetworkDNSSpec{
				Zone: "g10.lo",
				// Server intentionally empty — no microdns in unit tests
			},
		},
	}
	p.networks.Set("g10", testNet)

	// Set up iSCSI CDROMs
	p.iscsiCdroms.Set("baremetalservices", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "baremetalservices"},
		Status: ISCSICdromStatus{
			Phase:     "Ready",
			TargetIQN: "iqn.2000-02.com.mikrotik:file1",
		},
	})
	p.iscsiCdroms.Set("rawhideinstall", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "rawhideinstall"},
		Status: ISCSICdromStatus{
			Phase:     "Ready",
			TargetIQN: "iqn.2000-02.com.mikrotik:file--raid1-iso-rawhideinstall-iso",
		},
	})
	p.iscsiCdroms.Set("fedora43", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "fedora43"},
		Status: ISCSICdromStatus{
			Phase:     "Ready",
			TargetIQN: "iqn.2000-02.com.mikrotik:file--raid1-iso-fedora43-iso",
		},
	})
	p.iscsiCdroms.Set("fcos-cloudid", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "fcos-cloudid"},
		Status: ISCSICdromStatus{
			Phase:     "Ready",
			TargetIQN: "iqn.2000-02.com.mikrotik:file23",
		},
	})

	tests := []struct {
		name             string
		image            string
		wantRootPath     string // "" means should be empty (inherit pool default)
		wantIPXEBootURL  bool   // true = should be set, false = should be empty
	}{
		{
			name:            "baremetalservices inherits pool default",
			image:           "baremetalservices",
			wantRootPath:    "",
			wantIPXEBootURL: false,
		},
		{
			name:            "rawhideinstall gets specific root_path",
			image:           "rawhideinstall",
			wantRootPath:    "iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:file--raid1-iso-rawhideinstall-iso",
			wantIPXEBootURL: true,
		},
		{
			name:            "fedora43 gets specific root_path",
			image:           "fedora43",
			wantRootPath:    "iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:file--raid1-iso-fedora43-iso",
			wantIPXEBootURL: true,
		},
		{
			name:            "fcos-cloudid gets specific root_path",
			image:           "fcos-cloudid",
			wantRootPath:    "iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:file23",
			wantIPXEBootURL: true,
		},
		{
			name:            "localboot has no root_path",
			image:           "localboot",
			wantRootPath:    "",
			wantIPXEBootURL: true,
		},
		{
			name:            "empty image has no root_path",
			image:           "",
			wantRootPath:    "",
			wantIPXEBootURL: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mac := "AA:BB:CC:DD:EE:" + strings.ToUpper(tt.name[:2])
			bmh := &BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-" + tt.name,
					Namespace: "default",
				},
				Spec: BMHSpec{
					Network:        "g10",
					BootMACAddress: mac,
					IP:             "192.168.10.99",
					Image:          tt.image,
				},
			}

			// Clear reservations from previous test
			testNet.Spec.DHCP.Reservations = nil

			p.syncBMHToNetwork(context.Background(), bmh, "", "", "", "")

			// Find the reservation by MAC
			var found *NetworkDHCPReservation
			for i, r := range testNet.Spec.DHCP.Reservations {
				if strings.EqualFold(r.MAC, mac) {
					found = &testNet.Spec.DHCP.Reservations[i]
					break
				}
			}

			if bmh.Spec.Network == "" || bmh.Spec.BootMACAddress == "" {
				if found != nil {
					t.Errorf("expected no reservation for empty network/MAC, got one")
				}
				return
			}

			if found == nil {
				t.Fatalf("reservation not found for MAC %s", mac)
			}

			// Check root_path
			if found.RootPath != tt.wantRootPath {
				t.Errorf("root_path = %q, want %q", found.RootPath, tt.wantRootPath)
			}

			// Check ipxe_boot_url
			if tt.wantIPXEBootURL && found.IPXEBootURL == "" {
				t.Errorf("expected ipxe_boot_url to be set, got empty")
			}
			if !tt.wantIPXEBootURL && found.IPXEBootURL != "" {
				t.Errorf("expected ipxe_boot_url to be empty, got %q", found.IPXEBootURL)
			}
		})
	}
}

// TestBMHBootDefaultToBaremetalservices verifies that when no per-BMH root_path
// is set (baremetalservices, localboot), the reservation's root_path is empty
// so microdns uses the pool default (baremetalservices iSCSI target).
func TestBMHBootDefaultToBaremetalservices(t *testing.T) {
	p, _ := newTestProvider(t)

	testNet := &Network{
		ObjectMeta: metav1.ObjectMeta{Name: "g10"},
		Spec: NetworkSpec{
			Type:    NetworkTypeData,
			Gateway: "192.168.10.1",
			DNS: NetworkDNSSpec{
				Zone: "g10.lo",
				// Server intentionally empty — no microdns in unit tests
			},
		},
	}
	p.networks.Set("g10", testNet)

	p.iscsiCdroms.Set("baremetalservices", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "baremetalservices"},
		Status: ISCSICdromStatus{
			Phase:     "Ready",
			TargetIQN: "iqn.2000-02.com.mikrotik:file1",
		},
	})

	// Both baremetalservices and localboot should have empty root_path
	// so they inherit the pool default (baremetalservices).
	for _, image := range []string{"baremetalservices", "localboot"} {
		bmh := &BareMetalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "test-" + image, Namespace: "default"},
			Spec: BMHSpec{
				Network:        "g10",
				BootMACAddress: "AA:BB:CC:DD:EE:01",
				IP:             "192.168.10.50",
				Image:          image,
			},
		}

		testNet.Spec.DHCP.Reservations = nil
		p.syncBMHToNetwork(context.Background(), bmh, "", "", "", "")

		if len(testNet.Spec.DHCP.Reservations) == 0 {
			t.Fatalf("image=%s: no reservation created", image)
		}

		res := testNet.Spec.DHCP.Reservations[0]
		if res.RootPath != "" {
			t.Errorf("image=%s: root_path should be empty (inherit pool default), got %q", image, res.RootPath)
		}
	}
}

// TestBMHBootInstallOverridesPool verifies that install images get a per-BMH
// root_path that overrides the pool default baremetalservices target.
func TestBMHBootInstallOverridesPool(t *testing.T) {
	p, _ := newTestProvider(t)

	testNet := &Network{
		ObjectMeta: metav1.ObjectMeta{Name: "g10"},
		Spec: NetworkSpec{
			Type:    NetworkTypeData,
			Gateway: "192.168.10.1",
			DNS: NetworkDNSSpec{
				Zone: "g10.lo",
				// Server intentionally empty — no microdns in unit tests
			},
		},
	}
	p.networks.Set("g10", testNet)

	p.iscsiCdroms.Set("baremetalservices", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "baremetalservices"},
		Status:     ISCSICdromStatus{Phase: "Ready", TargetIQN: "iqn.2000-02.com.mikrotik:file1"},
	})
	p.iscsiCdroms.Set("rawhideinstall", &ISCSICdrom{
		ObjectMeta: metav1.ObjectMeta{Name: "rawhideinstall"},
		Status:     ISCSICdromStatus{Phase: "Ready", TargetIQN: "iqn.2000-02.com.mikrotik:rawhide"},
	})

	bmh := &BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "server4", Namespace: "default"},
		Spec: BMHSpec{
			Network:        "g10",
			BootMACAddress: "AA:BB:CC:DD:EE:04",
			IP:             "192.168.10.13",
			Image:          "rawhideinstall",
		},
	}

	p.syncBMHToNetwork(context.Background(), bmh, "", "", "", "")

	if len(testNet.Spec.DHCP.Reservations) == 0 {
		t.Fatal("no reservation created")
	}

	res := testNet.Spec.DHCP.Reservations[0]

	// Must NOT be the baremetalservices target
	if strings.Contains(res.RootPath, "file1") {
		t.Errorf("root_path contains baremetalservices IQN, should be rawhideinstall: %s", res.RootPath)
	}

	// Must be the rawhideinstall target
	want := "iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:rawhide"
	if res.RootPath != want {
		t.Errorf("root_path = %q, want %q", res.RootPath, want)
	}
}
