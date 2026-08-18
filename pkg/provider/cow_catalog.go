package provider

// CoW image catalog: untar once per image digest, clone per pod.
//
// Proven by the Phase 0/0b probes (2026-08-10, probe run 11):
//
//   golden:  stormblock fstemplate volume → attach → ext4 format → mount →
//            SEEDER container (file=<image docker-save>, root-dir on the
//            mount) makes RouterOS itself untar the image with modes intact
//            → detach → seal → remove seeder (its root-dir wipe hits a path
//            that no longer exists; the sealed snapshot keeps the content)
//
//   per pod: volume clone from_template (metadata cost) → attach → RouterOS
//            auto-mounts the ext4 → container = tiny generic docker-save
//            stub as file= + clone mounted at /payload + entrypoint
//            rewritten into the mount.
//
// Opt-in per pod via the annotation `vkube.io/image-mode: cow`. Phase 1
// targets scratch/static-binary images (the bulk of the fleet); full-distro
// images need the symlink-farm stub (see work plan).
//
// Hard RouterOS facts this design lives with: imageless container/add is
// rejected; extraction always displaces root-dir; mounts cannot target /;
// container removal wipes its root-dir.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/routeros"
)

const (
	annotationImageMode = "vkube.io/image-mode"
	imageModeCoW        = "cow"

	annCoWVolumeID = "vkube.io/cow-volume-id"
	annCoWTemplate = "vkube.io/cow-template"

	cowStubDevicePath = "raid1/cache/cow-generic-stub.tar"
	cowPayloadDst     = "/payload"

	// goldenSource values.
	goldenSourceMkube      = "mkube"
	goldenSourceSbRegistry = "sbregistry"
)

// isCoWPod reports whether the pod opted into the CoW image catalog.
func isCoWPod(pod *corev1.Pod) bool {
	return pod != nil && pod.Annotations[annotationImageMode] == imageModeCoW
}

// dockerSaveConfig is the subset of the OCI image config the CoW path needs.
type dockerSaveConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
}

// readDockerSaveConfig parses the embedded image config out of a cached
// docker-save tarball (local read — the cache lives under the /hostraid1
// mount as well as being the source of truth for seeding).
func readDockerSaveConfig(tarballPath string) (*dockerSaveConfig, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var manifestRaw, configRaw []byte
	files := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "manifest.json" || strings.HasSuffix(hdr.Name, ".json") {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			files[hdr.Name] = data
			if hdr.Name == "manifest.json" {
				manifestRaw = data
			}
		}
	}
	if manifestRaw == nil {
		return nil, fmt.Errorf("no manifest.json in %s", tarballPath)
	}
	var manifest []struct {
		Config string `json:"Config"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil || len(manifest) == 0 {
		return nil, fmt.Errorf("parsing manifest.json in %s: %v", tarballPath, err)
	}
	configRaw = files[manifest[0].Config]
	if configRaw == nil {
		return nil, fmt.Errorf("config %q not found in %s", manifest[0].Config, tarballPath)
	}
	var cfg struct {
		Config struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
			Env        []string `json:"Env"`
			WorkingDir string   `json:"WorkingDir"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configRaw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing image config in %s: %w", tarballPath, err)
	}
	return &dockerSaveConfig{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
	}, nil
}

// parseImageConfigJSON reads entrypoint/cmd/env out of an OCI image config
// blob (the few-KB document, not the tarball).
func parseImageConfigJSON(blob []byte) (*dockerSaveConfig, error) {
	var cfg struct {
		Config struct {
			Entrypoint []string `json:"Entrypoint"`
			Cmd        []string `json:"Cmd"`
			Env        []string `json:"Env"`
			WorkingDir string   `json:"WorkingDir"`
		} `json:"config"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return nil, fmt.Errorf("parsing image config: %w", err)
	}
	return &dockerSaveConfig{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
	}, nil
}

// sbTemplate is a stormblockmk fstemplate row.
type sbTemplate struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	RawVolumeID  string `json:"raw_volume_id"`
}

// sbCreateTemplateResp is the create-template response: the template row
// plus the attach block for the formatting/seeding phase.
type sbCreateTemplateResp struct {
	Template sbTemplate      `json:"template"`
	Attach   json.RawMessage `json:"attach"`
}

// createTemplateForFormatting creates an fstemplate that is left unformatted,
// and exports its raw volume so the caller can format over it.
//
// Two things changed under us when fstemplates moved into stormblock core:
//
//  1. create defaults to `format: true`, which formats **and seals** in the one
//     call. That is right for a blank, and useless for anything that needs to
//     put content in first — there would be no writable window. Passing
//     `format: false` leaves the template in `awaiting_format`.
//  2. create no longer returns an export or an attach block. The engine
//     formats in-core when it owns the whole job, so it never needs an
//     initiator. Anything that does need one asks for it.
//
// The caller must withdraw the returned export before sealing: seal refuses
// while a session is still established on it.
func (p *MicroKubeProvider) createTemplateForFormatting(
	ctx context.Context, sb *sbClient, name string, sizeBytes int64,
) (tmpl sbTemplate, attach sbAttach, exportID string, err error) {
	var created sbCreateTemplateResp
	if err = sb.do(ctx, http.MethodPost, "/api/v1/fstemplates",
		map[string]any{
			"name": name, "fs": "ext4", "size_bytes": sizeBytes,
			"label": name, "format": false,
		}, &created); err != nil {
		return tmpl, attach, "", fmt.Errorf("creating fstemplate %s: %w", name, err)
	}
	tmpl = created.Template
	if tmpl.RawVolumeID == "" {
		return tmpl, attach, "", fmt.Errorf("fstemplate %s returned no raw_volume_id", name)
	}

	var ex sbExport
	if err = sb.do(ctx, http.MethodPost, "/mk/v1/exports",
		map[string]any{"volume_id": tmpl.RawVolumeID, "protocol": p.sbProtocol(),
			"ephemeral": false}, &ex); err != nil {
		_ = sb.do(context.Background(), http.MethodDelete,
			"/api/v1/fstemplates/"+tmpl.ID+"?force=true", nil, nil)
		return tmpl, attach, "", fmt.Errorf("exporting fstemplate %s: %w", name, err)
	}
	attach = ex.Attach
	if attach.Transport == "" {
		attach.Transport = ex.Protocol
	}
	if attach.Address == "" {
		_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/exports/"+ex.ExportID, nil, nil)
		_ = sb.do(context.Background(), http.MethodDelete,
			"/api/v1/fstemplates/"+tmpl.ID+"?force=true", nil, nil)
		return tmpl, attach, "", fmt.Errorf("fstemplate %s export returned no attach parameters", name)
	}
	return tmpl, attach, ex.ExportID, nil
}

// ensureGenericStub makes sure the generic CoW stub archive exists on the
// device (one placeholder file; docker-save layout).
func (p *MicroKubeProvider) ensureGenericStub(ctx context.Context, ros *routeros.Client) error {
	if ok, err := ros.FileExists(ctx, cowStubDevicePath); err == nil && ok {
		return nil
	}
	return ros.UploadFile(ctx, cowStubDevicePath, bytes.NewReader(cowStubTar()))
}

// cowPayloadRoot returns the path inside a mounted clone that holds the
// image's filesystem.
//
// The two golden builders lay it out differently, and the difference is not
// cosmetic — get it wrong and the container dies with
// `execvpe /payload/bin/sh: No such file or directory`.
//
//   - sbregistry writes the image's rootfs at the **volume root**, which is
//     what the integration contract asks for: "write the image's rootfs into
//     it". A clone mounts and / is the image.
//   - mkube's own seeder extracts a docker-save tarball into <mount>/rootfs,
//     so the image sits one level down.
func (p *MicroKubeProvider) cowPayloadRoot(mountPoint string) string {
	if strings.EqualFold(p.deps.Config.Storage.Stormblock.GoldenSource, goldenSourceSbRegistry) {
		return mountPoint
	}
	return mountPoint + "/rootfs"
}

// cowTemplateName derives the fstemplate name for an image digest.
func cowTemplateName(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "img-" + d
}

// ensureGoldenTemplate makes sure a sealed fstemplate exists for the image
// and returns its name. The expensive path (create+seed+seal) runs once per
// image digest; every later call finds the sealed template by name.
func (p *MicroKubeProvider) ensureGoldenTemplate(ctx context.Context, ros *routeros.Client, imageRef, tarballPath, digest string) (string, error) {
	log := p.deps.Logger.With("image", imageRef, "digest", digest)
	name := cowTemplateName(digest)

	sb, err := p.newStormblockClient()
	if err != nil {
		return "", err
	}

	// Already sealed?
	var list struct {
		Items []sbTemplate `json:"items"`
	}
	if err := sb.do(ctx, http.MethodGet, "/api/v1/fstemplates", nil, &list); err != nil {
		return "", fmt.Errorf("listing fstemplates: %w", err)
	}
	for _, t := range list.Items {
		if t.Name == name {
			if strings.EqualFold(t.State, "ready") || strings.EqualFold(t.State, "sealed") {
				return name, nil
			}
			// Half-built template from a crashed attempt — remove and rebuild.
			log.Warnw("removing half-built fstemplate", "template", name, "state", t.State)
			if derr := sb.do(ctx, http.MethodDelete, "/api/v1/fstemplates/"+t.ID+"?force=true", nil, nil); derr != nil {
				return "", fmt.Errorf("removing half-built fstemplate %s: %w", name, derr)
			}
		}
	}

	// External builder (stormblock-registry): mkube never seeds. It writes
	// image layers straight into the volume — no RouterOS mount in the write
	// path, so the filesystem is clean by construction and the seal guard
	// passes, which mkube's own seeder cannot manage. Wait for the sealed
	// template to appear rather than racing it with a build that would fail.
	if strings.EqualFold(p.deps.Config.Storage.Stormblock.GoldenSource, goldenSourceSbRegistry) {
		wait := p.deps.Config.Storage.Stormblock.GoldenWait
		if wait <= 0 {
			wait = 5 * time.Minute
		}
		log.Infow("waiting for external golden template", "template", name, "wait", wait)
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			var poll struct {
				Items []sbTemplate `json:"items"`
			}
			if err := sb.do(ctx, http.MethodGet, "/api/v1/fstemplates", nil, &poll); err == nil {
				for _, t := range poll.Items {
					if t.Name == name && (strings.EqualFold(t.State, "ready") || strings.EqualFold(t.State, "sealed")) {
						log.Infow("external golden template ready", "template", name)
						return name, nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		return "", fmt.Errorf("golden template %s not published by the external builder within %s (goldenSource=sbregistry)", name, wait)
	}

	// Only NOW does a full image need to be on disk: mkube is about to seed
	// the golden itself, and the seeder feeds RouterOS `file=<tarball>`.
	// Pod creates skip staging entirely (digest and entrypoint come from the
	// registry), and sbregistry mode returned above without ever reaching
	// here — so this is once per digest, on the build path only.
	if tarballPath == "" {
		staged, sErr := p.deps.StorageMgr.EnsureImage(ctx, imageRef)
		if sErr != nil {
			return "", fmt.Errorf("staging %s to seed the golden: %w", imageRef, sErr)
		}
		tarballPath = staged
		log.Infow("staged image to seed the golden", "tarball", tarballPath)
	}

	// Size: generous thin headroom over the tarball (thin volumes only cost
	// what is written).
	st, err := os.Stat(tarballPath)
	if err != nil {
		return "", fmt.Errorf("stat tarball: %w", err)
	}
	sizeBytes := st.Size() * 4
	if min := int64(1 << 30); sizeBytes < min {
		sizeBytes = min
	}

	log.Infow("building golden template", "template", name, "size", sizeBytes)
	// Unformatted, with an export of its own: the engine would otherwise
	// format and seal in the same call, leaving nowhere to put the image.
	tmpl, attach, exportID, err := p.createTemplateForFormatting(ctx, sb, name, sizeBytes)
	if err != nil {
		return "", err
	}
	created := sbCreateTemplateResp{Template: tmpl}
	// Seal refuses while a session is still established on the export, so it
	// has to be withdrawn once the writing is done.
	defer func() {
		if exportID != "" {
			_ = sb.do(context.Background(), http.MethodDelete, "/mk/v1/exports/"+exportID, nil, nil)
		}
	}()
	if attach.Transport == "" {
		attach.Transport = "iscsi" // template formatting attach is documented iSCSI
	}

	// Attach + format + probe-kick + mount — same dance as stormblock PVCs.
	diskID, err := p.attachStormblockDisk(ctx, attach)
	if err != nil {
		return "", fmt.Errorf("attaching template volume: %w", err)
	}
	cleanupDisk := func() { _ = ros.RemoveDisk(ctx, diskID) }

	if err := p.formatStormblockVolume(ctx, sb, created.Template.RawVolumeID, attach, name); err != nil {
		cleanupDisk()
		return "", fmt.Errorf("formatting template volume: %w", err)
	}
	_ = ros.RemoveDisk(ctx, diskID)
	diskID, err = p.attachStormblockDisk(ctx, attach)
	if err != nil {
		return "", fmt.Errorf("re-attaching template volume: %w", err)
	}
	mountPoint, err := p.waitForDiskMount(ctx, ros, diskID, 120*time.Second)
	if err != nil {
		cleanupDisk()
		return "", fmt.Errorf("waiting for template volume mount: %w", err)
	}

	// Seed via a transient guarded container: RouterOS's own untar delivers
	// the rootfs with modes intact.
	if err := p.runCoWSeeder(ctx, ros, sb, created.Template.RawVolumeID, attach, tarballPath, mountPoint); err != nil {
		cleanupDisk()
		return "", fmt.Errorf("seeding template: %w", err)
	}

	// Detach BEFORE seal (seal refuses attachments), then seal, then the
	// seeder container is already gone (runCoWSeeder removes it after the
	// detach inside — see ordering note there).
	// Seal WITHOUT force: stormblockmk verifies the ext4 superblock is
	// cleanly unmounted, and that guard is the only thing standing between
	// us and a golden that clones as an empty filesystem. RouterOS detaches
	// without flushing its page cache, so a ROS-seeded volume currently
	// fails this check — data blocks land, directory metadata does not
	// (observed live: 60 MB allocated in raw, sealed AND clone volumes,
	// every clone mounting empty). Forcing past it produced broken goldens
	// silently; failing here is correct until the seeding write path
	// bypasses the ROS page cache (that is exactly what sbregistry's
	// direct-to-volume layer writes do).
	// RouterOS force-detaches without unmounting, so the seeded filesystem
	// is left flagged dirty and stormblockmk (correctly) refuses to seal it.
	// Writes have quiesced by now — the settle above — so restore the flag
	// explicitly rather than forcing past the guard.
	if rErr := p.reidentifyStormblockVolume(ctx, sb, created.Template.RawVolumeID, attach, name, true); rErr != nil {
		p.deps.Logger.Warnw("could not restore the clean flag before sealing", "template", name, "error", rErr)
	}

	// Withdraw our export before sealing. Seal refuses while a session is
	// still established on it, and the export is ours now — the engine no
	// longer makes one at create time, so nothing else will take it down.
	if exportID != "" {
		if wErr := sb.do(ctx, http.MethodDelete, "/mk/v1/exports/"+exportID, nil, nil); wErr != nil {
			p.deps.Logger.Warnw("could not withdraw the template export before sealing",
				"template", name, "export", exportID, "error", wErr)
		}
		exportID = "" // the deferred cleanup has nothing left to do
	}

	if err := sb.do(ctx, http.MethodPost, "/api/v1/fstemplates/"+created.Template.ID+"/seal", nil, nil); err != nil {
		_ = sb.do(ctx, http.MethodDelete, "/api/v1/fstemplates/"+created.Template.ID+"?force=true", nil, nil)
		return "", fmt.Errorf("sealing fstemplate %s (ROS-seeded volumes cannot currently be cleanly unmounted — use an sbregistry-created golden): %w", name, err)
	}
	log.Infow("golden template sealed", "template", name)
	return name, nil
}

// runCoWSeeder extracts the image tarball onto the mounted template volume
// via a throwaway container, then detaches the volume and removes the
// seeder (whose root-dir wipe then hits a path that no longer resolves).
func (p *MicroKubeProvider) runCoWSeeder(ctx context.Context, ros *routeros.Client, sb *sbClient, rawVolumeID string, attach sbAttach, tarballPath, mountPoint string) error {
	unguard := p.cowProbeGuardPod() // same shields as the probe
	defer unguard()

	seederName := "gt_cowprobe_cowprobe1"
	seederVeth := "veth_gt_cowprobe_0"
	if _, _, _, err := p.deps.NetworkMgr.AllocateInterface(ctx, seederVeth, "cowprobe.cowprobe", "gt", ""); err != nil {
		return fmt.Errorf("allocating seeder veth: %w", err)
	}
	defer func() { _ = p.deps.NetworkMgr.ReleaseInterface(ctx, seederVeth) }()

	rootfs := strings.TrimPrefix(mountPoint, "/") + "/rootfs"
	devTarball := strings.TrimPrefix(tarballPath, "/")
	spec := routeros.ContainerSpec{
		Name:        seederName,
		Interface:   seederVeth,
		RootDir:     rootfs,
		File:        devTarball,
		Logging:     "yes",
		StartOnBoot: "no",
	}
	if err := ros.CreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("seeder add: %w", err)
	}

	// Wait for extraction by watching BYTES LAND ON THE VOLUME, not by
	// watching the container: a freshly-added container reports stopped
	// while RouterOS extracts in the background, so the old check returned
	// in ~6 seconds and we sealed an empty filesystem. stormblockmk knows
	// exactly how much has been written.
	allocated := func() int64 {
		var list struct {
			Items []struct {
				ID        string `json:"id"`
				Allocated int64  `json:"allocated_bytes"`
			} `json:"items"`
		}
		if err := sb.do(ctx, http.MethodGet, "/mk/v1/volumes", nil, &list); err != nil {
			return -1
		}
		for _, v := range list.Items {
			if v.ID == rawVolumeID {
				return v.Allocated
			}
		}
		return -1
	}

	extracted := false
	var last int64 = -1
	stableFor := 0
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		cur := allocated()
		switch {
		case cur < 0:
			continue
		case cur != last:
			if last >= 0 {
				p.deps.Logger.Debugw("cow seeder: extracting", "allocatedMB", cur/(1024*1024))
			}
			last, stableFor = cur, 0
		default:
			stableFor += 3
		}
		// Growth stopped for 15s after real data landed ⇒ extraction done.
		if last > 4*1024*1024 && stableFor >= 15 {
			extracted = true
			p.deps.Logger.Infow("cow seeder: extraction complete", "allocatedMB", last/(1024*1024))
			break
		}
	}

	removeSeeder := func() {
		ros.InvalidateContainerCache()
		if ct, err := ros.GetContainer(ctx, seederName); err == nil {
			_ = ros.StopContainer(ctx, ct.ID)
			for i := 0; i < 6; i++ {
				time.Sleep(2 * time.Second)
				if rerr := ros.RemoveContainer(ctx, ct.ID); rerr == nil {
					break
				}
			}
		}
	}

	if !extracted {
		removeSeeder()
		return fmt.Errorf("seeder extraction did not complete")
	}

	// RouterOS extraction writes through its page cache and RemoveDisk
	// force-detaches WITHOUT flushing: sealing immediately after produced
	// snapshots holding the data blocks but not the filesystem metadata —
	// clones mounted as clean, EMPTY ext4 (observed live: sealed volume
	// 62MB allocated, clone /rootfs absent). Give the kernel writeback a
	// full window before detaching. (Proper fix if one exists: an explicit
	// RouterOS unmount/sync verb.)
	p.deps.Logger.Infow("cow seeder: settling writeback before detach", "seconds", 45)
	time.Sleep(45 * time.Second)

	// Ask the ENGINE to commit before we take the volume away. The writes
	// reached stormblockmk — allocation grew to the full image size — so
	// what is lost is not in RouterOS's cache but in partially-filled slab
	// slots: bulk file data fills 4 MB slots and persists, scattered 4 KB
	// metadata does not. SYNCHRONIZE CACHE is exactly the command for that,
	// and RouterOS never sends it for a network disk.
	if fErr := p.flushStormblockVolume(ctx, sb, rawVolumeID, attach); fErr != nil {
		p.deps.Logger.Warnw("cow seeder: target flush failed", "error", fErr)
	}

	// EJECT before detaching. /disk/remove force-detaches without flushing,
	// so everything RouterOS still held in its page cache — the directory
	// entries and inode updates that make the extracted image findable —
	// was being lost, leaving a volume with 60 MB allocated that mounts
	// completely empty. /disk/eject unmounts properly first.
	if disks, derr := ros.ListDisks(ctx); derr == nil {
		for i := range disks {
			d := &disks[i]
			if d.MountPoint != "" && "/"+d.MountPoint == mountPoint {
				// Eject is for hardware disks only — RouterOS says so and
				// points at disable instead, which is the one remaining way
				// to make it release a filesystem without a force-detach.
				if eerr := ros.EjectDisk(ctx, d.ID); eerr == nil {
					p.deps.Logger.Infow("cow seeder: disk ejected (filesystem quiesced)", "disk", d.ID)
				} else if derr := ros.DisableDisk(ctx, d.ID); derr == nil {
					p.deps.Logger.Infow("cow seeder: disk disabled (filesystem quiesced)", "disk", d.ID)
				} else {
					p.deps.Logger.Warnw("cow seeder: could not quiesce the filesystem — metadata may be lost",
						"disk", d.ID, "ejectError", eerr, "disableError", derr)
				}
				break
			}
		}
	}
	time.Sleep(5 * time.Second)

	// Ordering that keeps the content: detach the disk FIRST (mount path
	// vanishes), THEN remove the seeder — its root-dir wipe misses.
	if disk, err := ros.FindNetworkDisk(ctx, "iscsi", "", ""); err == nil && disk != nil && "/"+strings.TrimPrefix(mountPoint, "/") == "/"+disk.MountPoint {
		_ = ros.RemoveDisk(ctx, disk.ID)
	} else {
		// Fallback: locate by mount point across all disks.
		if all, aerr := ros.ListDisks(ctx); aerr == nil {
			for i := range all {
				if all[i].MountPoint != "" && "/"+all[i].MountPoint == mountPoint {
					_ = ros.RemoveDisk(ctx, all[i].ID)
					break
				}
			}
		}
	}
	removeSeeder()
	return nil
}

// provisionCoWRoot clones the golden template for one container and returns
// the mounted rootfs path (…/rootfs) plus the volume id for teardown.
func (p *MicroKubeProvider) provisionCoWRoot(ctx context.Context, ros *routeros.Client, pod *corev1.Pod, containerName, templateName string) (string, string, error) {
	sb, err := p.newStormblockClient()
	if err != nil {
		return "", "", err
	}

	// Idempotent: a recreate reuses the pod's existing clone (its writes
	// included) instead of leaking a fresh volume per retry — 8 clones
	// leaked in the first live run's retry loop.
	if ann := pod.GetAnnotations(); ann != nil && ann[annCoWVolumeID] != "" {
		volID := ann[annCoWVolumeID]
		if attach, ok := p.findCoWVolumeAttach(ctx, sb, volID); ok {
			if diskID, aerr := p.attachStormblockDisk(ctx, attach); aerr == nil {
				if mp, merr := p.waitForDiskMount(ctx, ros, diskID, 90*time.Second); merr == nil {
					return p.cowPayloadRoot(mp), volID, nil
				}
				_ = ros.RemoveDisk(ctx, diskID)
			}
		}
		// Unusable — hand it back and provision fresh below.
		_ = sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+volID+"?force=true", nil, nil)
	}

	volName := fmt.Sprintf("cow-%s-%s-%s", pod.Namespace, pod.Name, containerName)
	reqBody := map[string]any{
		"name":          volName,
		"from_template": templateName,
		"export":        true,
	}
	if t := p.deps.Config.Storage.Stormblock.Transport; t != "" {
		reqBody["protocol"] = t
	}
	var created sbCreateVolumeResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", reqBody, &created); err != nil {
		return "", "", fmt.Errorf("cloning cow volume: %w", err)
	}
	attach, ok := created.attachParams()
	if !ok {
		if created.ID != "" {
			_ = sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil)
		}
		return "", "", fmt.Errorf("cow volume %s: no attach parameters", created.ID)
	}
	// NO re-identify here. The engine stamps every clone with a fresh
	// filesystem UUID at clone time (stamp_uuid defaults on, and it verifies
	// the result), so a clone already has its own identity before anything
	// sees it.
	//
	// Doing it again from this side actively broke the mount. Rewriting the
	// UUID invalidates every checksum seeded from it, and the volume tool
	// writes the superblock without recomputing any of them — it has no
	// checksum code at all — while these goldens are formatted with
	// metadata_csum. The result is a filesystem RouterOS probes as ext4 and
	// then refuses to mount: `fs=ext4` with an empty mount-point, for the
	// full 120s wait, on every CoW pod. A clone of the same golden attached
	// by hand, without this step, mounts in under ten seconds.

	diskID, err := p.attachStormblockDisk(ctx, attach)
	if err != nil {
		_ = sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil)
		return "", "", fmt.Errorf("attaching cow volume: %w", err)
	}
	// Clone carries the sealed ext4 — RouterOS probes it at attach and mounts.
	mountPoint, err := p.waitForDiskMount(ctx, ros, diskID, 120*time.Second)
	if err != nil {
		_ = ros.RemoveDisk(ctx, diskID)
		_ = sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil)
		return "", "", fmt.Errorf("waiting for cow volume mount: %w", err)
	}

	// No /file-based content verification here: RouterOS's file index lags
	// freshly mounted disks (a just-mounted clone lists as empty for a
	// while) and a full /file print costs minutes — the diagnostic check
	// this replaced both lied and stalled every provision. The container
	// start is the real verification.
	return p.cowPayloadRoot(mountPoint), created.ID, nil
}

// findCoWVolumeAttach locates an existing volume by id and returns its
// attach parameters when it still has an active export.
func (p *MicroKubeProvider) findCoWVolumeAttach(ctx context.Context, sb *sbClient, volID string) (sbAttach, bool) {
	var list struct {
		Items []sbCreateVolumeResp `json:"items"`
	}
	if err := sb.do(ctx, http.MethodGet, "/mk/v1/volumes", nil, &list); err != nil {
		return sbAttach{}, false
	}
	for i := range list.Items {
		if list.Items[i].ID == volID {
			return list.Items[i].attachParams()
		}
	}
	return sbAttach{}, false
}

// deprovisionCoWRoot detaches and returns a pod's cow volume.
func (p *MicroKubeProvider) deprovisionCoWRoot(ctx context.Context, ros *routeros.Client, volumeID string) {
	if volumeID == "" {
		return
	}
	sb, err := p.newStormblockClient()
	if err != nil {
		return
	}
	// Find and detach any disk still consuming this volume's target.
	if all, aerr := ros.ListDisks(ctx); aerr == nil {
		for i := range all {
			d := &all[i]
			if (d.Type == "iscsi" && strings.Contains(d.ISCSIIQN, volumeID)) ||
				(d.Type == "nvme-tcp" && strings.Contains(d.NVMeTCPNQN, volumeID)) {
				_ = ros.RemoveDisk(ctx, d.ID)
			}
		}
	}
	if err := sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+volumeID+"?force=true", nil, nil); err != nil {
		p.deps.Logger.Warnw("cow volume cleanup failed", "volume", volumeID, "error", err)
	}
}

// rewriteEntrypointForCoW maps the effective entrypoint/cmd into the
// mounted payload. Pod-spec command/args win over the image config, same
// precedence as the normal path.
func rewriteEntrypointForCoW(pod *corev1.Pod, c *corev1.Container, imgCfg *dockerSaveConfig) (entrypoint string, cmd string) {
	argv0 := ""
	var rest []string
	switch {
	case len(c.Command) > 0:
		argv0 = c.Command[0]
		rest = append(append([]string{}, c.Command[1:]...), c.Args...)
	case imgCfg != nil && len(imgCfg.Entrypoint) > 0:
		argv0 = imgCfg.Entrypoint[0]
		rest = append(append([]string{}, imgCfg.Entrypoint[1:]...), firstNonEmptySlice(c.Args, imgCfg.Cmd)...)
	case imgCfg != nil && len(imgCfg.Cmd) > 0:
		argv0 = imgCfg.Cmd[0]
		rest = append(append([]string{}, imgCfg.Cmd[1:]...), c.Args...)
	}
	if argv0 == "" {
		return "", ""
	}
	if !strings.HasPrefix(argv0, cowPayloadDst+"/") {
		argv0 = cowPayloadDst + "/" + strings.TrimPrefix(argv0, "/")
	}
	return argv0, strings.Join(rest, " ")
}

func firstNonEmptySlice(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// cowGuardPodForSeeding is documented in cow_probe.go (cowProbeGuardPod);
// the seeder reuses the probe's guarded identity so every reaper
// (orphan-container sweep, veth sweep, CreatePod stale cleanup) skips it.
var _ = metav1.Now // keep metav1 import anchored for future use

// prewarmGoldenTemplates builds golden templates for cow-mode pods using the
// pushed repo. Kicked from registry push events, so the ONE untar an image
// ever gets overlaps the push instead of stalling the first pod create —
// by create time the template is sealed and the pod takes the ~2s clone
// path. One prewarm in flight per repo; a new digest gets a new template
// (old digests' templates are left for a future GC).
func (p *MicroKubeProvider) prewarmGoldenTemplates(repo string) {
	if !p.cowPrewarm.SetIfAbsent(repo, true) {
		return
	}
	go func() {
		defer p.cowPrewarm.Delete(repo)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		ros := p.getRouterOSClient()
		if ros == nil {
			return
		}
		seen := map[string]bool{}
		for _, pod := range p.allDesiredPods(ctx) {
			if !isCoWPod(pod) {
				continue
			}
			for i := range pod.Spec.Containers {
				img := pod.Spec.Containers[i].Image
				if repo != "" && !strings.Contains(img, repo) {
					continue
				}
				if seen[img] {
					continue
				}
				seen[img] = true
				tarball, err := p.deps.StorageMgr.EnsureImage(ctx, img)
				if err != nil {
					p.deps.Logger.Warnw("cow prewarm: ensure image", "image", img, "error", err)
					continue
				}
				digest := p.deps.StorageMgr.TarballDigest(tarball)
				if digest == "" {
					continue
				}
				if strings.EqualFold(p.deps.Config.Storage.Stormblock.GoldenSource, goldenSourceSbRegistry) {
					// The external builder owns creation; a prewarm here would
					// only wait. Record what we expect so the gap is visible.
					p.deps.Logger.Infow("cow prewarm: awaiting external golden", "image", img, "template", cowTemplateName(digest))
					continue
				}
				if _, terr := p.ensureGoldenTemplate(ctx, ros, img, tarball, digest); terr != nil {
					p.deps.Logger.Warnw("cow prewarm: golden template", "image", img, "error", terr)
				} else {
					p.deps.Logger.Infow("cow prewarm: golden template ready at push time", "image", img, "digest", digest)
				}
			}
		}
	}()
}
