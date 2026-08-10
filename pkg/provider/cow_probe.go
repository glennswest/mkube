package provider

// CoW Phase 0 capability probe.
//
// The golden-image/clone-per-pod catalog (untar an image ONCE into a sealed
// stormblock template, clone per pod, point the container root-dir at the
// mounted clone) hinges on one untested RouterOS behavior: will
// /container/add accept a root-dir that already contains a rootfs, with no
// file= or remote-image= to extract? This probe answers that with a live
// experiment and cleans up after itself:
//
//  1. provision a small stormblock volume (attach, format, mount) — the
//     exact path a clone would take
//  2. lay a one-binary rootfs on it (the static iscsi-pvc binary mkube ships)
//  3. /container/add with root-dir=<mount>/rootfs and NO image source ← verdict
//  4. start it and observe
//  5. tear everything down
//
// POST /api/v1/probes/cow runs it; the report lists every step and the
// verdict: "supported" (direct catalog is buildable), "unsupported"
// (stormblock-registry fallback needed), or "inconclusive" (a step before
// the decisive one failed).

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/routeros"
)

type CoWProbeReport struct {
	Verdict string   `json:"verdict"` // supported | unsupported | inconclusive
	Steps   []string `json:"steps"`
	Error   string   `json:"error,omitempty"`
}

const (
	cowProbePVCName   = "cow-probe"
	cowProbeNamespace = "gt"
	cowProbeContainer = "gt_cowprobe_cowprobe"
	cowProbeVeth      = "veth_gt_cowprobe_0"
	// The static aarch64-musl binary mkube's image carries — a one-file rootfs.
	cowProbeBinary = "/usr/local/bin/iscsi-pvc"
)

func (p *MicroKubeProvider) handleCoWProbe(w http.ResponseWriter, r *http.Request) {
	// Detached context: the probe must finish (and clean up) even if the
	// HTTP client gives up mid-run.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	report := p.RunCoWProbe(ctx)
	code := http.StatusOK
	if report.Verdict == "inconclusive" {
		code = http.StatusInternalServerError
	}
	podWriteJSON(w, code, report)
}

func (p *MicroKubeProvider) RunCoWProbe(ctx context.Context) *CoWProbeReport {
	rep := &CoWProbeReport{Verdict: "inconclusive"}
	step := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		rep.Steps = append(rep.Steps, s)
		p.deps.Logger.Infow("COW-PROBE: " + s)
	}
	fail := func(err error) *CoWProbeReport {
		rep.Error = err.Error()
		p.deps.Logger.Errorw("COW-PROBE failed", "error", err)
		return rep
	}

	ros := p.getRouterOSClient()
	if ros == nil {
		return fail(fmt.Errorf("probe requires the RouterOS backend"))
	}

	// Shield the probe container from mkube's own orphan sweep.
	unguard := p.cowProbeGuardPod()
	defer unguard()

	// ── 1. Volume: provision through the standard stormblock PVC path ──
	sc := pvcTypeStormblock
	pvcKey := cowProbeNamespace + "/" + cowProbePVCName
	pvc, ok := p.pvcs.Get(pvcKey)
	if !ok {
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              cowProbePVCName,
				Namespace:         cowProbeNamespace,
				CreationTimestamp: metav1.Now(),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: &sc,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("256Mi"),
					},
				},
			},
		}
		p.pvcs.Set(pvcKey, pvc)
		if p.deps.Store != nil {
			_, _ = p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, cowProbeNamespace+"."+cowProbePVCName, pvc)
		}
	}

	mountPoint, err := p.provisionStormblockPVC(ctx, pvc)
	if err != nil {
		p.cowProbeCleanup(ctx, ros, pvc, false)
		return fail(fmt.Errorf("provisioning probe volume: %w", err))
	}
	step("stormblock volume provisioned and mounted at %s", mountPoint)

	// ── 2. Rootfs: one static binary, delivered by making the DEVICE fetch
	// it from mkube's API (the REST upload resets on non-flash disk paths).
	rootfs := strings.TrimPrefix(mountPoint, "/") + "/rootfs"
	payloadURL := fmt.Sprintf("http://%s/api/v1/probes/cow/payload", p.ownAPIAddr())
	if err := ros.FetchFile(ctx, payloadURL, rootfs+"/bin/probe"); err != nil {
		p.cowProbeCleanup(ctx, ros, pvc, false)
		return fail(fmt.Errorf("device fetch of rootfs binary from %s: %w", payloadURL, err))
	}
	step("pre-populated rootfs at %s (device fetched static binary from %s)", rootfs, payloadURL)

	// ── 3. Network ──
	_, _, _, err = p.deps.NetworkMgr.AllocateInterface(ctx, cowProbeVeth, "cowprobe.cowprobe", "gt", "")
	if err != nil {
		p.cowProbeCleanup(ctx, ros, pvc, false)
		return fail(fmt.Errorf("allocating probe veth: %w", err))
	}
	step("veth %s allocated", cowProbeVeth)

	// ── 4a. Baseline: imageless container/add (Phase 0 — known rejected,
	// kept so the report always states the ground truth for this ROS build).
	imageless := routeros.ContainerSpec{
		Name:        cowProbeContainer,
		Interface:   cowProbeVeth,
		RootDir:     rootfs,
		Entrypoint:  "/bin/probe",
		Cmd:         "--help",
		Logging:     "yes",
		StartOnBoot: "no",
	}
	if err := ros.CreateContainer(ctx, imageless); err != nil {
		step("A0 imageless add: REJECTED (%v)", err)
	} else {
		step("A0 imageless add: ACCEPTED")
		p.cowProbeRemoveContainer(ctx, ros)
	}

	// ── 4b. Stub tarball on the device (one placeholder file) ──
	stubPath := "raid1/cache/cow-probe-stub.tar"
	if err := ros.UploadFile(ctx, stubPath, bytes.NewReader(cowStubTar())); err != nil {
		p.cowProbeCleanup(ctx, ros, pvc, false)
		return fail(fmt.Errorf("writing stub tarball: %w", err))
	}
	step("stub tarball at %s (%d bytes, single placeholder file)", stubPath, len(cowStubTar()))

	// deviceLog surfaces the device's own container-related log lines —
	// RouterOS reports async container failures only there (a failed
	// container is silently auto-removed).
	deviceLog := func(label string) {
		entries, lerr := ros.TailLog(ctx, 50)
		if lerr != nil {
			step("%s: device log unavailable: %v", label, lerr)
			return
		}
		var picked []string
		for _, e := range entries {
			if strings.Contains(e.Topics, "container") || strings.Contains(e.Message, "container") || strings.Contains(e.Message, "cowprobe") {
				picked = append(picked, e.Time+" "+e.Message)
			}
		}
		if len(picked) > 8 {
			picked = picked[len(picked)-8:]
		}
		step("%s — device log: %s", label, strings.Join(picked, " | "))
	}

	// ── 4b2. Control B0: stub-only container (no mount). If this survives
	// and B vanishes, the mount is what kills B.
	vethB0 := "veth_gt_cowprobe_2"
	if _, _, _, verr := p.deps.NetworkMgr.AllocateInterface(ctx, vethB0, "cowprobe0.cowprobe0", "gt", ""); verr != nil {
		step("B0 veth allocation failed: %v", verr)
	} else {
		specB0 := routeros.ContainerSpec{
			Name:        cowProbeContainer,
			Interface:   vethB0,
			RootDir:     "raid1/images/cowprobe-stub0",
			File:        stubPath,
			Entrypoint:  "/bin/sh",
			Logging:     "yes",
			StartOnBoot: "no",
		}
		if err := ros.CreateContainer(ctx, specB0); err != nil {
			step("B0 stub-only add: REJECTED (%v)", err)
		} else {
			time.Sleep(6 * time.Second)
			ros.InvalidateContainerCache()
			if ct, gerr := ros.GetContainer(ctx, cowProbeContainer); gerr == nil {
				step("B0 stub-only container persists after add (running=%q stopped=%q comment=%q)", ct.Running, ct.Stopped, ct.Comment)
			} else {
				step("B0 stub-only container VANISHED after add — stub/extraction problem, not the mount")
				deviceLog("B0")
			}
			p.cowProbeRemoveContainer(ctx, ros)
		}
		_ = ros.RemoveDirectory(ctx, "raid1/images/cowprobe-stub0")
		_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethB0)
	}

	// ── 4b3. Seeder: extract the payload docker-save archive ONTO the clone
	// via a throwaway container add — RouterOS's own untar preserves the
	// exec bit that /tool/fetch (0644) cannot ("execvpe: Permission denied"
	// in run 8). The seeder is left in place (never started); its removal
	// would wipe the rootfs it seeded.
	seederName := "gt_cowprobe_cowprobe1"
	payloadPath := "raid1/cache/cow-probe-payload.tar"
	if bin, rerr := os.ReadFile(cowProbeBinary); rerr != nil {
		step("seeder skipped: cannot read payload binary: %v", rerr)
	} else if uerr := ros.UploadFile(ctx, payloadPath, bytes.NewReader(cowPayloadTar(bin))); uerr != nil {
		step("seeder skipped: payload upload failed: %v", uerr)
	} else if _, _, _, verr := p.deps.NetworkMgr.AllocateInterface(ctx, cowProbeVeth, "cowprobe.cowprobe", "gt", ""); verr != nil {
		step("seeder skipped: veth re-ensure failed: %v", verr)
	} else {
		seeder := routeros.ContainerSpec{
			Name:        seederName,
			Interface:   cowProbeVeth,
			RootDir:     rootfs,
			File:        payloadPath,
			Logging:     "yes",
			StartOnBoot: "no",
		}
		if serr := ros.CreateContainer(ctx, seeder); serr != nil {
			step("seeder add failed: %v", serr)
		} else {
			time.Sleep(6 * time.Second)
			if ok, _ := ros.FileExists(ctx, rootfs+"/bin/probe"); ok {
				step("seeder extracted payload onto the clone (bin/probe present, modes preserved by untar)")
			} else {
				step("seeder extraction did not produce bin/probe on the clone")
				deviceLog("seeder")
			}
		}
	}

	// ── 4c. Variant B FIRST: stub root-dir on raid1 + clone content via
	// MOUNT, entrypoint inside the mount (covers scratch/static-binary
	// images). Runs before variant A because container/remove deletes the
	// root-dir contents — A's teardown destroys the clone rootfs B mounts.
	mountList := "cowprobe"
	_ = ros.RemoveMountsByList(ctx, mountList)
	// Variant A's container teardown consumes the veth — B needs its own.
	vethB := "veth_gt_cowprobe_1"
	if _, _, _, verr := p.deps.NetworkMgr.AllocateInterface(ctx, vethB, "cowprobeb.cowprobeb", "gt", ""); verr != nil {
		step("B veth allocation failed: %v", verr)
		vethB = ""
	}
	if vethB == "" {
		step("B skipped: no veth")
	} else if err := ros.CreateMount(ctx, mountList, "/"+rootfs, "/payload"); err != nil {
		step("B mount create failed: %v", err)
	} else {
		specB := routeros.ContainerSpec{
			Name:        cowProbeContainer,
			Interface:   vethB,
			RootDir:     "raid1/images/cowprobe-stub",
			File:        stubPath,
			MountLists:  mountList,
			Entrypoint:  "/payload/bin/probe",
			Cmd:         "--help",
			Logging:     "yes",
			StartOnBoot: "no",
		}
		if err := ros.CreateContainer(ctx, specB); err != nil {
			step("B stub-rootdir + clone-mount add: REJECTED (%v)", err)
		} else {
			time.Sleep(5 * time.Second)
			started, status := p.cowProbeTryStart(ctx, ros)
			ran := started
			if !ran {
				// The binary may have run to completion (one-shot) — the
				// device log is authoritative: a "started" line without an
				// execvpe error means RouterOS executed it from the mount.
				if entries, lerr := ros.TailLog(ctx, 40); lerr == nil {
					var sawStart, sawExecErr bool
					for _, e := range entries {
						if strings.Contains(e.Message, "started") && strings.Contains(e.Message, "/payload/bin/probe") {
							sawStart = true
						}
						if strings.Contains(e.Message, "execvpe") || strings.Contains(e.Message, "No such file") {
							sawExecErr = true
						}
					}
					ran = sawStart && !sawExecErr
				}
			}
			if ran {
				step("B stub-rootdir + clone-mount: container RAN the binary from the mounted clone (%s)", status)
				rep.Verdict = "supported"
			} else {
				step("B stub-rootdir + clone-mount: add accepted, start not clean (%s)", status)
				deviceLog("B")
			}
			p.cowProbeRemoveContainer(ctx, ros)
		}
		_ = ros.RemoveMountsByList(ctx, mountList)
		_ = ros.RemoveDirectory(ctx, "raid1/images/cowprobe-stub")
	}
	if vethB != "" {
		_ = p.deps.NetworkMgr.ReleaseInterface(ctx, vethB)
	}

	// ── 4d. Variant A LAST (destructive): stub file= with root-dir ON the
	// pre-populated clone — its container removal deletes the rootfs.
	// Decisive question: does extraction PRESERVE the existing files?
	specA := imageless
	specA.File = stubPath
	verdictA := "rejected"
	if err := ros.CreateContainer(ctx, specA); err != nil {
		step("A stub-into-clone add: REJECTED (%v)", err)
	} else {
		// Wait for extraction to settle, then check the pre-populated file.
		time.Sleep(5 * time.Second)
		preserved, perr := ros.FileExists(ctx, rootfs+"/bin/probe")
		stubThere, _ := ros.FileExists(ctx, rootfs+"/cow-probe-placeholder")
		switch {
		case perr != nil:
			verdictA = "accepted, preservation unknown"
			step("A stub-into-clone add: ACCEPTED; preservation check failed: %v", perr)
		case preserved:
			verdictA = "accepted, pre-populated content PRESERVED"
			step("A stub-into-clone add: ACCEPTED and pre-populated /bin/probe survived extraction (stub placeholder present: %v)", stubThere)
			if started, status := p.cowProbeTryStart(ctx, ros); started {
				step("A container started from the clone-backed root-dir (%s)", status)
			} else {
				step("A container start not clean (%s)", status)
			}
		default:
			verdictA = "accepted but extraction WIPED the root-dir"
			step("A stub-into-clone add: ACCEPTED but pre-populated /bin/probe is GONE — extraction wipes root-dir")
		}
		p.cowProbeRemoveContainer(ctx, ros)
	}

	// ── 4e. Variant B2: is dst=/ accepted for a mount? (full rootfs shadowing)
	rootList := "cowproberoot"
	_ = ros.RemoveMountsByList(ctx, rootList)
	if err := ros.CreateMount(ctx, rootList, "/"+rootfs, "/"); err != nil {
		step("B2 mount with dst=/: REJECTED (%v)", err)
	} else {
		step("B2 mount with dst=/: accepted by /container/mounts — full-root shadowing may be possible (not container-tested in this probe)")
		_ = ros.RemoveMountsByList(ctx, rootList)
	}

	if rep.Verdict == "inconclusive" {
		if strings.Contains(verdictA, "PRESERVED") {
			rep.Verdict = "supported"
		} else {
			rep.Verdict = "unsupported"
		}
	}
	step("variant A: %s", verdictA)

	_ = ros.RemoveFile(ctx, stubPath)
	p.cowProbeCleanup(ctx, ros, pvc, true)
	step("cleanup complete")
	return rep
}

// cowProbeTryStart starts the probe container and reports whether RouterOS
// executed it (running, or ran-to-completion with no error comment).
//
// Some states report neither running=true nor stopped=true (extraction,
// freshly added); after a short grace we issue the start unconditionally —
// waiting for stopped=true meant an indeterminate container never got
// started at all.
func (p *MicroKubeProvider) cowProbeTryStart(ctx context.Context, ros *routeros.Client) (bool, string) {
	row := func(ct *routeros.Container) string {
		return fmt.Sprintf("id=%s running=%q stopped=%q comment=%q tag=%q rootdir=%q",
			ct.ID, ct.Running, ct.Stopped, ct.Comment, ct.Tag, ct.RootDir)
	}
	var lastStatus string
	startIssued := false
	graceOver := time.Now().Add(8 * time.Second)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ros.InvalidateContainerCache()
		ct, err := ros.GetContainer(ctx, cowProbeContainer)
		if err != nil {
			lastStatus = fmt.Sprintf("lookup: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		lastStatus = row(ct)
		if ct.IsRunning() {
			return true, lastStatus
		}
		if !startIssued && (ct.IsStopped() || time.Now().After(graceOver)) {
			if serr := ros.StartContainer(ctx, ct.ID); serr != nil {
				return false, fmt.Sprintf("start failed: %v (%s)", serr, lastStatus)
			}
			startIssued = true
			time.Sleep(3 * time.Second)
			continue
		}
		if startIssued && ct.IsStopped() {
			// One-shot binary may have run to completion already.
			return ct.Comment == "", lastStatus
		}
		time.Sleep(2 * time.Second)
	}
	return false, lastStatus
}

// cowProbeRemoveContainer removes the probe container if present — via raw
// client calls, NOT stopAndRemoveContainer: that provider helper also tears
// down the pod's derived veths, which silently destroyed probe interfaces
// still needed by later variants (run 9: seeder's veth was gone).
func (p *MicroKubeProvider) cowProbeRemoveContainer(ctx context.Context, ros *routeros.Client) {
	ros.InvalidateContainerCache()
	ct, err := ros.GetContainer(ctx, cowProbeContainer)
	if err != nil {
		return
	}
	_ = ros.StopContainer(ctx, ct.ID)
	for i := 0; i < 6; i++ {
		time.Sleep(2 * time.Second)
		if rerr := ros.RemoveContainer(ctx, ct.ID); rerr == nil {
			break
		}
	}
	ros.InvalidateContainerCache()
}

// cowPayloadTar returns a docker-save archive whose layer carries the probe
// binary at bin/probe with mode 0755 — extraction by RouterOS preserves the
// exec bit that /tool/fetch (0644) cannot deliver. This is also the
// production seeding pattern: a throwaway container add extracts the real
// image onto the golden volume with correct modes.
func cowPayloadTar(bin []byte) []byte {
	var layer bytes.Buffer
	lw := tar.NewWriter(&layer)
	_ = lw.WriteHeader(&tar.Header{Name: "bin", Mode: 0o755, Typeflag: tar.TypeDir})
	_ = lw.WriteHeader(&tar.Header{Name: "bin/probe", Mode: 0o755, Size: int64(len(bin))})
	_, _ = lw.Write(bin)
	_ = lw.Close()

	layerSum := sha256.Sum256(layer.Bytes())
	config := fmt.Sprintf(`{"architecture":"arm64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["sha256:%x"]}}`, layerSum)
	manifest := `[{"Config":"config.json","RepoTags":["cow-probe-payload:latest"],"Layers":["layer.tar"]}]`

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	add("config.json", []byte(config))
	add("layer.tar", layer.Bytes())
	add("manifest.json", []byte(manifest))
	_ = tw.Close()
	return buf.Bytes()
}

// cowStubTar returns a minimal DOCKER-SAVE archive (manifest.json + config +
// one layer) holding a single placeholder file. RouterOS's file= extractor
// requires the docker-save layout — a plain tar fails with "no manifest.json
// in archive" and the container is silently auto-removed (device log,
// 2026-08-10).
func cowStubTar() []byte {
	// Inner layer: one placeholder file.
	var layer bytes.Buffer
	lw := tar.NewWriter(&layer)
	content := []byte("cow-probe\n")
	_ = lw.WriteHeader(&tar.Header{Name: "cow-probe-placeholder", Mode: 0o644, Size: int64(len(content))})
	_, _ = lw.Write(content)
	_ = lw.Close()

	layerSum := sha256.Sum256(layer.Bytes())
	config := fmt.Sprintf(`{"architecture":"arm64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["sha256:%x"]}}`, layerSum)
	manifest := `[{"Config":"config.json","RepoTags":["cow-probe-stub:latest"],"Layers":["layer.tar"]}]`

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	add("config.json", []byte(config))
	add("layer.tar", layer.Bytes())
	add("manifest.json", []byte(manifest))
	_ = tw.Close()
	return buf.Bytes()
}

// cowProbeGuardPod registers a fake tracked pod matching the probe container
// so mkube's own orphan sweep does not reap it mid-probe (device log showed
// 'container removed by api:mkube' seconds after each add). Returns an
// unguard func for cleanup.
func (p *MicroKubeProvider) cowProbeGuardPod() func() {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cowprobe",
			Namespace: "gt",
			Annotations: map[string]string{
				annotationNetwork: "gt",
				annotationNode:    p.nodeName,
			},
		},
		Spec: corev1.PodSpec{
			// Three container slots: the veth sweep derives owned veth names
			// as veth_<ns>_<pod>_<index>, so indices 0..2 cover every veth
			// the probe allocates (issue #18's reaper removes the CONTAINER
			// holding an unowned veth — it took the probe container out).
			Containers: []corev1.Container{{Name: "cowprobe"}, {Name: "cowprobe1"}, {Name: "cowprobe2"}},
		},
	}
	key := podKey(pod)
	p.pods.Set(key, pod)
	// The redeploying flag makes the reconciler SKIP this pod: without it,
	// "not all containers exist" enqueues CreatePod, whose pre-creation
	// cleanup removes "stale" probe containers seconds after every add —
	// the actual remover behind runs 4..10 (device log action ids were
	// CreatePod stale-cleanups, not sweeps).
	p.redeploying.Set(key, true)
	return func() {
		// Pod first, flag second: with the flag gone while the pod is still
		// tracked, one reconcile tick can enqueue a create for the guard
		// (observed: "creating pod gt/cowprobe" right after unguard).
		p.pods.Delete(key)
		p.redeploying.Delete(key)
	}
}

// handleCoWProbePayload serves the static probe binary so the device can
// /tool/fetch it onto the probe volume.
func (p *MicroKubeProvider) handleCoWProbePayload(w http.ResponseWriter, r *http.Request) {
	bin, err := os.ReadFile(cowProbeBinary)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bin)))
	_, _ = w.Write(bin)
}

// ownAPIAddr returns the host:port the DEVICE can reach mkube's API on,
// derived from the local end of a dial toward the device.
func (p *MicroKubeProvider) ownAPIAddr() string {
	addr := p.deps.Config.RouterOS.Address
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return "192.168.200.2:8082" // gt-network default
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "192.168.200.2:8082"
	}
	return net.JoinHostPort(host, "8082")
}

// cowProbeCleanup tears down everything the probe may have created.
func (p *MicroKubeProvider) cowProbeCleanup(ctx context.Context, ros *routeros.Client, pvc *corev1.PersistentVolumeClaim, hadContainer bool) {
	if hadContainer {
		if ct, err := ros.GetContainer(ctx, cowProbeContainer); err == nil {
			p.stopAndRemoveContainer(ctx, cowProbeContainer, ct.ID)
		}
	}
	if ct, err := ros.GetContainer(ctx, "gt_cowprobe_cowprobe1"); err == nil {
		p.stopAndRemoveContainer(ctx, "gt_cowprobe_cowprobe1", ct.ID)
	}
	_ = ros.RemoveFile(ctx, "raid1/cache/cow-probe-payload.tar")
	_ = p.deps.NetworkMgr.ReleaseInterface(ctx, cowProbeVeth)
	if err := p.deprovisionStormblockPVC(ctx, pvc); err != nil {
		p.deps.Logger.Warnw("COW-PROBE: volume cleanup failed", "error", err)
	}
	pvcKey := pvc.Namespace + "/" + pvc.Name
	p.pvcs.Delete(pvcKey)
	if p.deps.Store != nil {
		_ = p.deps.Store.PersistentVolumeClaims.Delete(ctx, pvc.Namespace+"."+pvc.Name)
	}
}
