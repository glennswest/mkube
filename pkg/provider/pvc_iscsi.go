package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/glennswest/mkube/pkg/routeros"
)

// iSCSI PVC constants
const (
	// pvcTypeISCSI is the storageClassName or annotation value for iSCSI-backed PVCs.
	pvcTypeISCSI = "iscsi"

	// Annotations used to track iSCSI PVC state
	annPVCType     = "vkube.io/pvc-type"      // "iscsi" for iSCSI-backed PVCs
	annDiskID      = "vkube.io/disk-id"        // RouterOS .id for the file disk
	annDiskSlot    = "vkube.io/disk-slot"       // disk slot name (= mount-point name)
	annMountPoint  = "vkube.io/mount-point"     // RouterOS mount-point path
	annDiskIQN     = "vkube.io/disk-iqn"        // iSCSI target IQN (for debug)
	annISCSIPortal = "vkube.io/iscsi-portal"    // iSCSI portal IP:port
)

// isISCSIPVC returns true if the PVC should use iSCSI-backed block storage
// instead of directory-based bind mounts.
func isISCSIPVC(pvc *corev1.PersistentVolumeClaim) bool {
	if ann := pvc.GetAnnotations(); ann != nil {
		if ann[annPVCType] == pvcTypeISCSI {
			return true
		}
	}
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == pvcTypeISCSI {
		return true
	}
	return false
}

// iscsiPVCHostPath returns the mount-point path for an already-provisioned iSCSI PVC.
// Returns empty string if not yet provisioned.
func iscsiPVCHostPath(pvc *corev1.PersistentVolumeClaim) string {
	if ann := pvc.GetAnnotations(); ann != nil {
		return ann[annMountPoint]
	}
	return ""
}

// provisionISCSIPVC creates an iSCSI-backed PVC on RouterOS.
// Flow:
//  1. Register file as file-backed disk on RouterOS
//  2. Enable iSCSI export
//  3. Format as ext4 via iscsi-pvc tool
//  4. Disable iSCSI export — RouterOS auto-detects ext4 and mounts
//  5. Wait for RouterOS to mount the disk
//  6. Store disk metadata in PVC annotations
//  7. Return the mount-point path
func (p *MicroKubeProvider) provisionISCSIPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	log := p.deps.Logger.With("pvc", pvc.Namespace+"/"+pvc.Name, "type", "iscsi")

	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return "", fmt.Errorf("iSCSI PVC requires RouterOS backend")
	}

	// Determine pool and file path
	pool := p.resolveStoragePool(p.pvcPoolName(pvc))
	fileName := fmt.Sprintf("%s_%s.img", pvc.Namespace, pvc.Name)
	filePath := fmt.Sprintf("%s/%s", pool.volumesPath(), fileName)
	rosFilePath := strings.TrimPrefix(filePath, "/")

	// Check if already provisioned
	if ann := pvc.GetAnnotations(); ann != nil && ann[annDiskID] != "" {
		if mp := ann[annMountPoint]; mp != "" {
			log.Debugw("iSCSI PVC already provisioned", "mountPoint", mp)
			return mp, nil
		}
		// Disk exists but mount-point not recorded — try to recover
		disk, err := rosClient.GetISCSIDisk(ctx, ann[annDiskID])
		if err == nil && disk.MountPoint != "" {
			mp := "/" + disk.MountPoint
			p.setISCSIPVCAnnotations(ctx, pvc, disk.ID, disk.Slot, mp, disk.ISCSIServerIQN, "")
			return mp, nil
		}
	}

	log.Infow("provisioning iSCSI PVC", "filePath", filePath)

	// Ensure the volumes directory exists
	dirPath := strings.TrimPrefix(pool.volumesPath(), "/")
	if err := rosClient.EnsureDirectory(ctx, dirPath); err != nil {
		log.Warnw("failed to ensure PVC directory", "path", dirPath, "error", err)
	}

	// Step 1: Create the file-backed disk on RouterOS
	diskID, err := rosClient.CreateFileDisk(ctx, rosFilePath)
	if err != nil {
		// Disk might already exist — try to find it
		existing, findErr := rosClient.FindFileDiskByPath(ctx, rosFilePath)
		if findErr != nil || existing == nil {
			return "", fmt.Errorf("creating file disk: %w", err)
		}
		diskID = existing.ID
		log.Infow("reusing existing file disk", "diskID", diskID)
	}

	// Step 2: Enable iSCSI export for formatting
	if err := rosClient.SetISCSIExport(ctx, diskID, true); err != nil {
		return "", fmt.Errorf("enabling iSCSI export: %w", err)
	}

	// Get disk details (slot, IQN)
	disk, err := rosClient.GetISCSIDisk(ctx, diskID)
	if err != nil {
		return "", fmt.Errorf("getting disk details: %w", err)
	}
	log.Infow("iSCSI target active", "diskID", diskID, "slot", disk.Slot, "iqn", disk.ISCSIServerIQN)

	// Determine portal IP
	portalIP := p.deps.Config.Storage.ISCSIPortalIP
	if portalIP == "" {
		portalIP = p.deps.Config.RouterOS.Address
		if idx := strings.Index(portalIP, ":"); idx > 0 {
			portalIP = portalIP[:idx]
		}
	}

	// Step 3: Format as ext4 via iscsi-pvc tool
	if disk.ISCSIServerIQN != "" {
		log.Infow("formatting iSCSI target as ext4", "portal", portalIP, "iqn", disk.ISCSIServerIQN)
		if err := p.formatISCSITargetExt4(ctx, portalIP, disk.ISCSIServerIQN, pvc.Name); err != nil {
			_ = rosClient.SetISCSIExport(ctx, diskID, false)
			return "", fmt.Errorf("formatting ext4: %w", err)
		}
	}

	// Step 4: Disable iSCSI export — RouterOS detects ext4 and auto-mounts
	if err := rosClient.SetISCSIExport(ctx, diskID, false); err != nil {
		return "", fmt.Errorf("disabling iSCSI export: %w", err)
	}

	// Step 5: Wait for RouterOS to mount the disk
	mountPoint, err := p.waitForDiskMount(ctx, rosClient, diskID, 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("waiting for disk mount: %w", err)
	}

	log.Infow("iSCSI PVC provisioned", "mountPoint", mountPoint, "diskID", diskID)

	// Step 6: Store metadata in PVC annotations
	portalAddr := fmt.Sprintf("%s:3260", portalIP)
	p.setISCSIPVCAnnotations(ctx, pvc, diskID, disk.Slot, mountPoint, disk.ISCSIServerIQN, portalAddr)

	return mountPoint, nil
}

// waitForDiskMount polls RouterOS until the file disk has a mount-point.
func (p *MicroKubeProvider) waitForDiskMount(ctx context.Context, ros *routeros.Client, diskID string, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for RouterOS to mount disk %s", diskID)
		case <-ticker.C:
			disk, err := ros.GetISCSIDisk(ctx, diskID)
			if err != nil {
				continue
			}
			if disk.MountPoint != "" {
				return "/" + disk.MountPoint, nil
			}
		}
	}
}

// formatISCSITargetExt4 formats an iSCSI target as ext4 using the iscsi-pvc tool.
func (p *MicroKubeProvider) formatISCSITargetExt4(ctx context.Context, portalIP, iqn, label string) error {
	log := p.deps.Logger.With("portal", portalIP, "iqn", iqn)

	user := p.deps.Config.RouterOS.User
	password := p.deps.Config.RouterOS.Password
	restURL := p.deps.Config.RouterOS.RESTURL

	log.Infow("executing iscsi-pvc format", "label", label)

	cmd := exec.CommandContext(ctx, "iscsi-pvc",
		"--url", restURL,
		"--user", user,
		"--password", password,
		"--portal", portalIP,
		"format", iqn,
		"--label", label,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnw("iscsi-pvc format failed", "error", err, "output", string(output))
		return fmt.Errorf("iscsi-pvc format: %w: %s", err, string(output))
	}

	log.Infow("iscsi-pvc format succeeded", "output", string(output))
	return nil
}

// setISCSIPVCAnnotations stores iSCSI disk metadata in the PVC annotations.
func (p *MicroKubeProvider) setISCSIPVCAnnotations(ctx context.Context, pvc *corev1.PersistentVolumeClaim, diskID, slot, mountPoint, iqn, portal string) {
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	pvc.Annotations[annPVCType] = pvcTypeISCSI
	pvc.Annotations[annDiskID] = diskID
	pvc.Annotations[annDiskSlot] = slot
	pvc.Annotations[annMountPoint] = mountPoint
	if iqn != "" {
		pvc.Annotations[annDiskIQN] = iqn
	}
	if portal != "" {
		pvc.Annotations[annISCSIPortal] = portal
	}

	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := pvc.Namespace + "." + pvc.Name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("failed to persist iSCSI PVC annotations", "key", storeKey, "error", err)
		}
	}
}

// cleanupISCSIPVC removes the iSCSI-backed PVC: removes the file disk from
// RouterOS which also unmounts the filesystem and deletes the backing file.
func (p *MicroKubeProvider) cleanupISCSIPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return fmt.Errorf("iSCSI PVC cleanup requires RouterOS backend")
	}

	ann := pvc.GetAnnotations()
	if ann == nil || ann[annDiskID] == "" {
		return nil
	}

	diskID := ann[annDiskID]
	log := p.deps.Logger.With("pvc", pvc.Namespace+"/"+pvc.Name, "diskID", diskID)
	log.Infow("cleaning up iSCSI PVC")

	_ = rosClient.SetISCSIExport(ctx, diskID, false)

	if err := rosClient.RemoveFileDisk(ctx, diskID); err != nil {
		log.Warnw("failed to remove file disk", "error", err)
		return fmt.Errorf("removing file disk: %w", err)
	}

	log.Infow("iSCSI PVC cleaned up")
	return nil
}

// resolveISCSIPVCVolume handles volume resolution for iSCSI-backed PVCs.
// If the PVC is already provisioned, returns the mount-point path.
// Otherwise, provisions it first.
func (p *MicroKubeProvider) resolveISCSIPVCVolume(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	// Check if already provisioned
	if mp := iscsiPVCHostPath(pvc); mp != "" {
		rosClient := p.getRouterOSClient()
		if rosClient != nil {
			if ann := pvc.GetAnnotations(); ann != nil {
				if diskID := ann[annDiskID]; diskID != "" {
					disk, err := rosClient.GetISCSIDisk(ctx, diskID)
					if err == nil && disk.MountPoint != "" {
						return "/" + disk.MountPoint, nil
					}
				}
			}
		}
	}
	return p.provisionISCSIPVC(ctx, pvc)
}

// parsePVCSize extracts the requested storage size from PVC spec in bytes.
func parsePVCSize(pvc *corev1.PersistentVolumeClaim) int64 {
	if pvc.Spec.Resources.Requests != nil {
		if storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			return storage.Value()
		}
	}
	return 0
}

// formatSizeForRouterOS formats bytes as a human-readable size for logging.
func formatSizeForRouterOS(bytes int64) string {
	const (
		gib = 1024 * 1024 * 1024
		mib = 1024 * 1024
	)
	if bytes >= gib {
		return fmt.Sprintf("%dG", bytes/gib)
	}
	return fmt.Sprintf("%dM", bytes/mib)
}

// ReconcileISCSIPVCMounts checks all iSCSI PVCs and ensures their disks
// are properly mounted (e.g. after RouterOS reboot).
func (p *MicroKubeProvider) ReconcileISCSIPVCMounts(ctx context.Context) {
	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return
	}

	for _, pvc := range p.pvcs.Snapshot() {
		if !isISCSIPVC(pvc) {
			continue
		}
		ann := pvc.GetAnnotations()
		if ann == nil || ann[annDiskID] == "" {
			continue
		}

		diskID := ann[annDiskID]
		disk, err := rosClient.GetISCSIDisk(ctx, diskID)
		if err != nil {
			p.deps.Logger.Warnw("iSCSI PVC disk not found on RouterOS",
				"pvc", pvc.Namespace+"/"+pvc.Name,
				"diskID", diskID, "error", err)
			continue
		}

		if disk.MountPoint != "" {
			mp := "/" + disk.MountPoint
			if ann[annMountPoint] != mp {
				p.deps.Logger.Infow("updating iSCSI PVC mount-point",
					"pvc", pvc.Namespace+"/"+pvc.Name,
					"old", ann[annMountPoint], "new", mp)
				p.setISCSIPVCAnnotations(ctx, pvc, diskID, disk.Slot, mp, disk.ISCSIServerIQN, ann[annISCSIPortal])
			}
		}
	}
}
