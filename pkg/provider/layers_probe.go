package provider

// Layer-store capability probe.
//
// RouterOS has had an overlayfs layers option since 7.11 and a configurable
// layer store (`/container/config layer-dir`) since 7.21. If it caches
// extracted layers and reuses them, a second container from the same image
// is an overlay mount rather than an untar — which would make the image
// half of the CoW catalog unnecessary and leave stormblock volumes carrying
// only writable data.
//
// mkube has never benefited from this because it FLATTENS every image into
// a single docker-save layer before handing it to `file=`. This probe skips
// mkube's pipeline entirely and uses `remote-image=`, letting RouterOS pull
// real layers from the local registry, then measures a create, a remove and
// a second create of the same image.
//
// POST /api/v1/probes/layers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/glennswest/mkube/pkg/routeros"
)

type LayersProbeReport struct {
	RegistryURL  string   `json:"registryUrl,omitempty"`
	LayerDir     string   `json:"layerDir"`
	FirstCreate  string   `json:"firstCreate"`
	SecondCreate string   `json:"secondCreate"`
	Verdict      string   `json:"verdict"`
	Steps        []string `json:"steps"`
	Error        string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleLayersProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunLayersProbe(ctx))
}

func (p *MicroKubeProvider) RunLayersProbe(ctx context.Context) *LayersProbeReport {
	rep := &LayersProbeReport{Verdict: "unknown"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("LAYERS-PROBE: " + s)
	}
	ros := p.getRouterOSClient()
	if ros == nil {
		rep.Error = "probe requires the RouterOS backend"
		return rep
	}

	if rows, err := ros.ListRaw(ctx, "/container/config"); err == nil && len(rows) > 0 {
		if v, ok := rows[0]["layer-dir"].(string); ok {
			rep.LayerDir = v
		}
		if v, ok := rows[0]["registry-url"].(string); ok {
			rep.RegistryURL = v
		}
		step("container config: layer-dir=%q registry-url=%q", rep.LayerDir, rep.RegistryURL)
	}

	unguard := p.cowProbeGuardPod()
	defer unguard()

	veth := "veth_gt_cowprobe_0"
	if _, _, _, err := p.deps.NetworkMgr.AllocateInterface(ctx, veth, "cowprobe.cowprobe", "gt", ""); err != nil {
		rep.Error = fmt.Sprintf("allocating probe veth: %v", err)
		return rep
	}
	defer func() { _ = p.deps.NetworkMgr.ReleaseInterface(ctx, veth) }()

	const image = "192.168.200.3:5000/nats:edge"
	name := cowProbeContainer

	run := func(label string) (time.Duration, error) {
		rootDir := "raid1/images/layerprobe-" + label
		_ = ros.RemoveDirectory(ctx, rootDir)
		start := time.Now()
		if err := ros.CreateContainer(ctx, routeros.ContainerSpec{
			Name:        name,
			Interface:   veth,
			RootDir:     rootDir,
			RemoteImage: image,
			Logging:     "yes",
			StartOnBoot: "no",
		}); err != nil {
			return 0, err
		}
		// Wait until RouterOS finishes pulling+extracting (container settles).
		deadline := time.Now().Add(6 * time.Minute)
		for time.Now().Before(deadline) {
			ros.InvalidateContainerCache()
			ct, err := ros.GetContainer(ctx, name)
			if err == nil && (ct.IsStopped() || ct.IsRunning()) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		elapsed := time.Since(start)
		ros.InvalidateContainerCache()
		if ct, err := ros.GetContainer(ctx, name); err == nil {
			_ = ros.StopContainer(ctx, ct.ID)
			for i := 0; i < 6; i++ {
				time.Sleep(2 * time.Second)
				if ros.RemoveContainer(ctx, ct.ID) == nil {
					break
				}
			}
		}
		_ = ros.RemoveDirectory(ctx, rootDir)
		return elapsed, nil
	}

	d1, err := run("a")
	if err != nil {
		rep.Error = fmt.Sprintf("first create (remote-image=%s): %v", image, err)
		step("first create FAILED: %v", err)
		return rep
	}
	rep.FirstCreate = d1.Round(time.Millisecond).String()
	step("first create from %s: %s", image, rep.FirstCreate)

	d2, err := run("b")
	if err != nil {
		rep.Error = fmt.Sprintf("second create: %v", err)
		return rep
	}
	rep.SecondCreate = d2.Round(time.Millisecond).String()
	step("second create (same image): %s", rep.SecondCreate)

	switch {
	case d2 < d1/3:
		rep.Verdict = "layers-reused"
		step("second create was %.1fx faster — RouterOS reuses cached layers", float64(d1)/float64(d2))
	case d2 < d1*4/5:
		rep.Verdict = "partial-reuse"
	default:
		rep.Verdict = "no-reuse"
		step("no meaningful speedup — RouterOS re-extracts per container")
	}
	if rows, err := ros.ListRaw(ctx, "/container/config"); err == nil && len(rows) > 0 {
		step("layer-dir after runs: %v", rows[0]["layer-dir"])
	}
	return rep
}
