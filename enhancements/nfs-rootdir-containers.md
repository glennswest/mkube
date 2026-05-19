# Switch RouterOS container creation from tarball-extract to NFS root-dir

**Status:** proposed — 2026-05-19
**Owner:** glennswest
**Scope:** MikroTik (RouterOS) only. OpenShift / Proxmox / StormBase keep their existing flows. Replaces mkube-update's "pull blob → `tar -x` into `/raid1/images/<name>` → `/container/add file=...`" cycle with "ask registry for an NFS export of image:tag → `/container/add root-dir=nfs://...`".

## Motivation

Container start latency on rose1 is dominated by **tarball extraction**: each new container does a full re-extract of every layer into its own `/raid1/images/<name>` directory. With ~20 containers on rose1 sharing 4–5 base images, that's gigabytes of redundant on-disk data and minutes of cold-start time.

RouterOS containers accept `root-dir=nfs://…` directly (verified empirically; see `[where?]`). Combined with the [rspacefs-registry backend](rspacefs-registry-backend.md) and the [nextnfs rspacefs export type](../../nextnfs/enhancements/rspacefs-export-type.md), we can replace the entire extract-to-disk step with a live NFS mount whose backing is the registry's already-extracted layer pool.

End state:

```
        old                              new
─────────────────────         ─────────────────────────
podman push image:tag         podman push image:tag
        ↓                             ↓
mkube-update polls            mkube-update polls
        ↓                             ↓
fetch tarball blob            POST /v1/exports {image, container_id}
        ↓                             ↓
tar -x into /raid1/...        nfs:// URL ← registry composes lowers + upper
        ↓                             ↓
/container/add file=...       /container/add root-dir=nfs://...
                                     (container reads through NFS;
                                      writes go to per-container upper)
```

Win: container start drops from extract-time (seconds to minutes) to mount-time (~tens of ms). Storage drops to one copy of each layer plus per-container uppers.

## Architectural shape

```
   ┌────────────────────────┐
   │  rose1 (RouterOS)       │
   │  ┌──────────────────┐   │
   │  │ container A      │───┼──────►  nfs://kube.gt.lo/exports/A
   │  │  root-dir=nfs:// │   │              │
   │  └──────────────────┘   │              │
   │  ┌──────────────────┐   │              │
   │  │ container B      │───┼──────►  nfs://kube.gt.lo/exports/B
   │  │  root-dir=nfs:// │   │              │
   │  └──────────────────┘   │              │
   └────────────────────────┘              │
                                           ▼
                          ┌────────────────────────────────┐
                          │  mkube container (kube.gt.lo)   │
                          │  ┌──────────────────────────┐  │
                          │  │ nextnfs                   │  │
                          │  │  rspacefs export A,B,...  │──┐
                          │  └──────────────────────────┘  │ │
                          │  ┌──────────────────────────┐  │ │
                          │  │ mkube-registry            │  │ │
                          │  │  layers/sha256/<digest>/  │◄─┘ │
                          │  │  exports/A/upper          │    │
                          │  │  exports/B/upper          │    │
                          │  └──────────────────────────┘    │
                          └──────────────────────────────────┘
```

mkube-registry and nextnfs run in the same `kube.gt.lo` container — they share a filesystem so the registry's layer pool is the lowers and `exports/<id>/upper` is the writable upper. No data copying between them.

## Changes in mkube

### `pkg/provider/` — container creation path

Today (rough call graph):
```
CreatePod → pullAndExtractTarball → reconcileMounts → RouterOS /container/add file=<tarball>
```

New:
```
CreatePod
  → registryClient.CreateExport(image, container_id)        // POST /v1/exports
  → returns { nfs_url }
  → reconcileMounts (unchanged)
  → RouterOS /container/add root-dir=<nfs_url>, name=..., ...
```

Touched files:
- `pkg/provider/provider.go` — `CreatePod`, switch on container backend
- `pkg/runtime/routeros.go` — accept `root-dir` URL value, not just a local path
- `pkg/routeros/client.go` — `CreateContainer` signature gains an optional `RootDirURL` field
- `pkg/registry/client.go` (new) — small REST client for `POST /v1/exports`

### `cmd/mkube-update/` — image-policy=auto path

Today it does the same extract; replace with the same call:
```
On digest change:
  registryClient.CreateExport(...) → nfs_url
  StopContainer(old) → RemoveContainer(old) → CreateContainer(new root-dir=nfs_url)
```

The old tarball-cache directory under `/raid1/images/<name>` is no longer needed for nfs-rootdir containers. Tear-down on migration cleans those up.

### `deploy/rose1-config.yaml` — config flag

Per-network or per-pod opt-in:
```yaml
backend: routeros
container_rootdir:
  default: nfs                # was: tarball
  per_namespace:
    infra: tarball            # keep specific things on the old path
```

Default `tarball` initially (safety); flip to `nfs` once stable.

## Changes in RouterOS — none

RouterOS 7.20+ accepts `root-dir` as an `nfs://` URL on `/container/add`. We're already on 7.22.2 on rose1. No image, kernel, or driver changes on the device.

## Failure modes & mitigations

| Failure | Today (tarball) | New (NFS) | Mitigation |
|---|---|---|---|
| nextnfs daemon down | n/a (no NFS) | Container can't read its rootfs | mkube-update watchdog already restarts nextnfs if it stops. Also: keep tarball flow available as fallback per `container_rootdir` config. |
| Registry restart mid-pod-create | Retry the extract | Retry the `POST /v1/exports`; the upper-dir is idempotent (re-use existing `exports/<container_id>/upper`). | Spec API as idempotent on `container_id`. |
| NFS hang | n/a | Container blocks on I/O | Mount options `soft,timeo=30,retrans=3` so I/O fails fast rather than hanging forever. |
| Container does a lot of small writes | Local disk speed | NFS small-write overhead | If profiling shows it, add NFS server-side write-coalescing or bump to `actimeo`. Most containers are read-heavy. |

## Migration plan

1. Land the rspacefs-registry backend (separate spec). Default flag `eager_extract=true`.
2. Land the nextnfs rspacefs export type (separate spec).
3. Land this mkube change. Default `container_rootdir=tarball` initially; new code path is dormant.
4. Flip `container_rootdir=nfs` for one harmless namespace first (e.g., `gt/pvc-test`). Confirm container starts, reads rootfs, runs binaries, accepts writes.
5. Flip `infra/*` one container at a time.
6. Flip the DNS pods last (they're load-bearing).
7. Once stable, remove the tarball-extract code path from `mkube-update` and `CreatePod`. mkube-registry can stop maintaining the tarball blob form for rspacefs-aware repos (configurable per repo).

## Out of scope

- **OpenShift, Proxmox, StormBase backends.** They keep their existing flows. OpenShift goes through CRI-O + kernel overlayfs; Proxmox uses native LXC; StormBase has its own rootfs assembly. NFS-root-dir is a RouterOS-specific deal for now.
- **Live image upgrades while a container is running.** Out — image upgrade is still "stop, recreate with new export, start." The whole point of the upper-per-container is so the lower swap doesn't conflict with the live writable state.
- **Cross-host shared NFS exports** for clustering. mkube-cluster sync of exports is its own problem.

## See also

- `mkube/enhancements/rspacefs-registry-backend.md` — registry side (provides `/v1/exports`)
- `nextnfs/enhancements/rspacefs-export-type.md` — nextnfs side (serves the exports)
- `rspacefs/CLAUDE.md` — the rspacefs work plan items #3 and #4
