package provider

// Dump the ext4 superblock of a stormblock volume.
//
// Worth having as a first-class endpoint rather than a one-off: when a
// volume mounts but behaves wrongly, the superblock is where the answer
// lives, and the interesting failures are all feature bits. ext4 mounts
// READ-ONLY when feature_ro_compat carries a bit the kernel does not
// implement — so a volume that mounts cleanly and then refuses every write
// looks like a storage fault and is actually a mkfs option.
//
// POST /api/v1/probes/sb?volume=<volume-id>

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type sbSuperblockReport struct {
	VolumeID string   `json:"volumeId"`
	Target   string   `json:"target,omitempty"`
	Fields   []string `json:"fields,omitempty"`
	Error    string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleSuperblockProbe(w http.ResponseWriter, r *http.Request) {
	volumeID := r.URL.Query().Get("volume")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunSuperblockProbe(ctx, volumeID))
}

func (p *MicroKubeProvider) RunSuperblockProbe(ctx context.Context, volumeID string) *sbSuperblockReport {
	rep := &sbSuperblockReport{VolumeID: volumeID}
	if volumeID == "" {
		rep.Error = "pass ?volume=<volume-id>"
		return rep
	}
	sb, err := p.newStormblockClient()
	if err != nil {
		rep.Error = err.Error()
		return rep
	}

	// Reuse the volume's existing export when it has one; otherwise add a
	// temporary one and withdraw it again, so this never disturbs a volume
	// a pod is using.
	// stormblockmk has no GET on a single volume (405), so find it in the
	// listing.
	var list struct {
		Items []struct {
			ID     string    `json:"id"`
			Name   string    `json:"name"`
			Export *sbExport `json:"export"`
		} `json:"items"`
	}
	if err := sb.do(ctx, http.MethodGet, "/mk/v1/volumes", nil, &list); err != nil {
		rep.Error = "listing volumes: " + err.Error()
		return rep
	}
	var export *sbExport
	found := false
	for _, v := range list.Items {
		if v.ID == volumeID {
			found, export = true, v.Export
			break
		}
	}
	if !found {
		rep.Error = fmt.Sprintf("volume %s not found", volumeID)
		return rep
	}

	var attach sbAttach
	temporary := ""
	if export != nil && export.Attach.Address != "" {
		attach = export.Attach
		if attach.Transport == "" {
			attach.Transport = export.Protocol
		}
	} else {
		var ex sbExport
		if err := sb.do(ctx, http.MethodPost, "/mk/v1/exports",
			map[string]any{"volume_id": volumeID, "protocol": p.sbProtocol()}, &ex); err != nil {
			rep.Error = "exporting volume: " + err.Error()
			return rep
		}
		temporary = ex.ExportID
		attach = ex.Attach
		if attach.Transport == "" {
			attach.Transport = ex.Protocol
		}
	}
	defer func() {
		if temporary != "" {
			_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/exports/"+temporary, nil, nil)
		}
	}()

	if t, err := sbToolTarget(attach); err == nil {
		rep.Target = t
	}
	out, err := p.runVolumeTool(ctx, attach, "sb")
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rep.Fields = append(rep.Fields, line)
		}
	}
	if len(rep.Fields) == 0 {
		rep.Error = fmt.Sprintf("no superblock output for volume %s", volumeID)
	}
	return rep
}
