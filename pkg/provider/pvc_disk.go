package provider

// Disk-backed PVC helpers, shared by every transport.
//
// These used to live in pvc_iscsi.go, which is gone: the iSCSI data path was
// retired in favour of NVMe/TCP (#20). None of what remains here is
// iSCSI-specific — RouterOS mounts an attached disk the same way whatever
// carried it — so it outlived the path it was written for.

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/glennswest/mkube/pkg/routeros"
)

const (
	// Annotations describing the RouterOS disk behind a PVC.
	annPVCType    = "vkube.io/pvc-type"    // which provisioner owns it
	annDiskID     = "vkube.io/disk-id"     // RouterOS .id for the disk
	annDiskSlot   = "vkube.io/disk-slot"   // disk slot name (= mount-point name)
	annMountPoint = "vkube.io/mount-point" // RouterOS mount-point path
	// Recorded for whoever is debugging a volume by hand; nothing reads them
	// back. The historical spellings are kept so existing PVCs stay legible.
	annDiskTarget = "vkube.io/disk-iqn"     // target name (an NQN now)
	annDiskPortal = "vkube.io/iscsi-portal" // portal address:port
)

// waitForDiskMount blocks until RouterOS reports a mount-point for the disk.
//
// Polls at 150ms, not 1s: RouterOS probes and mounts an attached disk in well
// under a second, so a 1s tick spent ~1.1s of a 1.5s clone-provision simply
// waiting to notice. Each poll is one cheap /disk print.
func (p *MicroKubeProvider) waitForDiskMount(ctx context.Context, ros *routeros.Client, diskID string, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	var lastRows string
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for RouterOS to mount disk %s (rows: %s)", diskID, lastRows)
		case <-ticker.C:
			disk, err := ros.GetISCSIDisk(ctx, diskID)
			if err != nil {
				continue
			}
			if disk.MountPoint != "" {
				return "/" + disk.MountPoint, nil
			}
			// Check child rows (detected filesystem / partitions).
			all, err := ros.ListDisks(ctx)
			if err != nil {
				continue
			}
			var rows []string
			for i := range all {
				d := &all[i]
				related := d.ID == diskID ||
					(disk.Slot != "" && (d.Parent == disk.Slot || (d.Slot != disk.Slot && strings.HasPrefix(d.Slot, disk.Slot))))
				if !related {
					continue
				}
				rows = append(rows, fmt.Sprintf("{id=%s slot=%s type=%s parent=%s fs=%s mount=%s}",
					d.ID, d.Slot, d.Type, d.Parent, d.Filesystem, d.MountPoint))
				if d.ID != diskID && d.MountPoint != "" {
					return "/" + d.MountPoint, nil
				}
			}
			lastRows = strings.Join(rows, " ")
		}
	}
}

// setPVCDiskAnnotations records the RouterOS disk behind a PVC and persists it.
//
// `pvcType` is explicit because the caller is the only thing that knows which
// provisioner owns the volume. It used to be hard-coded to "iscsi", which meant
// a transport migration stamped "iscsi" onto a stormblock PVC and
// isStormblockPVC() then stopped recognising it — the cleanup path would look
// for a volume the wrong provisioner never made.
func (p *MicroKubeProvider) setPVCDiskAnnotations(ctx context.Context, pvc *corev1.PersistentVolumeClaim, pvcType, diskID, slot, mountPoint, target, portal string) {
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations[annPVCType] = pvcType
	pvc.Annotations[annDiskID] = diskID
	pvc.Annotations[annDiskSlot] = slot
	pvc.Annotations[annMountPoint] = mountPoint
	if target != "" {
		pvc.Annotations[annDiskTarget] = target
	}
	if portal != "" {
		pvc.Annotations[annDiskPortal] = portal
	}

	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := pvc.Namespace + "." + pvc.Name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("failed to persist PVC disk annotations", "key", storeKey, "error", err)
		}
	}
}

// parsePVCSize extracts the requested storage size from a PVC spec, in bytes.
func parsePVCSize(pvc *corev1.PersistentVolumeClaim) int64 {
	if pvc.Spec.Resources.Requests != nil {
		if storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			return storage.Value()
		}
	}
	return 0
}
