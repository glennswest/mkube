# mkube boot: pivot to NFS-served rootfs once registry is up

**Status:** proposed — 2026-05-19
**Owner:** glennswest
**Scope:** Refactor mkube-update's bootstrap so that **after the rspacefs-registry is running, mkube's own container starts from the registry's NFS export** — not from a re-extracted tarball. Cuts the tarball-extract step out of the steady-state restart path for mkube itself. Solves the chicken-and-egg of "the registry that holds mkube is a container managed by mkube."

## Motivation

Today's mkube boot (rose1):

```
mkube-update-updater (bootstrap container, baked image)
   │
   ▼  /container/add file=mkube-tarball.tar
RouterOS extracts tarball to /raid1/images/kube.gt.lo/
   │
   ▼  /container/start
mkube starts → reconciles cluster → brings up everything else
   (including mkube-registry as a container, and nextnfs)
```

Every mkube restart re-extracts the mkube image into `/raid1/images/kube.gt.lo/`. Once `mkube-registry` is running and `mkube-update` migrated to the [nfs-rootdir flow](nfs-rootdir-containers.md) for normal containers, the *one* container still using tarball extraction is **mkube itself**. The image of mkube is sitting right there in the registry's content pool — it's silly to re-extract.

After this change:

```
First boot of a node (registry not yet running):
  mkube-update tarball-bootstraps mkube exactly as today.
  mkube starts, brings up rspacefs-registry.
  rspacefs-registry sees its content pool is empty → fetches images
  from GHCR (or upstream cluster master) and populates content pool.
  mkube image is now in the local registry.

Subsequent boots / mkube image updates:
  mkube-update notices registry has the mkube image.
  POST /v1/exports {image: "ghcr.io/glennswest/mkube:edge",
                    container_id: "kube.gt.lo"}
  Get nfs_url back.
  /container/stop kube.gt.lo (clean shutdown).
  /container/add or /container/set root-dir=<nfs_url>
  /container/start kube.gt.lo
  → mkube live-overlays from registry; per-container upper for runtime state.
```

mkube restarts go from "extract a few hundred MB of tarball" to "tear down + remount NFS." Order-of-magnitude faster, and identical to how all the other containers will boot.

## What changes

### `cmd/mkube-update/main.go`

Add a "mode adoption" decision at boot:

```go
// Pseudocode
func bootstrapMkube() error {
    if registryReachable("localhost:5000") && registryHasImage("mkube:edge") {
        // Steady state: use NFS root-dir.
        export := registryCreateExport("mkube:edge", "kube.gt.lo")
        return startContainerWithRootDir("kube.gt.lo", export.NfsURL)
    }
    // Cold start: tarball path, current behavior.
    return startContainerFromTarball("kube.gt.lo", "/raid1/tarballs/mkube.tar")
}
```

After bootstrap, watch for the registry to come up and for the mkube image to appear in it (via long-poll on `/v1/watch`). Once it does, the *next* `replaceContainer(...)` call (triggered by a normal image update or operator request) uses the NFS path.

### `mkube-update-updater` baked image

Keeps the tarball-extract code (for cold starts) **and** gains the NFS-rootdir code. Same binary, both paths.

### Bootstrap content pre-staging (optional but huge win)

In `stormbase`-built install ISOs / RouterOS NPK builds, **pre-populate `/raid1/registry/blobs/content/` and `/raid1/registry/layers/`** with the bootstrap images (mkube, registry, nextnfs, microdns, registry, mkube-update). When the node boots for the first time:

1. `mkube-update-updater` starts.
2. It finds `/raid1/registry/` already populated — pretends the registry already ran a push by reconstructing manifests from pre-staged data.
3. Brings up `kube.gt.lo` directly via NFS root-dir → no tarball extract on first boot either.
4. Registry container starts, immediately serves the pre-staged content; no pull from GHCR needed for offline installs.

This is the registry-side answer to "bundle containers in the install ISO" — same mechanism, no special bootstrap format.

## Failure modes

| Scenario | Behavior |
|---|---|
| Registry unreachable at boot (cold start, no pre-staged data) | mkube-update falls back to tarball path; same as today. |
| Registry up but `mkube:edge` content blob missing | Treat as cold start: fall back to tarball. Background: pull from upstream master / GHCR; next restart goes NFS. |
| NFS mount fails during steady-state restart | mkube-update retries; on N failures, falls back to tarball if a recent tarball cache exists; if not, alerts and waits. |
| Registry storage corrupt | mkube-update detects via verity-check; treats as cold start. Background: re-populate from upstream / GHCR. |
| nextnfs unreachable (NFS server down) | Same as "NFS mount fails." mkube-update has the tarball fallback. |

The tarball cache stays as a safety net. Eventually (separate spec / decision) it can be retired once the NFS path has accumulated enough operational hours. For now, keep it.

## Bootstrap chicken-and-egg analysis

**Who starts whom?**
- `mkube-update-updater` is the first user-controlled process on a fresh node (RouterOS starts it on boot).
- It starts `kube.gt.lo` (mkube).
- mkube starts `registry.gt.lo` (rspacefs-registry).
- mkube starts `nextnfs` (or it's a separate container under mkube control).
- mkube starts everything else.

**Where does the mkube image come from on first boot?**
- Pre-staged in `/raid1/registry/...` by the install ISO. → `mkube-update-updater` skips tarball entirely.
- Or: pre-staged tarball at `/raid1/tarballs/mkube.tar`. → `mkube-update-updater` uses today's flow.
- Or: pulled from GHCR by `mkube-update-updater` if network is available. → today's flow.

**What happens when mkube updates its own image?**
- `mkube-update-updater` watches the registry / GHCR.
- New image digest detected.
- `mkube-update-updater` requests new NFS export for `kube.gt.lo`.
- Stop, swap root-dir, start. ~seconds, no extraction.

The flow is symmetric with how other containers will work. mkube isn't special anymore.

## Implementation order

Depends on `rspacefs-registry-backend.md` step 4 (registry exports) and `nfs-rootdir-containers.md` (root-dir=nfs:// support in mkube). Once both are landed, this is a small change to `mkube-update`'s bootstrap logic.

1. `mkube-update`: detect-and-pivot logic (try NFS, fall back to tarball).
2. `mkube-update`: long-poll watch for "mkube image now available" → schedule pivot for next restart.
3. `stormbase` ISO build: optional `--prestage-registry` step that copies a content pool tarball into the ISO's `/raid1/registry/`. (Separate spec in stormbase.)
4. Operational testing: switch rose1 to NFS-rootdir-only-for-mkube. Watch a few weeks; if stable, deprecate tarball cache.

## See also

- `mkube/enhancements/rspacefs-registry-backend.md` — registry that provides the NFS exports
- `mkube/enhancements/nfs-rootdir-containers.md` — root-dir=nfs:// for normal containers (this spec applies the same mechanism to mkube itself)
- `nextnfs/enhancements/rspacefs-export-type.md` — NFS export type
- `stormbase` — install ISO build needs an option to pre-stage registry content (separate spec, in stormbase)
