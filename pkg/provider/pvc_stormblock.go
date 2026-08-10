package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
				p.setISCSIPVCAnnotations(ctx, pvc, disk.ID, disk.Slot, mp, disk.ISCSIServerIQN, ann[annSBPortal])
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
	// `from_template` is the mkfs-once path: the volume is a CoW clone of a
	// pre-formatted filesystem, so there is nothing to format here and the
	// clone costs metadata rather than a full format. Without a template
	// configured we fall back to formatting below.
	name := fmt.Sprintf("pvc-%s-%s", pvc.Namespace, pvc.Name)
	reqBody := map[string]any{
		"name":       name,
		"size_bytes": sizeBytes,
		"export":     true,
	}
	template := p.deps.Config.Storage.Stormblock.Template
	if template != "" {
		reqBody["from_template"] = template
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

	mountPoint, err := p.waitForDiskMount(ctx, rosClient, diskID, 120*time.Second)
	if err != nil {
		// A fresh volume from a template already carries a filesystem; a raw
		// one does not, and RouterOS will not mount what it cannot recognise.
		if template == "" {
			log.Infow("formatting stormblock volume as ext4", "diskID", diskID)
			if fErr := p.formatISCSITargetExt4(ctx, attach.Address, sbTargetName(attach), name); fErr != nil {
				return "", fmt.Errorf("formatting stormblock volume: %w", fErr)
			}
			mountPoint, err = p.waitForDiskMount(ctx, rosClient, diskID, 120*time.Second)
		}
		if err != nil {
			return "", fmt.Errorf("waiting for stormblock disk to mount: %w", err)
		}
	}

	disk, err := rosClient.GetISCSIDisk(ctx, diskID)
	slot := ""
	if err == nil {
		slot = disk.Slot
	}
	portal := fmt.Sprintf("%s:%d", attach.Address, attach.Port)
	p.setISCSIPVCAnnotations(ctx, pvc, diskID, slot, mountPoint, sbTargetName(attach), portal)
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
	// iSCSI portals are addressed host:port; the NVMe initiator takes the
	// address alone and finds the subsystem by NQN.
	address := a.Address
	if transport == "iscsi" && a.Port != 0 {
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
