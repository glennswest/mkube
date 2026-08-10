package provider

// NVMe-TCP capability probe.
//
// Two independent questions block the stormblock transport switchover, and
// this answers both without touching production volumes:
//
//  1. Does the RUNNING stormblockmk honor `protocol` on export creation?
//     (v0.3.0+ does; v0.2.0 silently exports iSCSI.)
//  2. Does the RouterOS initiator understand `/disk add type=nvme-tcp` at
//     all? This has never been exercised on rose1 — the spec expects
//     ROS ≥ 7.9 — and it is the real risk in the switchover, independent
//     of which stormblockmk build is deployed.
//
// Question 2 is answered even when question 1 says "no": the probe issues a
// deliberately unreachable nvme-tcp attach and classifies the device's
// complaint. "invalid value for argument type" means the initiator has no
// NVMe support; a connect/timeout/no-target style error means the syntax
// was accepted and only the target was missing — support exists.
//
// POST /api/v1/probes/nvme

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/glennswest/mkube/pkg/routeros"
)

type NVMeProbeReport struct {
	RouterOSVersion string   `json:"routerosVersion,omitempty"`
	StormblockmkVer string   `json:"stormblockmkVersion,omitempty"`
	HonorsProtocol  string   `json:"honorsProtocol"`  // yes | no | unknown
	InitiatorNVMe   string   `json:"initiatorNvmeTcp"` // supported | unsupported | unknown
	EndToEnd        string   `json:"endToEnd"`         // attached | not-attempted | failed
	Steps           []string `json:"steps"`
	Error           string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleNVMeProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunNVMeProbe(ctx))
}

func (p *MicroKubeProvider) RunNVMeProbe(ctx context.Context) *NVMeProbeReport {
	rep := &NVMeProbeReport{HonorsProtocol: "unknown", InitiatorNVMe: "unknown", EndToEnd: "not-attempted"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("NVME-PROBE: " + s)
	}

	ros := p.getRouterOSClient()
	if ros == nil {
		rep.Error = "probe requires the RouterOS backend"
		return rep
	}
	if res, err := ros.GetSystemResource(ctx); err == nil {
		rep.RouterOSVersion = res.Version
		step("RouterOS %s on %s (%s)", res.Version, res.BoardName, res.Architecture)
	}

	sb, err := p.newStormblockClient()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := sb.do(ctx, http.MethodGet, "/mk/v1/health", nil, &health); err == nil {
		rep.StormblockmkVer = health.Version
		step("stormblockmk %s", health.Version)
	}

	// ── 1. Ask for an NVMe export on a throwaway volume ──────────────
	name := "nvme-probe"
	// Clear any leftover from an earlier run.
	p.nvmeProbePurge(ctx, sb, name)

	var created sbCreateVolumeResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", map[string]any{
		"name":       name,
		"size_bytes": 64 * 1024 * 1024,
		"export":     true,
		"protocol":   "nvme-tcp",
	}, &created); err != nil {
		rep.Error = fmt.Sprintf("creating probe volume: %v", err)
		return rep
	}
	defer p.nvmeProbePurge(ctx, sb, name)

	attach, ok := created.attachParams()
	gotProto := ""
	if created.Export != nil {
		gotProto = created.Export.Protocol
	}
	switch {
	case !ok:
		step("volume created but no attach block returned")
	case gotProto == "nvme-tcp" || attach.NQN != "":
		rep.HonorsProtocol = "yes"
		step("stormblockmk honored protocol=nvme-tcp → nqn=%s addr=%s:%d", attach.NQN, attach.Address, attach.Port)
	default:
		rep.HonorsProtocol = "no"
		step("stormblockmk ignored protocol and exported %q (iqn=%s) — pre-v0.3.0 build", gotProto, attach.IQN)
	}

	// ── 2. Real end-to-end attach when we actually have an NVMe export ─
	if rep.HonorsProtocol == "yes" {
		diskID, aerr := p.attachStormblockDisk(ctx, attach)
		if aerr != nil {
			rep.EndToEnd = "failed"
			rep.InitiatorNVMe = classifyNVMeError(aerr)
			step("nvme-tcp attach FAILED: %v", aerr)
		} else {
			rep.EndToEnd = "attached"
			rep.InitiatorNVMe = "supported"
			step("nvme-tcp attach SUCCEEDED — disk %s", diskID)
			if disk, derr := ros.GetISCSIDisk(ctx, diskID); derr == nil {
				step("disk row: slot=%s type=%s nqn=%s addr=%s", disk.Slot, disk.Type, disk.NVMeTCPNQN, disk.NVMeTCPAddress)
			}
			// Device-passthrough feasibility: can the attached target be
			// handed to a container as a DEVICE instead of a mounted path?
			// Two prerequisites, both checked here.
			if disk, derr := ros.GetISCSIDisk(ctx, diskID); derr == nil {
				if disk.BlockDevice != "" {
					step("PASSTHROUGH 1/2: disk exposes block-device %q", disk.BlockDevice)
				} else {
					step("PASSTHROUGH 1/2: disk row exposes no block-device path")
				}
			}
			step("PASSTHROUGH 2/2: %s", p.probeContainerDeviceArg(ctx, ros))
			// Devices come from /system/hardware (passthrough shipped in 7.20)
			// — see docs/routeros-container-changes.md.
			for _, menu := range []string{"/system/hardware", "/system/hardware/device", "/container/config"} {
				rows, merr := ros.ListRaw(ctx, menu)
				if merr != nil {
					step("menu %s: %v", menu, merr)
					continue
				}
				keys := map[string]bool{}
				for _, r := range rows {
					for k := range r {
						keys[k] = true
					}
				}
				var ks []string
				for k := range keys {
					ks = append(ks, k)
				}
				sort.Strings(ks)
				step("menu %s EXISTS — %d row(s), fields: %v", menu, len(rows), ks)
				for i, r := range rows {
					if i >= 3 {
						break
					}
					step("   row: %v", r)
				}
			}
			_ = ros.RemoveDisk(ctx, diskID)
			step("probe disk detached")
		}
		return rep
	}

	// ── 3. Syntax-only capability check ──────────────────────────────
	// The target does not exist; we only care WHICH complaint comes back.
	step("no NVMe export available — testing initiator syntax against a deliberately absent target")
	_, serr := ros.AttachNetworkDisk(ctx, "nvme-tcp", "192.168.200.21:4420", "nqn.2026-08.lo.gt:nvme-probe-absent")
	if serr == nil {
		rep.InitiatorNVMe = "supported"
		step("initiator accepted type=nvme-tcp (and even created a disk row — cleaning up)")
		if d, ferr := ros.FindNetworkDisk(ctx, "nvme-tcp", "192.168.200.21", "nqn.2026-08.lo.gt:nvme-probe-absent"); ferr == nil && d != nil {
			_ = ros.RemoveDisk(ctx, d.ID)
		}
		return rep
	}
	rep.InitiatorNVMe = classifyNVMeError(serr)
	step("initiator response: %v → %s", serr, rep.InitiatorNVMe)
	return rep
}

// classifyNVMeError decides whether a failed nvme-tcp attach means the
// initiator lacks NVMe support, or merely that the target was unreachable.
func classifyNVMeError(err error) string {
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "invalid value for argument type"),
		strings.Contains(e, "unknown argument"),
		strings.Contains(e, "no such argument"),
		strings.Contains(e, "not supported"):
		return "unsupported"
	case strings.Contains(e, "could not find the disk entry"),
		strings.Contains(e, "timeout"), strings.Contains(e, "timed out"),
		strings.Contains(e, "connect"), strings.Contains(e, "refused"),
		strings.Contains(e, "no route"), strings.Contains(e, "unreachable"),
		strings.Contains(e, "no such target"), strings.Contains(e, "failure:"):
		return "supported"
	default:
		return "unknown"
	}
}

// nvmeProbePurge deletes any probe volume left behind by an earlier run.
func (p *MicroKubeProvider) nvmeProbePurge(ctx context.Context, sb *sbClient, name string) {
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := sb.do(ctx, http.MethodGet, "/mk/v1/volumes", nil, &list); err != nil {
		return
	}
	ros := p.getRouterOSClient()
	for _, v := range list.Items {
		if v.Name != name {
			continue
		}
		if ros != nil {
			if all, aerr := ros.ListDisks(ctx); aerr == nil {
				for i := range all {
					d := &all[i]
					if strings.Contains(d.ISCSIIQN, v.ID) || strings.Contains(d.NVMeTCPNQN, v.ID) {
						_ = ros.RemoveDisk(ctx, d.ID)
					}
				}
			}
		}
		_ = sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+v.ID+"?force=true", nil, nil)
	}
}

var _ = routeros.FileDisk{} // keep the routeros import anchored


// probeContainerDeviceArg asks the device whether /container/add accepts a
// `devices=` argument at all. An "unknown/no such argument" complaint means
// this RouterOS build has no container device passthrough; anything else
// (invalid value, missing image, …) means the argument is recognised and
// passthrough is worth designing against on THIS version.
func (p *MicroKubeProvider) probeContainerDeviceArg(ctx context.Context, ros *routeros.Client) string {
	err := ros.ContainerAddRaw(ctx, map[string]string{
		"name":      "gt_devprobe_devprobe",
		"root-dir":  "raid1/images/devprobe-nonexistent",
		"devices":   "/dev/null",
		"interface": "veth_gt_cowprobe_0",
	})
	if err == nil {
		// Should not happen (no image source), but clean up if it did.
		if ct, gerr := ros.GetContainer(ctx, "gt_devprobe_devprobe"); gerr == nil {
			_ = ros.RemoveContainer(ctx, ct.ID)
		}
		return "container/add ACCEPTED a devices= argument — passthrough exists on this build"
	}
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "unknown argument"), strings.Contains(e, "no such argument"),
		strings.Contains(e, "unrecognized"), strings.Contains(e, "invalid argument"):
		return fmt.Sprintf("container/add REJECTED the argument itself — no device passthrough on this build (%v)", err)
	default:
		return fmt.Sprintf("container/add recognised devices= and failed later — passthrough likely supported (%v)", err)
	}
}
