package proxmox

import (
	"testing"
)

func TestVMIDAllocatorBasic(t *testing.T) {
	alloc, err := NewVMIDAllocator("200-205")
	if err != nil {
		t.Fatalf("NewVMIDAllocator: %v", err)
	}

	// Allocate sequentially
	vmid, err := alloc.Allocate("a")
	if err != nil || vmid != 200 {
		t.Errorf("Allocate(a) = %d, %v, want 200", vmid, err)
	}

	vmid, err = alloc.Allocate("b")
	if err != nil || vmid != 201 {
		t.Errorf("Allocate(b) = %d, %v, want 201", vmid, err)
	}

	// Same name returns same VMID
	vmid, err = alloc.Allocate("a")
	if err != nil || vmid != 200 {
		t.Errorf("Allocate(a) again = %d, %v, want 200", vmid, err)
	}
}

func TestVMIDAllocatorExhausted(t *testing.T) {
	alloc, err := NewVMIDAllocator("100-102")
	if err != nil {
		t.Fatalf("NewVMIDAllocator: %v", err)
	}

	for _, name := range []string{"a", "b", "c"} {
		if _, err := alloc.Allocate(name); err != nil {
			t.Fatalf("Allocate(%s): %v", name, err)
		}
	}

	// Range exhausted
	_, err = alloc.Allocate("d")
	if err == nil {
		t.Fatal("expected error for exhausted range")
	}
}

func TestVMIDAllocatorRelease(t *testing.T) {
	alloc, err := NewVMIDAllocator("100-102")
	if err != nil {
		t.Fatalf("NewVMIDAllocator: %v", err)
	}

	alloc.Allocate("a") // 100
	alloc.Allocate("b") // 101
	alloc.Allocate("c") // 102

	// Release middle one
	alloc.Release("b")

	// Next allocation should get the released VMID
	vmid, err := alloc.Allocate("d")
	if err != nil || vmid != 101 {
		t.Errorf("Allocate(d) after release = %d, %v, want 101", vmid, err)
	}
}

func TestVMIDAllocatorMarkUsed(t *testing.T) {
	alloc, err := NewVMIDAllocator("200-210")
	if err != nil {
		t.Fatalf("NewVMIDAllocator: %v", err)
	}

	// Simulate discovery: VMIDs 200, 203, 205 already in use
	alloc.MarkUsed(200, "web")
	alloc.MarkUsed(203, "dns")
	alloc.MarkUsed(205, "db")

	// Lookup
	if vmid, ok := alloc.Lookup("web"); !ok || vmid != 200 {
		t.Errorf("Lookup(web) = %d, %v, want 200/true", vmid, ok)
	}
	if name, ok := alloc.LookupName(203); !ok || name != "dns" {
		t.Errorf("LookupName(203) = %q, %v, want dns/true", name, ok)
	}

	// Allocate should skip used VMIDs
	vmid, _ := alloc.Allocate("new1")
	if vmid != 201 {
		t.Errorf("first alloc = %d, want 201", vmid)
	}
	vmid, _ = alloc.Allocate("new2")
	if vmid != 202 {
		t.Errorf("second alloc = %d, want 202", vmid)
	}
	vmid, _ = alloc.Allocate("new3")
	if vmid != 204 {
		t.Errorf("third alloc = %d, want 204 (skip 203)", vmid)
	}
}

func TestVMIDAllocatorInRange(t *testing.T) {
	alloc, err := NewVMIDAllocator("200-299")
	if err != nil {
		t.Fatalf("NewVMIDAllocator: %v", err)
	}

	if !alloc.InRange(200) {
		t.Error("200 should be in range")
	}
	if !alloc.InRange(299) {
		t.Error("299 should be in range")
	}
	if alloc.InRange(199) {
		t.Error("199 should not be in range")
	}
	if alloc.InRange(300) {
		t.Error("300 should not be in range")
	}
}

func TestVMIDAllocatorInvalidRange(t *testing.T) {
	tests := []string{
		"invalid",
		"abc-def",
		"200",
		"300-200",
	}

	for _, spec := range tests {
		_, err := NewVMIDAllocator(spec)
		if err == nil {
			t.Errorf("NewVMIDAllocator(%q) should fail", spec)
		}
	}
}
