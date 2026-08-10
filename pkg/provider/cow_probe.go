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

	// ── 4. THE decisive step: container/add with no image source ──
	spec := routeros.ContainerSpec{
		Name:        cowProbeContainer,
		Interface:   cowProbeVeth,
		RootDir:     rootfs,
		Entrypoint:  "/bin/probe",
		Cmd:         "--help",
		Logging:     "yes",
		StartOnBoot: "no",
	}
	if err := ros.CreateContainer(ctx, spec); err != nil {
		step("container/add REJECTED without an image source: %v", err)
		rep.Verdict = "unsupported"
		p.cowProbeCleanup(ctx, ros, pvc, true)
		return rep
	}
	step("container/add ACCEPTED with pre-populated root-dir and no file=/remote-image=")

	// ── 5. Does it start? ──
	started := false
	var lastStatus string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ros.InvalidateContainerCache()
		ct, gerr := ros.GetContainer(ctx, cowProbeContainer)
		if gerr != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		lastStatus = fmt.Sprintf("running=%s stopped=%s comment=%q", ct.Running, ct.Stopped, ct.Comment)
		if ct.IsRunning() {
			started = true
			break
		}
		if ct.IsStopped() {
			if serr := ros.StartContainer(ctx, ct.ID); serr != nil {
				step("container/start failed: %v", serr)
				break
			}
			time.Sleep(3 * time.Second)
			ros.InvalidateContainerCache()
			if after, aerr := ros.GetContainer(ctx, cowProbeContainer); aerr == nil {
				lastStatus = fmt.Sprintf("running=%s stopped=%s comment=%q", after.Running, after.Stopped, after.Comment)
				// A one-shot binary may already have run and exited — a
				// clean stopped state with no error comment counts as
				// "RouterOS executed it".
				started = after.IsRunning() || (after.IsStopped() && after.Comment == "")
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	if started {
		step("container started from the pre-populated root-dir (%s)", lastStatus)
		rep.Verdict = "supported"
	} else {
		step("container did not start cleanly (%s) — add works, start behavior needs a real init; verdict still supported for the add capability", lastStatus)
		rep.Verdict = "supported"
	}

	p.cowProbeCleanup(ctx, ros, pvc, true)
	step("cleanup complete")
	return rep
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
