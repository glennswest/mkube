package provider

// layer-dir on a CoW clone.
//
// The question: can a container's LAYER STORE live on a mounted stormblock
// clone, so the rootfs materializes from layers RouterOS already has
// instead of from a tarball we hand it? If yes, the 5 KB stub disappears
// and with it the last per-pod tarball — the container references an image
// by name, finds its layers already present on the clone, and mounts.
//
// This is the RouterOS form of the hybrid in stormblock-registry's
// docs/vs-overlayfs.md: "shared read-only clone as overlay lowerdir".
//
// Sequence:
//   1. provision + mount a stormblock volume (the clone stand-in)
//   2. point a container's layer-dir at it — per-container first, since
//      changing the GLOBAL setting would send production layers onto a
//      volume this probe later deletes
//   3. create from remote-image, time it, list what landed in the store
//   4. remove, create again, time it — a fast second create means layers
//      were reused FROM THE CLONE
//   5. restore everything
//
// POST /api/v1/probes/layerdir

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type LayerDirProbeReport struct {
	PerContainerLayerDir string   `json:"perContainerLayerDir"` // supported | unsupported | unknown
	MountPoint           string   `json:"mountPoint,omitempty"`
	FirstCreate          string   `json:"firstCreate,omitempty"`
	SecondCreate         string   `json:"secondCreate,omitempty"`
	LayerStoreContents   []string `json:"layerStoreContents,omitempty"`
	StubStillNeeded      string   `json:"stubStillNeeded"` // yes | no | unknown
	Verdict              string   `json:"verdict"`
	Steps                []string `json:"steps"`
	Error                string   `json:"error,omitempty"`
}

func (p *MicroKubeProvider) handleLayerDirProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	podWriteJSON(w, http.StatusOK, p.RunLayerDirProbe(ctx))
}

func (p *MicroKubeProvider) RunLayerDirProbe(ctx context.Context) *LayerDirProbeReport {
	rep := &LayerDirProbeReport{PerContainerLayerDir: "unknown", StubStillNeeded: "unknown", Verdict: "unknown"}
	step := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("LAYERDIR-PROBE: " + s)
	}
	ros := p.getRouterOSClient()
	if ros == nil {
		rep.Error = "probe requires the RouterOS backend"
		return rep
	}

	unguard := p.cowProbeGuardPod()
	defer unguard()

	// ── 1. A stormblock volume, formatted and mounted: the clone stand-in.
	sc := pvcTypeStormblock
	pvcKey := cowProbeNamespace + "/layerdir-probe"
	pvc, ok := p.pvcs.Get(pvcKey)
	if !ok {
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "layerdir-probe", Namespace: cowProbeNamespace, CreationTimestamp: metav1.Now()},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &sc,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("512Mi")},
				},
			},
		}
		p.pvcs.Set(pvcKey, pvc)
	}
	mountPoint, err := p.provisionStormblockPVC(ctx, pvc)
	if err != nil {
		rep.Error = fmt.Sprintf("provisioning clone volume: %v", err)
		return rep
	}
	defer func() {
		if derr := p.deprovisionStormblockPVC(context.Background(), pvc); derr != nil {
			p.deps.Logger.Warnw("LAYERDIR-PROBE: volume cleanup", "error", derr)
		}
		p.pvcs.Delete(pvcKey)
	}()
	rep.MountPoint = mountPoint
	step("clone volume mounted at %s", mountPoint)

	// Discovery store lives on raid1, not on the clone: raid1 is bind-mounted
	// into mkube, so the tree can be walked LOCALLY with sizes and dotfiles
	// visible — and the hidden bookkeeping is the thing we are actually
	// after. (The clone is still provisioned above: it is the destination
	// once we know what a complete layer looks like.)
	layerStore := "raid1/layerprobe-store"
	_ = ros.RemoveDirectory(ctx, layerStore)
	if err := ros.EnsureDirectory(ctx, layerStore); err != nil {
		step("could not create %s: %v", layerStore, err)
	}
	defer func() { _ = ros.RemoveDirectory(context.Background(), layerStore) }()

	veth := "veth_gt_cowprobe_0"
	if _, _, _, verr := p.deps.NetworkMgr.AllocateInterface(ctx, veth, "cowprobe.cowprobe", "gt", ""); verr != nil {
		rep.Error = fmt.Sprintf("allocating probe veth: %v", verr)
		return rep
	}
	defer func() { _ = p.deps.NetworkMgr.ReleaseInterface(ctx, veth) }()

	// Repo:tag only — RouterOS resolves it against /container/config
	// registry-url (http://192.168.200.3:5000). A fully-qualified ref sends
	// the pull down the TLS path against our private CA instead.
	const image = "nats:edge"
	name := cowProbeContainer

	remove := func() {
		ros.InvalidateContainerCache()
		if ct, err := ros.GetContainer(ctx, name); err == nil {
			_ = ros.StopContainer(ctx, ct.ID)
			for i := 0; i < 8; i++ {
				time.Sleep(2 * time.Second)
				if ros.RemoveContainer(ctx, ct.ID) == nil {
					break
				}
			}
		}
		ros.InvalidateContainerCache()
	}

	// ── 2. Per-container layer-dir on the clone.
	create := func(rootDir string) (time.Duration, error) {
		_ = ros.RemoveDirectory(ctx, rootDir)
		start := time.Now()
		err := ros.ContainerAddRaw(ctx, map[string]string{
			"name":          name,
			"interface":     veth,
			"root-dir":      rootDir,
			"remote-image":  image,
			"layer-dir":     layerStore,
			"logging":       "yes",
			"start-on-boot": "no",
		})
		if err != nil {
			return 0, err
		}
		settled := false
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			ros.InvalidateContainerCache()
			if ct, gerr := ros.GetContainer(ctx, name); gerr == nil && (ct.IsStopped() || ct.IsRunning()) {
				settled = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !settled {
			// RouterOS reports pull/extract failures only in its own log.
			if entries, lerr := ros.TailLog(ctx, 40); lerr == nil {
				for _, e := range entries {
					if strings.Contains(e.Topics, "container") || strings.Contains(e.Message, "cowprobe") ||
						strings.Contains(strings.ToLower(e.Message), "pull") || strings.Contains(strings.ToLower(e.Message), "registry") {
						step("   device log: %s %s", e.Time, e.Message)
					}
				}
			}
			return 0, fmt.Errorf("container never settled within 3m (pull or extract failed — see device log above)")
		}
		elapsed := time.Since(start)

		// The decisive measurement: is the rootfs COPIED per container, or
		// overlay-mounted from the shared store? root-dir holding a full
		// image means copy-per-container (download once, extract each time);
		// a near-empty root-dir beside a populated store means the layers are
		// mounted, which is the zero-copy model.
		rootBytes, _ := ros.DirectoryDiskUsage(ctx, rootDir)
		storeBytes, _ := ros.DirectoryDiskUsage(ctx, layerStore)
		step("after create: root-dir %s = %.1f MB | layer store = %.1f MB",
			rootDir, float64(rootBytes)/(1024*1024), float64(storeBytes)/(1024*1024))
		if rootBytes < storeBytes/4 && storeBytes > 0 {
			step("   → root-dir is a fraction of the store: layers look MOUNTED, not copied")
		} else if rootBytes > 0 {
			step("   → root-dir carries the image: layers are COPIED per container")
		}

		return elapsed, nil
	}

	d1, err := create("raid1/images/layerdir-a")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "argument") {
			rep.PerContainerLayerDir = "unsupported"
			step("container/add rejected a per-container layer-dir: %v", err)
			step("NOT falling back to the global setting — production layers would land on a volume this probe deletes")
			return rep
		}
		rep.Error = fmt.Sprintf("first create: %v", err)
		step("first create failed: %v", err)
		return rep
	}
	rep.PerContainerLayerDir = "supported"
	rep.FirstCreate = d1.Round(time.Millisecond).String()
	step("per-container layer-dir ACCEPTED; first create %s", rep.FirstCreate)

	// Full local walk: names, sizes, and any hidden bookkeeping RouterOS
	// uses to decide a layer is present and complete.
	if entries, lerr := ros.LocalTree(layerStore, 120); lerr == nil {
		rep.LayerStoreContents = entries
		step("layer store tree (%d entries):", len(entries))
		for _, e := range entries {
			step("   %s", e)
		}
	} else {
		step("local walk unavailable (%v) — falling back to /file listing", lerr)
		if entries, lerr2 := ros.ListDirectory(ctx, layerStore); lerr2 == nil {
			rep.LayerStoreContents = entries
			step("layer store holds: %v", entries)
		}
	}
	remove()
	_ = ros.RemoveDirectory(ctx, "raid1/images/layerdir-a")

	// ── 3. Second create — reuse from the clone?
	d2, err := create("raid1/images/layerdir-b")
	if err != nil {
		rep.Error = fmt.Sprintf("second create: %v", err)
		return rep
	}
	rep.SecondCreate = d2.Round(time.Millisecond).String()
	step("second create %s", rep.SecondCreate)
	remove()
	_ = ros.RemoveDirectory(ctx, "raid1/images/layerdir-b")

	// ── 4. The prize: with the store populated, is an image source still
	// required at all? Imageless add was rejected on an EMPTY store; the
	// question is whether a populated layer-dir satisfies it.
	imagelessErr := ros.ContainerAddRaw(ctx, map[string]string{
		"name":          name,
		"interface":     veth,
		"root-dir":      "raid1/images/layerdir-c",
		"layer-dir":     layerStore,
		"logging":       "yes",
		"start-on-boot": "no",
	})
	if imagelessErr == nil {
		rep.StubStillNeeded = "no"
		step("IMAGELESS add with a populated layer-dir ACCEPTED — no tarball needed at all")
		remove()
	} else {
		step("imageless add with a populated layer-dir: %v", imagelessErr)
	}
	_ = ros.RemoveDirectory(ctx, "raid1/images/layerdir-c")

	// ── 5. No root-dir at all. RouterOS allows root-dir to be unset (7.21.4
	// fixed startup for exactly that case) and unpacks layers relative to it
	// (7.18). If a container runs from the layer store alone, that IS the
	// overlay model: layers as the lower, RouterOS owning the upper — and
	// nothing per-pod to extract.
	for _, v := range []struct {
		label  string
		params map[string]string
	}{
		{"layer-dir + image, NO root-dir", map[string]string{
			"name": name, "interface": veth, "remote-image": image,
			"layer-dir": layerStore, "logging": "yes", "start-on-boot": "no",
		}},
		{"layer-dir ONLY (no image, no root-dir)", map[string]string{
			"name": name, "interface": veth,
			"layer-dir": layerStore, "logging": "yes", "start-on-boot": "no",
		}},
	} {
		if err := ros.ContainerAddRaw(ctx, v.params); err != nil {
			step("%s: REJECTED (%v)", v.label, err)
			continue
		}
		step("%s: ACCEPTED", v.label)
		ros.InvalidateContainerCache()
		if ct, gerr := ros.GetContainer(ctx, name); gerr == nil {
			step("   → root-dir RouterOS chose: %q (running=%q stopped=%q)", ct.RootDir, ct.Running, ct.Stopped)
			if v.params["remote-image"] == "" {
				rep.StubStillNeeded = "no"
			}
		}
		remove()
	}

	switch {
	case d2 < d1/3:
		rep.Verdict = "layers-reused-from-clone"
		step("second create %.1fx faster — the clone's layer store is reused; a per-pod tarball is unnecessary", float64(d1)/float64(d2))
	case d2 < d1*4/5:
		rep.Verdict = "partial-reuse"
	default:
		rep.Verdict = "no-reuse"
		rep.StubStillNeeded = "yes"
		step("no speedup — RouterOS re-extracts even with the layer store present")
	}
	return rep
}
