package provider

// Choosing which pre-formatted template a PVC is cloned from.
//
// stormblockmk ships a size ladder of ready-made, pre-formatted, empty
// filesystems — pvc-ext4-1m through pvc-ext4-10240m as of 0.5.0. A PVC is a
// CoW clone of whichever one fits, so provisioning never formats: the
// filesystem exists before the volume does and the clone costs metadata
// rather than a full mkfs.
//
// A single configured template name cannot serve claims of different sizes,
// which is why this picks per claim: the SMALLEST ready template that is at
// least as large as the request. Picking the smallest keeps a 1 MiB claim
// from cloning a 10 GiB filesystem — the clone is thin, but its filesystem
// geometry (and therefore its apparent size) is the template's, not the
// claim's.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// sbTemplateRow is an fstemplate as listed by stormblockmk.
type sbTemplateRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	FS        string `json:"fs"`
	State     string `json:"state"`
	SizeBytes int64  `json:"size_bytes"`
}

type sbTemplateList struct {
	Count int             `json:"count"`
	Items []sbTemplateRow `json:"items"`
}

// isPVCTemplate reports whether a template is an empty filesystem meant for
// PVCs, as opposed to a container-image golden.
//
// Image goldens are named img-<digest12> and carry a rootfs; cloning one for
// a PVC would hand the claim somebody else's files.
func isPVCTemplate(t sbTemplateRow) bool {
	return !strings.HasPrefix(t.Name, "img-")
}

// pickStormblockTemplate returns the name of the template to clone for a
// claim of sizeBytes.
//
// An explicitly configured template always wins — an operator naming one is
// making a deliberate choice, and silently overriding it would be worse than
// cloning something slightly too large.
func (p *MicroKubeProvider) pickStormblockTemplate(ctx context.Context, sb *sbClient, sizeBytes int64) (string, error) {
	if configured := p.deps.Config.Storage.Stormblock.Template; configured != "" {
		return configured, nil
	}

	var list sbTemplateList
	if err := sb.do(ctx, http.MethodGet, "/api/v1/fstemplates", nil, &list); err != nil {
		return "", fmt.Errorf("listing filesystem templates: %w", err)
	}

	var usable []sbTemplateRow
	var largest int64
	for _, t := range list.Items {
		if t.State != "ready" || !isPVCTemplate(t) {
			continue
		}
		if t.SizeBytes > largest {
			largest = t.SizeBytes
		}
		if t.SizeBytes >= sizeBytes {
			usable = append(usable, t)
		}
	}
	if len(usable) == 0 {
		if largest == 0 {
			return "", fmt.Errorf("stormblockmk has no ready PVC filesystem templates; " +
				"a PVC is a clone of a pre-formatted template, so one must exist before a volume can be provisioned")
		}
		return "", fmt.Errorf("no ready template is large enough for %d bytes (largest is %d bytes)",
			sizeBytes, largest)
	}
	// Smallest that fits; name as the tie-break so the choice is stable
	// across calls rather than dependent on listing order.
	sort.Slice(usable, func(i, j int) bool {
		if usable[i].SizeBytes != usable[j].SizeBytes {
			return usable[i].SizeBytes < usable[j].SizeBytes
		}
		return usable[i].Name < usable[j].Name
	})
	return usable[0].Name, nil
}
