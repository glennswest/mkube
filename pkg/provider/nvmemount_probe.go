package provider

// Can RouterOS MOUNT an NVMe-TCP disk?
//
// Attaching is already proven (see nvme_probe.go): the 7.22.2 initiator
// accepts `/disk add type=nvme-tcp` and the row appears. That is not the
// question that matters. mkube's PVC path needs RouterOS to put a
// filesystem on the disk and expose it at /<slot> so containers can bind
// mount it. If the initiator attaches but never mounts, NVMe cannot carry
// PVCs no matter how good the transport is, and "no iSCSI anywhere" has to
// come from a different direction (a newer RouterOS, or device passthrough
// that lets the container mount for itself).
//
// Three legs, ordered so each one isolates a single variable:
//
//	CONTROL   volume V1, formatted ext4, attached over iSCSI.
//	          Establishes that V1 and its filesystem are good.
//	NVMe-PRE  the SAME volume V1, re-exported nvme-tcp and re-attached.
//	          Transport is now the only thing that changed, so a mount here
//	          and a mount in CONTROL differ by exactly one variable.
//	NVMe-FMT  a raw volume V2 over nvme-tcp that RouterOS formats ITSELF.
//	          The strongest test available: RouterOS can only format what it
//	          can write and re-read, and it mounts what it has just
//	          formatted. If this passes, NVMe is fully usable and the
//	          pre-formatted leg failing would point at superblock probing
//	          rather than at the transport.
//
// POST /api/v1/probes/nvmemount

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type nvmeMountLeg struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Attached  bool   `json:"attached"`
	Mounted   bool   `json:"mounted"`
	MountPath string `json:"mountPath,omitempty"`
	Rows      string `json:"rows,omitempty"`
	Error     string `json:"error,omitempty"`
}

type NVMeMountReport struct {
	Verdict string         `json:"verdict"`
	Legs    []nvmeMountLeg `json:"legs"`
	Steps   []string       `json:"steps"`
	Error   string         `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleNVMeMountProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunNVMeMountProbe(ctx))
}

func (p *MicroKubeProvider) RunNVMeMountProbe(ctx context.Context) *NVMeMountReport {
	rep := &NVMeMountReport{Verdict: "unknown"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("NVMEMOUNT-PROBE: " + s)
	}

	ros := p.getRouterOSClient()
	if ros == nil {
		rep.Error = "probe requires the RouterOS backend"
		return rep
	}
	sb, err := p.newStormblockClient()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}

	const size = 256 * 1024 * 1024

	// --- helpers -----------------------------------------------------------

	newVolume := func(name string) (string, error) {
		var created sbCreateVolumeResp
		err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes",
			map[string]any{"name": name, "size_bytes": size, "export": false}, &created)
		if err != nil {
			return "", err
		}
		return created.ID, nil
	}
	dropVolume := func(id string) {
		if id == "" {
			return
		}
		if err := sb.do(context.Background(), http.MethodDelete, "/mk/v1/volumes/"+id+"?force=true", nil, nil); err != nil {
			p.deps.Logger.Warnw("probe could not remove volume", "volume", id, "error", err)
		}
	}
	addExport := func(volID, proto string) (*sbExport, error) {
		var ex sbExport
		if err := sb.do(ctx, http.MethodPost, "/mk/v1/exports",
			map[string]any{"volume_id": volID, "protocol": proto}, &ex); err != nil {
			return nil, err
		}
		if ex.Attach.Transport == "" {
			ex.Attach.Transport = ex.Protocol
		}
		if ex.Attach.Address == "" {
			return nil, fmt.Errorf("%s export returned no attach parameters", proto)
		}
		return &ex, nil
	}
	dropExport := func(id string) {
		if id == "" {
			return
		}
		if err := sb.do(context.Background(), http.MethodDelete, "/mk/v1/exports/"+id, nil, nil); err != nil {
			p.deps.Logger.Warnw("probe could not withdraw export", "export", id, "error", err)
		}
	}
	detach := func(diskID string) {
		if diskID == "" {
			return
		}
		if err := ros.RemoveDisk(context.Background(), diskID); err != nil {
			p.deps.Logger.Warnw("probe could not detach disk", "disk", diskID, "error", err)
		}
	}
	// describe reports what RouterOS thinks of the disk, so a failed leg
	// carries evidence rather than just "no".
	describe := func(diskID string) string {
		d, err := ros.GetISCSIDisk(context.Background(), diskID)
		if err != nil {
			return "disk row unreadable: " + err.Error()
		}
		return fmt.Sprintf("{slot=%s type=%s fs=%s mount=%s block-device=%s}",
			d.Slot, d.Type, d.Filesystem, d.MountPoint, d.BlockDevice)
	}

	// runLeg attaches an export and waits for RouterOS to mount it.
	runLeg := func(name string, ex *sbExport, format bool) (nvmeMountLeg, string) {
		leg := nvmeMountLeg{Name: name, Transport: sbTransport(ex.Attach)}
		diskID, err := p.attachStormblockDisk(ctx, ex.Attach)
		if err != nil {
			leg.Error = "attach: " + err.Error()
			return leg, ""
		}
		leg.Attached = true
		step("%s: attached as disk %s", name, diskID)

		if format {
			// Let RouterOS lay the filesystem down itself.
			if err := ros.FormatDrive(ctx, diskID, "ext4", "nvmefmt"); err != nil {
				leg.Error = "format-drive: " + err.Error()
				leg.Rows = describe(diskID)
				return leg, diskID
			}
			step("%s: RouterOS accepted format-drive ext4", name)
		}

		// 90s: a local format finishes in seconds, but a format over a
		// network transport plus the mount that follows deserves room.
		mp, err := p.waitForDiskMount(ctx, ros, diskID, 90*time.Second)
		if err != nil {
			leg.Error = err.Error()
			leg.Rows = describe(diskID)
			return leg, diskID
		}
		leg.Mounted = true
		leg.MountPath = mp
		leg.Rows = describe(diskID)
		step("%s: MOUNTED at %s", name, mp)
		return leg, diskID
	}

	// --- leg 1: control, iSCSI with a filesystem we wrote ------------------

	v1, err := newVolume("nvmemount-probe-v1")
	if err != nil {
		rep.Error = "creating control volume: " + err.Error()
		return rep
	}
	defer dropVolume(v1)
	step("control volume %s created (%d MiB)", v1, size/1024/1024)

	iscsiEx, err := addExport(v1, "iscsi")
	if err != nil {
		rep.Error = "iscsi export: " + err.Error()
		return rep
	}
	if err := p.formatStormblockVolume(ctx, sb, v1, iscsiEx.Attach, "nvmeprobe"); err != nil {
		dropExport(iscsiEx.ExportID)
		rep.Error = "formatting control volume: " + err.Error()
		return rep
	}
	step("control volume formatted ext4 over iscsi")

	controlLeg, controlDisk := runLeg("CONTROL iscsi pre-formatted", iscsiEx, false)
	rep.Legs = append(rep.Legs, controlLeg)
	detach(controlDisk)
	dropExport(iscsiEx.ExportID)
	step("control leg torn down (disk detached, iscsi export withdrawn)")

	// --- leg 2: same volume, same filesystem, NVMe instead -----------------

	nvmeEx, err := addExport(v1, "nvme-tcp")
	if err != nil {
		rep.Legs = append(rep.Legs, nvmeMountLeg{
			Name: "NVMe pre-formatted", Transport: "nvme-tcp",
			Error: "nvme-tcp export: " + err.Error()})
	} else {
		preLeg, preDisk := runLeg("NVMe pre-formatted (same volume)", nvmeEx, false)
		rep.Legs = append(rep.Legs, preLeg)
		detach(preDisk)
		dropExport(nvmeEx.ExportID)
	}

	// --- leg 3: RouterOS formats an NVMe disk itself -----------------------

	v2, err := newVolume("nvmemount-probe-v2")
	if err != nil {
		rep.Error = "creating format-leg volume: " + err.Error()
	} else {
		defer dropVolume(v2)
		fmtEx, err := addExport(v2, "nvme-tcp")
		if err != nil {
			rep.Legs = append(rep.Legs, nvmeMountLeg{
				Name: "NVMe RouterOS-formatted", Transport: "nvme-tcp",
				Error: "nvme-tcp export: " + err.Error()})
		} else {
			fmtLeg, fmtDisk := runLeg("NVMe RouterOS-formatted", fmtEx, true)
			rep.Legs = append(rep.Legs, fmtLeg)
			detach(fmtDisk)
			dropExport(fmtEx.ExportID)
		}
	}

	// --- verdict -----------------------------------------------------------

	get := func(i int) nvmeMountLeg {
		if i < len(rep.Legs) {
			return rep.Legs[i]
		}
		return nvmeMountLeg{}
	}
	control, pre, selfFmt := get(0), get(1), get(2)
	switch {
	case !control.Mounted:
		rep.Verdict = "inconclusive — the iSCSI control leg did not mount either, so the probe cannot attribute anything to NVMe"
	case pre.Mounted && selfFmt.Mounted:
		rep.Verdict = "NVMe IS MOUNTABLE — both NVMe legs mounted; the PVC path can move off iSCSI entirely"
	case pre.Mounted:
		rep.Verdict = "NVMe mounts a pre-formatted volume, but RouterOS could not format one itself — mkube must own mkfs (it already does)"
	case selfFmt.Mounted:
		rep.Verdict = "RouterOS formats and mounts its own NVMe disk but ignores a filesystem it did not write — a probing gap, not a transport limit"
	default:
		rep.Verdict = "NVMe ATTACHES BUT NEVER MOUNTS on this RouterOS — the PVC mount path cannot move off iSCSI without a RouterOS change or device passthrough"
	}
	step("verdict: %s", rep.Verdict)
	return rep
}
