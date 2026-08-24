package provider

// Moving an existing stormblock volume onto the configured transport.
//
// A volume's transport is fixed when its export is created, so flipping
// `storage.stormblock.transport` only governs volumes created afterwards.
// Anything provisioned earlier keeps the transport it was born with, which
// is how a cluster ends up with iSCSI mounts long after the config says
// NVMe.
//
// Nothing here touches the DATA. The volume lives in stormblock's slab and
// is untouched — only the export in front of it and the disk RouterOS
// attaches through are replaced. That is what makes this safe to run on a
// volume a pod is using, and why it is a migration rather than a restore.
//
// The pod is a separate concern: its container mount names a host path
// (/iscsi3), and after migration the volume lives at a different one
// (/nvme-tcp1). The PVC's annotations are updated here; the pod must be
// recreated to pick the new path up, which the caller decides.
//
// POST /api/v1/namespaces/{namespace}/persistentvolumeclaims/{name}/transport

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type transportMigrationReport struct {
	PVC       string   `json:"pvc"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	VolumeID  string   `json:"volumeId,omitempty"`
	OldMount  string   `json:"oldMountPoint,omitempty"`
	NewMount  string   `json:"newMountPoint,omitempty"`
	Migrated  bool     `json:"migrated"`
	PodsToFix []string `json:"podsNeedingRecreate,omitempty"`
	Steps     []string `json:"steps"`
	Error     string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleMigratePVCTransport(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	rep := p.MigratePVCTransport(ctx, ns, name)
	code := http.StatusOK
	if rep.Error != "" {
		code = http.StatusInternalServerError
	}
	podWriteJSON(w, code, rep)
}

// MigratePVCTransport re-exports a stormblock PVC's volume on the configured
// transport and re-attaches it, preserving the volume's contents.
func (p *MicroKubeProvider) MigratePVCTransport(ctx context.Context, namespace, name string) *transportMigrationReport {
	key := namespace + "/" + name
	rep := &transportMigrationReport{PVC: key, To: p.sbProtocol()}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("PVC-TRANSPORT: " + s)
	}

	ros := p.getRouterOSClient()
	if ros == nil {
		rep.Error = "transport migration requires the RouterOS backend"
		return rep
	}

	// The store keys PVCs as "<namespace>.<name>" (NATS KV keys cannot carry
	// a slash); `key` above is only the display form.
	storeKey := namespace + "." + name
	var stored corev1.PersistentVolumeClaim
	if _, err := p.deps.Store.PersistentVolumeClaims.GetJSON(ctx, storeKey, &stored); err != nil {
		rep.Error = fmt.Sprintf("PVC %s not found: %v", key, err)
		return rep
	}
	pvc := &stored
	if !isStormblockPVC(pvc) {
		rep.Error = fmt.Sprintf("PVC %s is not a stormblock volume; only stormblock volumes carry a transport", key)
		return rep
	}

	ann := pvc.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	volumeID := ann[annSBVolumeID]
	if volumeID == "" {
		rep.Error = fmt.Sprintf("PVC %s has no stormblock volume id recorded", key)
		return rep
	}
	rep.VolumeID = volumeID
	rep.OldMount = ann[annMountPoint]
	oldDiskID := ann[annDiskID]

	// What transport is it on today? The attached disk row is the ground
	// truth — annotations can lag, the device cannot.
	if oldDiskID != "" {
		if d, derr := ros.GetISCSIDisk(ctx, oldDiskID); derr == nil {
			rep.From = d.Type
		}
	}
	if rep.From == p.sbProtocol() {
		step("already on %s, nothing to do", rep.To)
		rep.Migrated = false
		return rep
	}
	step("migrating volume %s from %s to %s", volumeID, rep.From, rep.To)

	sb, err := p.newStormblockClient()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}

	// Detach first so the initiator is gone before the target is withdrawn —
	// the same ordering deprovision uses, so stormblockmk sees a clean
	// session teardown rather than relying on its grace period.
	if oldDiskID != "" {
		if err := ros.RemoveDisk(ctx, oldDiskID); err != nil {
			step("could not detach old disk %s: %v (continuing)", oldDiskID, err)
		} else {
			step("detached old disk %s", oldDiskID)
		}
	}
	if oldExport := ann[annSBExportID]; oldExport != "" {
		if err := sb.do(ctx, http.MethodDelete, "/mk/v1/exports/"+oldExport, nil, nil); err != nil {
			step("could not withdraw old export %s: %v (continuing)", oldExport, err)
		} else {
			step("withdrew old %s export", rep.From)
		}
	}

	// New export on the configured transport, same volume.
	var ex sbExport
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/exports",
		map[string]any{"volume_id": volumeID, "protocol": p.sbProtocol()}, &ex); err != nil {
		rep.Error = fmt.Sprintf("creating %s export: %v", rep.To, err)
		return rep
	}
	attach := ex.Attach
	if attach.Transport == "" {
		attach.Transport = ex.Protocol
	}
	if attach.Address == "" {
		rep.Error = "new export returned no attach parameters"
		return rep
	}
	step("exported %s on %s:%d", sbTargetName(attach), attach.Address, attach.Port)

	diskID, err := p.attachStormblockDisk(ctx, attach)
	if err != nil {
		rep.Error = "attaching the migrated export: " + err.Error()
		return rep
	}
	mountPoint, err := p.waitForDiskMount(ctx, ros, diskID, cowMountWait)
	if err != nil {
		rep.Error = fmt.Sprintf("volume %s attached over %s but did not mount: %v", volumeID, rep.To, err)
		return rep
	}
	rep.NewMount = mountPoint
	step("mounted at %s", mountPoint)

	slot := ""
	if d, derr := ros.GetISCSIDisk(ctx, diskID); derr == nil {
		slot = d.Slot
	}
	portal := fmt.Sprintf("%s:%d", attach.Address, attach.Port)
	p.setPVCDiskAnnotations(ctx, pvc, pvcTypeStormblock, diskID, slot, mountPoint, sbTargetName(attach), portal)
	if ex.ExportID != "" {
		p.annotatePVC(ctx, pvc, annSBExportID, ex.ExportID)
	}
	rep.Migrated = true

	// Any pod mounting this PVC still names the OLD host path in its
	// container mount, so report them rather than silently leaving a pod
	// pointing at a path that no longer exists.
	if rep.OldMount != mountPoint {
		rep.PodsToFix = p.podsUsingPVC(namespace, name)
		if len(rep.PodsToFix) > 0 {
			step("mount path changed %s -> %s; %d pod(s) must be recreated to follow it",
				rep.OldMount, mountPoint, len(rep.PodsToFix))
		}
	}
	return rep
}

// podsUsingPVC names the pods that mount a PVC.
func (p *MicroKubeProvider) podsUsingPVC(namespace, name string) []string {
	var out []string
	p.pods.Range(func(_ string, pod *corev1.Pod) bool {
		if pod == nil || pod.Namespace != namespace {
			return true
		}
		for i := range pod.Spec.Volumes {
			pvcRef := pod.Spec.Volumes[i].PersistentVolumeClaim
			if pvcRef != nil && pvcRef.ClaimName == name {
				out = append(out, pod.Namespace+"/"+pod.Name)
				break
			}
		}
		return true
	})
	return out
}

// annotatePVC sets one annotation and persists the PVC.
func (p *MicroKubeProvider) annotatePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, key, value string) {
	ann := pvc.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = value
	pvc.SetAnnotations(ann)
	storeKey := pvc.Namespace + "." + pvc.Name
	if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
		p.deps.Logger.Warnw("persisting PVC annotation", "pvc", storeKey, "key", key, "error", err)
	}
}
