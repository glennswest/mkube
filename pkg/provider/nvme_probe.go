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
