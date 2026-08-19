package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// stormblock-backed PVCs: thin, copy-on-write volumes served by stormblockmk
// and attached by the RouterOS initiator.
//
// The difference from the iSCSI PVCs in pvc_iscsi.go is only *who provisions
// the block device*. There, RouterOS itself owns a sparse file and exports it.
// Here stormblockmk owns a thin volume in its slab and exports it on a
// dedicated target — which buys CoW snapshots, instant clones from a
// pre-formatted filesystem template (no mkfs per PVC), thin overcommit, and a
// volume that can later be served to a bare-metal host over NVMe-TCP instead.
//
// Everything downstream of the attach is shared with the iSCSI path: RouterOS
// mounts the disk at /<slot>, waitForDiskMount discovers where, and the same
// annotations record the result so provisioning is idempotent.
const (
	pvcTypeStormblock = "stormblock"

	// Annotations owned by this path (annDiskID/annSlot/annMountPoint/annDiskIQN
	// are shared with the iSCSI path and mean the same thing).
	annSBVolumeID = "vkube.io/stormblock-volume-id"
	annSBExportID = "vkube.io/stormblock-export-id"
	annSBPortal   = "vkube.io/stormblock-portal"
)

// volumeToolBinary is the NVMe/TCP volume tool mkube ships in its own image:
// format, re-identify, flush and pattern-verify a stormblock volume by
// talking to the export directly.
//
// Overridable because it is the one path mkube resolves inside its own image
// rather than through a RouterOS mount. Everything else it opens — /etc/mkube,
// /data — arrives on a mount and lands at the same place whatever the
// container's root is. So when mkube runs from a CoW clone, where the image
// sits under the mount point rather than at /, this is the single value that
// has to move with it. MKUBE_VOLUME_TOOL is how the launcher says where.
var volumeToolBinary = envOr("MKUBE_VOLUME_TOOL", "/usr/local/bin/nvme-pvc")

// envOr returns the environment variable's value, or a default when unset or
// empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isStormblockPVC reports whether a PVC asks for a stormblock volume.
func isStormblockPVC(pvc *corev1.PersistentVolumeClaim) bool {
	if pvc == nil {
		return false
	}
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == pvcTypeStormblock {
		return true
	}
	return pvc.Annotations[annPVCType] == pvcTypeStormblock
}

// sbAttach is the attach parameter block stormblockmk returns for an export.
// Both transports are represented; Transport distinguishes them.
type sbAttach struct {
	Transport string `json:"transport"` // "" or absent ⇒ iSCSI, "nvme-tcp" ⇒ NVMe
	Address   string `json:"address"`
	Port      int    `json:"port"`
	IQN       string `json:"iqn"`
	NQN       string `json:"nqn"`
	LUN       int    `json:"lun"`
}

// sbExport mirrors stormblockmk's export object: the transport lives in
// `protocol` and the attach parameters are nested one level down in `attach`
// (verified against a live /mk/v1/volumes response 2026-08-10 — parsing the
// export as a flat attach block left every field empty and provisioning
// failed "no attach parameters returned").
type sbExport struct {
	ExportID string   `json:"export_id"`
	Protocol string   `json:"protocol"` // "iscsi" | "nvme-tcp"
	State    string   `json:"state"`
	Attach   sbAttach `json:"attach"`
}

type sbCreateVolumeResp struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Export *sbExport `json:"export"`
}

// attachParams flattens the export into the attach block the rest of the
// flow consumes, stamping the transport from the export's protocol.
func (r *sbCreateVolumeResp) attachParams() (sbAttach, bool) {
	if r.Export == nil || r.Export.Attach.Address == "" {
		return sbAttach{}, false
	}
	a := r.Export.Attach
	if a.Transport == "" {
		a.Transport = r.Export.Protocol
	}
	return a, true
}

// sbClient is a minimal client for stormblockmk's /mk/v1 surface.
type sbClient struct {
	base   string
	token  string
	client *http.Client
}

// newStormblockClient builds a client from config, or returns nil when no
// endpoint is configured (in which case stormblock PVCs are unavailable and
// the caller reports that clearly rather than silently falling back to a
// directory-backed volume, which would lose the semantics the user asked for).
func (p *MicroKubeProvider) newStormblockClient() (*sbClient, error) {
	cfg := p.deps.Config.Storage.Stormblock
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("storage.stormblock.endpoint is not configured")
	}
	// Secret first: keeps the token out of the config file (and therefore out
	// of git), and it is already decrypted in memory by the Secret store.
	token := ""
	if cfg.TokenSecret != "" {
		ns, name, ok := strings.Cut(cfg.TokenSecret, "/")
		if !ok {
			return nil, fmt.Errorf("storage.stormblock.tokenSecret %q must be \"namespace/name\"", cfg.TokenSecret)
		}
		secret, found := p.secrets.Get(ns + "/" + name)
		if !found {
			return nil, fmt.Errorf("stormblock token Secret %s not found", cfg.TokenSecret)
		}
		key := cfg.TokenSecretKey
		if key == "" {
			key = "token"
		}
		raw, has := secret.Data[key]
		if !has {
			return nil, fmt.Errorf("stormblock token Secret %s has no key %q", cfg.TokenSecret, key)
		}
		token = strings.TrimSpace(string(raw))
	}
	if token == "" {
		token = cfg.Token
	}
	if token == "" && cfg.TokenFile != "" {
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading stormblock token from %s: %w", cfg.TokenFile, err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		// stormblockmk requires a bearer token unless it was started with
		// STORMBLOCKMK_INSECURE=1; an unauthenticated call would 401 and the
		// PVC would fail with a confusing error (mkube#15).
		p.deps.Logger.Warnw("no stormblock API token configured — calls will fail unless the target is running insecure",
			"endpoint", cfg.Endpoint)
	}
	return &sbClient{
		base:   strings.TrimSuffix(cfg.Endpoint, "/"),
		token:  token,
		client: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *sbClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(buf.String()))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// sbToolTarget renders an export's address as the "host:port" the nvme-pvc
// tool expects.
//
// The port is mandatory. stormblockmk gives every export its own port and
// the shared 4420 serves discovery only, so an address without one reaches a
// controller that answers the handshake and then fails every I/O.
func sbToolTarget(a sbAttach) (string, error) {
	if a.Address == "" {
		return "", fmt.Errorf("export carries no address")
	}
	if a.Port == 0 {
		return "", fmt.Errorf("export %s carries no port", sbTargetName(a))
	}
	return fmt.Sprintf("%s:%d", a.Address, a.Port), nil
}

// runVolumeTool invokes nvme-pvc against a volume's own export.
//
// Every one of these operations used to have a second code path that created
// a TEMPORARY iSCSI export, because the tool spoke only iSCSI and could not
// reach an NVMe-exported volume through its own export. The tool now speaks
// NVMe/TCP, so the volume is addressed directly and that fallback is gone.
func (p *MicroKubeProvider) runVolumeTool(ctx context.Context, attach sbAttach, op string, extra ...string) (string, error) {
	target, err := sbToolTarget(attach)
	if err != nil {
		return "", err
	}
	// Bound the tool independently of the caller's deadline. A protocol
	// mistake against a controller that simply stops answering would
	// otherwise hold a pod-worker slot for as long as the caller allows.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	args := []string{
		"--url", p.deps.Config.RouterOS.RESTURL,
		"--user", p.deps.Config.RouterOS.User,
		"--password", p.deps.Config.RouterOS.Password,
		"--target", target,
		op, sbTargetName(attach),
	}
	args = append(args, extra...)
	out, err := exec.CommandContext(ctx, volumeToolBinary, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nvme-pvc %s: %w: %s", op, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// formatStormblockVolume formats a freshly-created volume as ext4 over its
// own NVMe export.
func (p *MicroKubeProvider) formatStormblockVolume(ctx context.Context, sb *sbClient, volumeID string, attach sbAttach, label string) error {
	out, err := p.runVolumeTool(ctx, attach, "format", "--label", label)
	if err != nil {
		return err
	}
	p.deps.Logger.Infow("volume formatted ext4", "volume", volumeID, "label", label, "output", out)
	return nil
}

// flushStormblockVolume issues an NVMe FLUSH so the engine commits everything
// it is holding.
//
// stormblock allocates in 4 MB slab slots and implements FLUSH as
// device.flush(). Without it, small scattered writes — filesystem metadata —
// can sit in partial slab slots while bulk file data that fills whole slots
// persists, which is how a volume ends up mounting cleanly and empty.
func (p *MicroKubeProvider) flushStormblockVolume(ctx context.Context, sb *sbClient, volumeID string, attach sbAttach) error {
	if _, err := p.runVolumeTool(ctx, attach, "flush"); err != nil {
		return err
	}
	p.deps.Logger.Infow("volume cache flushed", "volume", volumeID)
	return nil
}

// reidentifyStormblockVolume gives a volume's filesystem a fresh UUID (and
// label), optionally restoring the "cleanly unmounted" flag.
//
// A CoW clone is byte-identical to its golden, so without this every clone
// on the host claims the same filesystem identity — which is what makes
// mount-by-UUID and blkid caching misbehave. `clean` additionally repairs
// the flag left cleared by a force-detach; only pass it once writes have
// quiesced.
func (p *MicroKubeProvider) reidentifyStormblockVolume(ctx context.Context, sb *sbClient, volumeID string, attach sbAttach, label string, clean bool) error {
	extra := []string{"--label", label}
	if clean {
		extra = append(extra, "--clean")
	}
	out, err := p.runVolumeTool(ctx, attach, "reid", extra...)
	if err != nil {
		return err
	}
	p.deps.Logger.Infow("filesystem re-identified",
		"volume", volumeID, "label", label, "clean", clean, "output", out)
	return nil
}

// provisionStormblockPVC creates (or recovers) the volume behind a PVC and
// returns the host path RouterOS mounted it at.
//
// Idempotent: a PVC whose annotations already name a mounted disk short-circuits,
// and a PVC whose disk exists but whose mount point was never recorded recovers
// it from RouterOS rather than provisioning a second volume.
func (p *MicroKubeProvider) provisionStormblockPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	log := p.deps.Logger.With("pvc", pvc.Namespace+"/"+pvc.Name, "type", pvcTypeStormblock)

	rosClient := p.getRouterOSClient()
	if rosClient == nil {
		return "", fmt.Errorf("stormblock PVC requires the RouterOS backend")
	}

	// Already provisioned?
	if ann := pvc.GetAnnotations(); ann != nil && ann[annSBVolumeID] != "" {
		if mp := ann[annMountPoint]; mp != "" {
			log.Debugw("stormblock PVC already provisioned", "mountPoint", mp)
			return mp, nil
		}
		if id := ann[annDiskID]; id != "" {
			if disk, err := rosClient.GetISCSIDisk(ctx, id); err == nil && disk.MountPoint != "" {
				mp := "/" + disk.MountPoint
				p.setPVCDiskAnnotations(ctx, pvc, pvcTypeStormblock, disk.ID, disk.Slot, mp, disk.ISCSIServerIQN, ann[annSBPortal])
				return mp, nil
			}
		}
	}

	sb, err := p.newStormblockClient()
	if err != nil {
		return "", err
	}

	sizeBytes := parsePVCSize(pvc)
	if sizeBytes <= 0 {
		sizeBytes = 100 * 1024 * 1024
	}

	// Ask stormblockmk for a thin volume, exported and ready to attach.
	//
	// `from_template` is how a PVC gets a filesystem at all: the volume is a
	// CoW clone of a template the registry built and formatted. A raw volume
	// comes back empty (verified live 2026-08-11: fs=- on a freshly created
	// volume), so without a template there is nothing for RouterOS to mount.
	name := fmt.Sprintf("pvc-%s-%s", pvc.Namespace, pvc.Name)
	reqBody := map[string]any{
		"name":       name,
		"size_bytes": sizeBytes,
		"export":     true,
	}
	// Which pre-formatted template to clone. Chosen per claim from the size
	// ladder stormblockmk publishes, because one configured name cannot
	// serve claims of different sizes.
	template, err := p.pickStormblockTemplate(ctx, sb, sizeBytes)
	if err != nil {
		return "", err
	}
	reqBody["from_template"] = template
	// Transport for the export. stormblockmk honors this from v0.3.0; mkube
	// attaches whatever protocol the export actually reports, so a target
	// that ignored the field would still be attached correctly.
	if t := p.deps.Config.Storage.Stormblock.Transport; t != "" {
		reqBody["protocol"] = t
	}
	var created sbCreateVolumeResp
	if err := sb.do(ctx, http.MethodPost, "/mk/v1/volumes", reqBody, &created); err != nil {
		return "", fmt.Errorf("creating stormblock volume: %w", err)
	}
	attach, ok := created.attachParams()
	if !ok {
		// Roll back — a created-but-unusable volume would leak space and,
		// if exported, a target and port (this exact leak happened live
		// 2026-08-10 while the response shape was mis-parsed).
		if created.ID != "" {
			if delErr := sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil); delErr != nil {
				log.Warnw("could not roll back stormblock volume", "volumeID", created.ID, "error", delErr)
			}
		}
		return "", fmt.Errorf("stormblock volume %s created but no attach parameters returned", created.ID)
	}
	log.Infow("stormblock volume provisioned",
		"volumeID", created.ID, "size", sizeBytes, "template", template,
		"transport", sbTransport(attach), "target", sbTargetName(attach))

	// Attach it with the RouterOS initiator. Both transports are addressed by
	// (address, target name) — the per-volume target means no LUN/namespace
	// selection is needed, which is the whole point of stormblockmk's
	// per-export addressing.
	diskID, err := p.attachStormblockDisk(ctx, attach)
	if err != nil {
		// Roll the volume back: leaving an exported-but-unattached volume
		// behind would leak a target, a port and the space.
		if delErr := sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil); delErr != nil {
			log.Warnw("could not roll back stormblock volume", "volumeID", created.ID, "error", delErr)
		}
		return "", err
	}

	// Roll back both the attach and the volume when anything past this point
	// fails — a failed format left an attached disk and an exported volume
	// behind (observed live 2026-08-10), and the next attempt provisions a
	// fresh volume, so partial state only leaks.
	rollback := func() {
		if err := rosClient.RemoveDisk(ctx, diskID); err != nil {
			log.Warnw("could not roll back attached stormblock disk", "diskID", diskID, "error", err)
		}
		if delErr := sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+created.ID+"?force=true", nil, nil); delErr != nil {
			log.Warnw("could not roll back stormblock volume", "volumeID", created.ID, "error", delErr)
		}
	}

	// mkube does not format. A stormblock PVC is a CoW clone of a template
	// the registry built and formatted, so the filesystem exists before the
	// volume does and a clone costs metadata rather than a full mkfs.
	//
	// This used to fall back to formatting a raw volume in place, which also
	// needed a detach/re-attach afterwards because RouterOS probes a
	// consumed target's filesystem at ATTACH time only. Both are gone: with
	// the clone arriving pre-formatted there is nothing to write and nothing
	// to re-probe.
	mountPoint, err := p.waitForDiskMount(ctx, rosClient, diskID, 120*time.Second)
	if err != nil {
		rollback()
		return "", fmt.Errorf("stormblock volume %s (clone of template %s) did not mount: %w",
			created.ID, template, err)
	}

	disk, err := rosClient.GetISCSIDisk(ctx, diskID)
	slot := ""
	if err == nil {
		slot = disk.Slot
	}
	portal := fmt.Sprintf("%s:%d", attach.Address, attach.Port)
	p.setPVCDiskAnnotations(ctx, pvc, pvcTypeStormblock, diskID, slot, mountPoint, sbTargetName(attach), portal)
	p.setStormblockPVCAnnotations(ctx, pvc, created.ID, portal)
	log.Infow("stormblock PVC ready", "mountPoint", mountPoint, "slot", slot, "portal", portal)
	return mountPoint, nil
}

// attachStormblockDisk attaches the exported volume with the RouterOS
// initiator and returns the disk id.
//
// The transport comes from the attach block stormblockmk returned, so mkube
// follows whatever the volume was actually exported as rather than assuming.
func (p *MicroKubeProvider) attachStormblockDisk(ctx context.Context, a sbAttach) (string, error) {
	ros := p.getRouterOSClient()
	if ros == nil {
		return "", fmt.Errorf("no RouterOS client")
	}
	transport := sbTransport(a)
	// Both transports are addressed host:port. stormblockmk gives every
	// export its OWN port — the shared portal (3260 iSCSI / 4420 NVMe) is a
	// discovery endpoint that does not serve per-volume targets — so the
	// port is not optional on either side.
	//
	// Dropping it for NVMe sent the initiator to RouterOS's built-in default
	// of 4420 and asked there for a per-volume NQN that port never serves.
	// The attach still "succeeded" (the row appears) and then sat at
	// `state=I/O error, block-device=false, read-ops=0`, which read exactly
	// like "RouterOS cannot mount NVMe" and cost a day of chasing the
	// initiator instead of the address.
	address := a.Address
	if a.Port != 0 {
		address = fmt.Sprintf("%s:%d", a.Address, a.Port)
	}
	id, err := ros.AttachNetworkDisk(ctx, transport, address, sbTargetName(a))
	if err != nil {
		return "", fmt.Errorf("attaching %s %s: %w", transport, sbTargetName(a), err)
	}
	return id, nil
}

// deprovisionStormblockPVC detaches the disk and hands the volume back.
//
// Order matters: detach first so the initiator is gone before the target is
// withdrawn, then delete the volume — stormblockmk's own teardown ladder
// (active → draining → withdrawn) waits for the session to clear before it
// pulls the LUN, and deleting from this end while still attached would rely on
// that grace period instead of a clean handshake.
func (p *MicroKubeProvider) deprovisionStormblockPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	ann := pvc.GetAnnotations()
	if ann == nil || ann[annSBVolumeID] == "" {
		return nil
	}
	log := p.deps.Logger.With("pvc", pvc.Namespace+"/"+pvc.Name, "type", pvcTypeStormblock)

	if id := ann[annDiskID]; id != "" {
		if ros := p.getRouterOSClient(); ros != nil {
			if err := ros.RemoveDisk(ctx, id); err != nil {
				log.Warnw("detaching stormblock disk", "diskID", id, "error", err)
			} else {
				log.Infow("stormblock disk detached", "diskID", id)
			}
		}
	}

	sb, err := p.newStormblockClient()
	if err != nil {
		return err
	}
	volumeID := ann[annSBVolumeID]
	if err := sb.do(ctx, http.MethodDelete, "/mk/v1/volumes/"+volumeID+"?force=true", nil, nil); err != nil {
		return fmt.Errorf("deleting stormblock volume %s: %w", volumeID, err)
	}
	log.Infow("stormblock volume deleted", "volumeID", volumeID)
	return nil
}

func (p *MicroKubeProvider) setStormblockPVCAnnotations(ctx context.Context, pvc *corev1.PersistentVolumeClaim, volumeID, portal string) {
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[annPVCType] = pvcTypeStormblock
	pvc.Annotations[annSBVolumeID] = volumeID
	if portal != "" {
		pvc.Annotations[annSBPortal] = portal
	}
	key := pvc.Namespace + "/" + pvc.Name
	p.pvcs.Set(key, pvc)
	if p.deps.Store != nil && p.deps.Store.PersistentVolumeClaims != nil {
		storeKey := pvc.Namespace + "." + pvc.Name
		if _, err := p.deps.Store.PersistentVolumeClaims.PutJSON(ctx, storeKey, pvc); err != nil {
			p.deps.Logger.Warnw("persisting stormblock PVC annotations", "pvc", key, "error", err)
		}
	}
}

// sbTransport names the transport in an attach block.
func sbTransport(a sbAttach) string {
	if a.Transport != "" {
		return a.Transport
	}
	return "iscsi"
}

// sbTargetName is the IQN or NQN, whichever this export uses.
func sbTargetName(a sbAttach) string {
	if a.NQN != "" {
		return a.NQN
	}
	return a.IQN
}

// sbProtocol is the transport mkube asks stormblockmk to export with.
//
// Probes and the PVC path both go through this rather than naming a
// transport inline, so there is exactly one place that decides and no way
// for a helper to quietly disagree with the configured data path.
func (p *MicroKubeProvider) sbProtocol() string {
	if t := p.deps.Config.Storage.Stormblock.Transport; t != "" {
		return t
	}
	return "nvme-tcp"
}
