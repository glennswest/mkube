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

// sbTemplate is a stormblockmk fstemplate row.
type sbTemplate struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// sbCreateTemplateResp is the create-template response: the template row
// plus the attach block for the formatting/seeding phase.
type sbCreateTemplateResp struct {
	Template sbTemplate      `json:"template"`
	Attach   json.RawMessage `json:"attach"`
}

// ensureGenericStub makes sure the generic CoW stub archive exists on the
// device (one placeholder file; docker-save layout).
func (p *MicroKubeProvider) ensureGenericStub(ctx context.Context, ros *routeros.Client) error {
	if ok, err := ros.FileExists(ctx, cowStubDevicePath); err == nil && ok {
		return nil
	}
	return ros.UploadFile(ctx, cowStubDevicePath, bytes.NewReader(cowStubTar()))
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
	if err := sb.do(ctx, http.MethodGet, "/mk/v1/fstemplates", nil, &list); err != nil {
		return "", fmt.Errorf("listing fstemplates: %w", err)
	}
	for _, t := range list.Items {
		if t.Name == name {
			if strings.EqualFold(t.State, "ready") || strings.EqualFold(t.State, "sealed") {
				return name, nil
			}
			// Half-built template from a crashed attempt — remove and rebuild.
			log.Warnw("removing half-built fstemplate", "template", name, "state", t.State)
			if derr := sb.do(ctx, http.MethodDelete, "/mk/v1/fstemplates/"+t.ID+"?force=true", nil, nil); derr != nil {
				return "", fmt.Errorf("removing half-built fstemplate %s: %w", name, derr)
			}
		}
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
	var created sbCreateTemplateResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/fstemplates",
		map[string]any{"name": name, "fs": "ext4", "size_bytes": sizeBytes}, &created); err != nil {
		return "", fmt.Errorf("creating fstemplate %s: %w", name, err)
	}

	// The attach field is the full wiring object: {protocol, state,
	// attach:{address,port,iqn,...}} — same shape as a volume export.
	var wiring sbExport
	if err := json.Unmarshal(created.Attach, &wiring); err != nil || wiring.Attach.Address == "" {
		return "", fmt.Errorf("fstemplate %s returned no usable attach block", name)
	}
	attach := wiring.Attach
	if attach.Transport == "" {
		attach.Transport = wiring.Protocol
	}
	if attach.Transport == "" {
		attach.Transport = "iscsi" // template formatting attach is documented iSCSI
	}

	// Attach + format + probe-kick + mount — same dance as stormblock PVCs.
	diskID, err := p.attachStormblockDisk(ctx, attach)
	if err != nil {
		return "", fmt.Errorf("attaching template volume: %w", err)
	}
	cleanupDisk := func() { _ = ros.RemoveDisk(ctx, diskID) }

	formatPortal := attach.Address
	if attach.Port != 0 {
		formatPortal = fmt.Sprintf("%s:%d", attach.Address, attach.Port)
	}
	if err := p.formatISCSITargetExt4(ctx, formatPortal, sbTargetName(attach), name); err != nil {
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
	if err := p.runCoWSeeder(ctx, ros, digest, tarballPath, mountPoint); err != nil {
		cleanupDisk()
		return "", fmt.Errorf("seeding template: %w", err)
	}

	// Detach BEFORE seal (seal refuses attachments), then seal, then the
	// seeder container is already gone (runCoWSeeder removes it after the
	// detach inside — see ordering note there).
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/fstemplates/"+created.Template.ID+"/seal?force=true", nil, nil); err != nil {
		return "", fmt.Errorf("sealing fstemplate %s: %w", name, err)
	}
	log.Infow("golden template sealed", "template", name)
	return name, nil
}

// runCoWSeeder extracts the image tarball onto the mounted template volume
// via a throwaway container, then detaches the volume and removes the
// seeder (whose root-dir wipe then hits a path that no longer resolves).
func (p *MicroKubeProvider) runCoWSeeder(ctx context.Context, ros *routeros.Client, digest, tarballPath, mountPoint string) error {
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

	// Wait for extraction to finish (container reaches stopped).
	extracted := false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		ros.InvalidateContainerCache()
		if ct, err := ros.GetContainer(ctx, seederName); err == nil && ct.IsStopped() {
			extracted = true
			break
		}
		time.Sleep(3 * time.Second)
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
					return mp + "/rootfs", volID, nil
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
	return mountPoint + "/rootfs", created.ID, nil
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
