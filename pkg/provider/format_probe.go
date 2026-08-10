package provider

// Format-signature comparison.
//
// mkube formats stormblock volumes with `iscsi-pvc`, a hand-written ext4
// writer — not mkfs.ext4. It clearly produces something RouterOS mounts
// over iSCSI, but "one path mounts it" is not "the same signature and
// layout RouterOS itself writes", and a stricter probe (the NVMe path, for
// instance) may reject what a lenient one accepts.
//
// So: format one volume our way, have RouterOS format another with its own
// `/disk/format-drive`, and dump both superblocks field by field.
//
// POST /api/v1/probes/format

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

type FormatProbeReport struct {
	OursSuperblock     []string `json:"oursSuperblock,omitempty"`
	RouterOSSuperblock []string `json:"routerosSuperblock,omitempty"`
	Differences        []string `json:"differences,omitempty"`
	FormatDriveSyntax  string   `json:"formatDriveSyntax,omitempty"`
	OursMounted        string   `json:"oursMounted"`
	RouterOSMounted    string   `json:"routerosMounted"`
	Steps              []string `json:"steps"`
	Error              string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleFormatProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunFormatProbe(ctx))
}

func (p *MicroKubeProvider) RunFormatProbe(ctx context.Context) *FormatProbeReport {
	rep := &FormatProbeReport{OursMounted: "unknown", RouterOSMounted: "unknown"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("FORMAT-PROBE: " + s)
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

	// A volume, exported over iSCSI, attached — twice: one formatted by us,
	// one handed to RouterOS to format.
	makeVol := func(name string) (string, sbAttach, string, error) {
		var created sbCreateVolumeResp
		if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", map[string]any{
			"name": name, "size_bytes": 512 * 1024 * 1024, "export": true, "protocol": "iscsi",
		}, &created); err != nil {
			return "", sbAttach{}, "", err
		}
		attach, ok := created.attachParams()
		if !ok {
			return created.ID, sbAttach{}, "", fmt.Errorf("no attach parameters")
		}
		diskID, aerr := p.attachStormblockDisk(ctx, attach)
		return created.ID, attach, diskID, aerr
	}
	drop := func(volID, diskID string) {
		if diskID != "" {
			_ = ros.RemoveDisk(ctx, diskID)
		}
		if volID != "" {
			_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/volumes/"+volID+"?force=true", nil, nil)
		}
	}

	dumpSB := func(attach sbAttach) []string {
		portal := attach.Address
		if attach.Port != 0 {
			portal = fmt.Sprintf("%s:%d", attach.Address, attach.Port)
		}
		cmd := exec.CommandContext(ctx, "/usr/local/bin/iscsi-pvc",
			"--url", p.deps.Config.RouterOS.RESTURL,
			"--user", p.deps.Config.RouterOS.User,
			"--password", p.deps.Config.RouterOS.Password,
			"--portal", portal,
			"sb", sbTargetName(attach))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return []string{fmt.Sprintf("superblock read failed: %v: %s", err, strings.TrimSpace(string(out)))}
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return lines
	}

	// ── ours ───────────────────────────────────────────────────────────
	volA, attachA, diskA, err := makeVol("fmt-probe-ours")
	if err != nil {
		rep.Error = fmt.Sprintf("ours: %v", err)
		drop(volA, diskA)
		return rep
	}
	defer drop(volA, diskA)
	if ferr := p.formatStormblockVolume(ctx, sb, volA, attachA, "fmt-probe-ours"); ferr != nil {
		rep.Error = fmt.Sprintf("our format: %v", ferr)
		return rep
	}
	step("formatted by iscsi-pvc")
	_ = ros.RemoveDisk(ctx, diskA)
	if d, aerr := p.attachStormblockDisk(ctx, attachA); aerr == nil {
		diskA = d
		if mp, merr := p.waitForDiskMount(ctx, ros, diskA, 60*time.Second); merr == nil {
			rep.OursMounted = "yes (" + mp + ")"
		} else {
			rep.OursMounted = "no"
		}
	}
	step("ours mounted: %s", rep.OursMounted)
	rep.OursSuperblock = dumpSB(attachA)

	// ── RouterOS's own format ──────────────────────────────────────────
	volB, attachB, diskB, err := makeVol("fmt-probe-ros")
	if err != nil {
		rep.Error = fmt.Sprintf("ros volume: %v", err)
		drop(volB, diskB)
		return rep
	}
	defer drop(volB, diskB)

	// Discover the accepted /disk/format-drive shape.
	variants := []map[string]string{
		{".id": diskB, "file-system": "ext4"},
		{"numbers": diskB, "file-system": "ext4"},
		{".id": diskB, "file-system": "ext4", "label": "fmt-probe-ros"},
	}
	formatted := false
	for i, v := range variants {
		if err := ros.ContainerAddRawTo(ctx, "/disk/format-drive", v); err != nil {
			step("format-drive variant %d rejected: %v", i+1, err)
			continue
		}
		rep.FormatDriveSyntax = fmt.Sprintf("%v", v)
		formatted = true
		step("format-drive ACCEPTED with %v", v)
		break
	}
	if !formatted {
		step("RouterOS would not format the disk itself — cannot compare")
		return rep
	}
	// Formatting is asynchronous; wait for a filesystem to appear.
	if mp, merr := p.waitForDiskMount(ctx, ros, diskB, 3*time.Minute); merr == nil {
		rep.RouterOSMounted = "yes (" + mp + ")"
	} else {
		rep.RouterOSMounted = "no"
	}
	step("routeros-formatted mounted: %s", rep.RouterOSMounted)
	rep.RouterOSSuperblock = dumpSB(attachB)

	// ── diff ───────────────────────────────────────────────────────────
	ours := map[string]string{}
	for _, l := range rep.OursSuperblock {
		if k, v, ok := strings.Cut(l, " "); ok {
			ours[k] = strings.TrimSpace(v)
		}
	}
	for _, l := range rep.RouterOSSuperblock {
		k, v, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if o, seen := ours[k]; seen && o != v {
			rep.Differences = append(rep.Differences, fmt.Sprintf("%s: ours=%s routeros=%s", k, o, v))
		}
	}
	if len(rep.Differences) == 0 {
		step("superblocks agree on every field compared")
	} else {
		for _, d := range rep.Differences {
			step("DIFF %s", d)
		}
	}
	return rep
}

// handleInspect reports what a mounted clone actually looks like — disk
// rows, this container's mount entries, and targeted existence checks.
//
// Deliberately avoids ListDirectory and unfiltered ListMounts on paths
// outside raid1: both fall back to a full /file or /container/mounts print,
// which takes minutes on this box (the very thing the local-file-ops work
// removed from the hot paths).
//
// GET /api/v1/probes/inspect?path=iscsi15&container=gt_cow-test_cow-test
func (p *MicroKubeProvider) handleInspect(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
	container := r.URL.Query().Get("container")
	ros := p.getRouterOSClient()
	if ros == nil {
		http.Error(w, "RouterOS backend required", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	out := map[string]any{"path": path}

	if disks, err := ros.ListDisks(ctx); err == nil {
		var rows []string
		for i := range disks {
			d := &disks[i]
			if d.MountPoint != "" || d.Type == "iscsi" || d.Type == "nvme-tcp" {
				rows = append(rows, fmt.Sprintf("id=%s slot=%s type=%s fs=%s mount=%s iqn=%s",
					d.ID, d.Slot, d.Type, d.Filesystem, d.MountPoint, d.ISCSIIQN))
			}
		}
		out["disks"] = rows
	} else {
		out["disksError"] = err.Error()
	}

	if container != "" {
		if mounts, err := ros.ListMountsByList(ctx, container); err == nil {
			var rows []string
			for _, m := range mounts {
				rows = append(rows, fmt.Sprintf("src=%s dst=%s", m.Src, m.Dst))
			}
			out["mounts"] = rows
		} else {
			out["mountsError"] = err.Error()
		}
	}

	// Targeted, name-filtered existence checks only.
	checks := map[string]any{}
	if path != "" {
		for _, sub := range []string{"", "/rootfs", "/rootfs/bin", "/rootfs/usr"} {
			target := path + sub
			exists, err := ros.FileExists(ctx, target)
			if err != nil {
				checks[target] = "error: " + err.Error()
			} else {
				checks[target] = exists
			}
		}
	}
	out["exists"] = checks
	podWriteJSON(w, http.StatusOK, out)
}
