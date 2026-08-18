package provider

// Does the storage actually store bytes?
//
// Every conclusion so far has trusted `allocated_bytes` as evidence that
// data landed. This writes a KNOWN pattern (each block stamped with its own
// LBA) and reads it back at four points, so each layer is tested separately:
//
//	1. same session            — the write path at all
//	2. fresh session           — persistence across an iSCSI login
//	3. re-export + re-attach   — persistence across a detach
//	4. clone of a sealed template — whether a clone carries the bytes
//
// Step 4 is the one that matters for CoW: it writes the pattern well past
// the filesystem's own metadata, so a clone either reproduces those blocks
// or it does not, independent of ext4.
//
// POST /api/v1/probes/datapath

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type DataPathReport struct {
	SameSession  string   `json:"sameSession"`
	FreshSession string   `json:"freshSession"`
	AfterDetach  string   `json:"afterDetach"`
	FromClone    string   `json:"fromClone"`
	Steps        []string `json:"steps"`
	Error        string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleDataPathProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunDataPathProbe(ctx))
}

func (p *MicroKubeProvider) RunDataPathProbe(ctx context.Context) *DataPathReport {
	rep := &DataPathReport{SameSession: "not-run", FreshSession: "not-run", AfterDetach: "not-run", FromClone: "not-run"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("DATAPATH: " + s)
	}
	sb, err := p.newStormblockClient()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}

	// nvme-pvc pattern helper.
	pattern := func(attach sbAttach, mode string) (string, error) {
		out, err := p.runVolumeTool(ctx, attach, "pattern",
			"--mode", mode, "--lba", "8192", "--count", "64")
		txt := strings.TrimSpace(out)
		for _, l := range strings.Split(txt, "\n") {
			if strings.HasPrefix(l, "wrote") || strings.HasPrefix(l, "checked") {
				txt = l
			}
		}
		if err != nil {
			return txt, fmt.Errorf("%w: %s", err, txt)
		}
		return txt, nil
	}

	// ── A volume, exported over iSCSI ──────────────────────────────────
	var created sbCreateVolumeResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", map[string]any{
		"name": "datapath-probe", "size_bytes": 512 * 1024 * 1024, "export": true, "protocol": p.sbProtocol(),
	}, &created); err != nil {
		rep.Error = fmt.Sprintf("creating volume: %v", err)
		return rep
	}
	defer func() {
		_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil)
	}()
	attach, ok := created.attachParams()
	if !ok {
		rep.Error = "no attach parameters"
		return rep
	}
	step("volume %s exported at %s:%d", created.ID[:8], attach.Address, attach.Port)

	// 1 + 2. Write (with flush), then verify in a brand-new session.
	if out, err := pattern(attach, "write"); err != nil {
		rep.Error = fmt.Sprintf("writing pattern: %v", err)
		return rep
	} else {
		step("write: %s", out)
	}
	rep.SameSession = "written"
	if out, err := pattern(attach, "check"); err != nil {
		rep.FreshSession = "FAILED: " + out
		step("fresh-session verify FAILED: %s", out)
	} else {
		rep.FreshSession = out
		step("fresh-session verify: %s", out)
	}

	// 3. Withdraw the export, re-export, verify again — does a detach lose it?
	if created.Export != nil && created.Export.ExportID != "" {
		_ = sb.do(ctx, http.MethodDelete, "/mk/v1/exports/"+created.Export.ExportID, nil, nil)
	}
	var re sbExport
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/exports",
		map[string]any{"volume_id": created.ID, "protocol": p.sbProtocol()}, &re); err != nil {
		rep.AfterDetach = "re-export failed: " + err.Error()
	} else {
		a2 := re.Attach
		if a2.Transport == "" {
			a2.Transport = re.Protocol
		}
		if out, err := pattern(a2, "check"); err != nil {
			rep.AfterDetach = "FAILED: " + out
			step("after-detach verify FAILED: %s", out)
		} else {
			rep.AfterDetach = out
			step("after-detach verify: %s", out)
		}
		attach = a2
	}

	// 4. THE decisive leg: known bytes → seal → clone → read them back.
	//
	// stormblockmk templates create their own volume, so the pattern goes
	// into the TEMPLATE's raw volume, past the filesystem's metadata. The
	// clone either reproduces those exact blocks or it does not, and that
	// is independent of ext4, RouterOS, and every other layer we have been
	// arguing about.
	tplRow, tAttach, tplExportID, terr := p.createTemplateForFormatting(
		ctx, sb, "datapath-tpl", 512*1024*1024)
	if terr != nil {
		rep.FromClone = "template create failed: " + terr.Error()
		return rep
	}
	tmpl := sbCreateTemplateResp{Template: tplRow}
	defer func() {
		if tplExportID != "" {
			_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/exports/"+tplExportID, nil, nil)
		}
	}()
	defer func() {
		_ = sb.do(context.Background(), http.MethodDelete, "/api/v1/fstemplates/"+tmpl.Template.ID+"?force=true", nil, nil)
	}()

	step("template %s raw volume exported at %s:%d", tmpl.Template.Name, tAttach.Address, tAttach.Port)

	// Format so the seal guard has a valid, clean ext4 to verify...
	if err := p.formatStormblockVolume(ctx, sb, tmpl.Template.RawVolumeID, tAttach, "datapath-tpl"); err != nil {
		rep.FromClone = "formatting template volume failed: " + err.Error()
		return rep
	}
	// ...then stamp the pattern well past the filesystem metadata.
	if out, err := pattern(tAttach, "write"); err != nil {
		rep.FromClone = "writing pattern to template volume failed: " + out
		return rep
	} else {
		step("pattern written into the template volume: %s", out)
	}
	if out, err := pattern(tAttach, "check"); err != nil {
		rep.FromClone = "pattern not readable on the template volume itself: " + out
		step("template volume verify FAILED before sealing: %s", out)
		return rep
	} else {
		step("template volume verify before sealing: %s", out)
	}

	if err := sb.do(ctx, http.MethodPost, "/api/v1/fstemplates/"+tmpl.Template.ID+"/seal", nil, nil); err != nil {
		rep.FromClone = "seal failed: " + err.Error()
		step("seal failed: %v", err)
		return rep
	}
	step("template sealed (guard verified the filesystem)")

	var clone sbCreateVolumeResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", map[string]any{
		"name": "datapath-clone", "from_template": tmpl.Template.Name, "export": true, "protocol": p.sbProtocol(),
	}, &clone); err != nil {
		rep.FromClone = "clone failed: " + err.Error()
		return rep
	}
	defer func() {
		_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/volumes/"+clone.ID+"?force=true", nil, nil)
	}()
	cAttach, ok2 := clone.attachParams()
	if !ok2 {
		rep.FromClone = "clone returned no attach parameters"
		return rep
	}
	step("clone %s exported at %s:%d", clone.ID[:8], cAttach.Address, cAttach.Port)

	if out, err := pattern(cAttach, "check"); err != nil {
		rep.FromClone = "CLONE LOST THE DATA: " + out
		step("clone verify FAILED: %s", out)
	} else {
		rep.FromClone = out
		step("clone verify: %s", out)
	}

	return rep
}
