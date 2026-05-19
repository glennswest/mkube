# Extend `mkube-registry` with the rspacefs storage backend

**Status:** proposed — 2026-05-19 (revised)
**Owner:** glennswest
**Scope:** Replace `mkube-registry`'s tarball-only storage with a content-addressable file-blob pool plus hardlink-materialized layer trees. Adds APIs for non-destructive container commit, live NFS export (RouterOS-only consumer), and a mandatory faster image-push path. OpenShift / podman see standard OCI v2 distribution on the wire — they just get a much faster registry.

## Goals

1. **Zero-tarball-extract container start** for both MikroTik (via NFS export) and OpenShift (via fast pull).
2. **Non-destructive container commit** — running container never stops, rollback is automatic via OCI manifest history.
3. **File-content dedup** across all layers of all images.
4. **Standard OCI distribution v2 compatibility** for all stock clients.
5. **Mandatory fast push paths** for clients that can use them (podman + buildah extensions).

Out of scope: NFS in the OpenShift data path (it stays kernel overlayfs there). RouterOS host changes (already supports `root-dir=nfs://…`).

## Storage layout

```
<store>/
  blobs/
    oci/sha256/<digest>           OCI tarball view (compat for stock pulls)
    content/sha256/<digest>       File-content blob (the dedup pool — what NFS / hardlinks point at)
  layers/sha256/<digest>/         Materialized layer tree (hardlinks into content pool)
    entries.json                  Sidecar: ordered (path, content_sha, mode, uid, gid, mtime)
  manifests/<repo>/<tag>          OCI manifest JSON
  verity/sha256/<digest>.merkle   rspacefs-verity Merkle tree (per layer, optional)
  exports/<container_id>/
    upper/                        Per-container writable upper (live NFS path only)
    composed                      Layer-list metadata used by nextnfs (lowers + upper paths)
  index.db                        repo → tag → manifest_digest mapping
  gc.lock                         File lock taken by the nightly GC sweep
```

**Dedup mechanism:** every file in every materialized layer is a hardlink (`ln`) — or a reflink (`cp --reflink=always`) on btrfs/xfs/apfs — to its content blob in `blobs/content/`. Same content, written by 20 layers, exists once on disk.

**Inode pressure:** N layers × M files per layer hardlinks. Easily within ext4's limits even at registry-scale (50k files × 100 layers = 5M directory entries, ~negligible storage for the entries, content blob count is the actual disk cost).

## API surface

### Standard OCI v2 distribution (unchanged on the wire)

`/v2/<repo>/manifests/<tag>`, `/v2/<repo>/blobs/<digest>`, `/v2/<repo>/blobs/uploads/...` — all preserved. Stock `podman pull`, `buildah push`, `skopeo` etc. work unchanged. Tarballs served from `blobs/oci/sha256/<digest>`.

### Push (standard, default path)

1. Accept `POST /v2/<repo>/blobs/uploads/` etc. as today.
2. On finalize, write to `blobs/oci/sha256/<digest>` (tarball form).
3. **Eager extraction:** spawn a worker that streams the blob through `tar -x` into a temp dir, then:
   - For each extracted file F:
     - `content_sha = sha256(F)`
     - If `blobs/content/sha256/<content_sha>` doesn't exist, move F there.
     - Otherwise (collision = same content already in pool), delete the extracted copy.
   - Record `(path, content_sha, mode, uid, gid, mtime)` per file.
4. `layer_digest = sha256(canonical(entries))`. Atomically rename the temp dir into `layers/sha256/<layer_digest>/`, with hardlinks (or reflinks) from each `<path>` to `blobs/content/sha256/<content_sha>`. Write `entries.json`.
5. Optional: compute verity Merkle via `rspacefs-verity::MerkleTree::build_from_vfs`, write to `verity/sha256/<layer_digest>.merkle`.

Push API itself is idempotent on `(repo, blob digest)`; re-pushing the same layer is a no-op.

### Push (fast path) — **MANDATORY for v1**

`POST /v2/<repo>/extracted-blobs/uploads/<repo>` (parallel to `/blobs/uploads/...`).

- **Multipart**, one part per file.
- Each part's `Content-Disposition` includes:
  - `name="path"` — relative path within the layer
  - `name="mode"`, `name="uid"`, `name="gid"`, `name="mtime"` — POSIX metadata
- Body is the raw file content (no `tar`, no compression).
- Server hashes each part's body as it streams (no temp file → tarball roundtrip), dedupes against `blobs/content/`, builds the layer dir incrementally.
- On final `commit-extracted-blob` request, server returns the computed `layer_digest`.

Eliminates: client-side tar packaging, server-side tar parsing. Wins the ~30% of push time that today goes to tar.

Client requirements: a small podman/buildah patch that produces the multipart stream instead of a tar. Cross-project spec to follow (separate task).

### `POST /v1/commit` — snapshot a running container as a new image

Body: `{ container_id, image: "repo:tag", base_image: "repo:tag", message: "..." }`.

```
1. mkube tells the registry where container_id's upper-dir lives.
2. Walk the upper READ-ONLY:
   For each file F:
     content_sha = sha256(F)
     if !exists blobs/content/sha256/<content_sha>:
         cp F → blobs/content/sha256/<content_sha>   (read F, never modify)
3. entries = ordered (path, content_sha, mode, uid, gid, mtime) list
4. layer_digest = sha256(canonical(entries))
5. mkdir -p layers/sha256/<layer_digest>/
   For each (path, content_sha) in entries:
       ln blobs/content/sha256/<content_sha>  layers/sha256/<layer_digest>/<path>
       (or `cp --reflink=always` if FS supports it)
   Write entries.json
6. new_manifest = base_image's layers + layer_digest
   Write manifests/<repo>/<tag> = new_manifest
   Update index.db
7. Background: produce blobs/oci/sha256/<layer_digest> tarball view for OCI-compat pulls.
   Background: produce verity/sha256/<layer_digest>.merkle.
```

Returns `{ digest, manifest_path, tag }`.

**Why this is non-destructive:** step 2 only *reads* the upper. The running container keeps writing to its upper unchanged. The new layer is a content-snapshot of the upper at commit time. Container writes after commit don't appear in the new layer (correct snapshot semantics); they're still in the container's live upper.

**Why rollback is free:** the previous tag's manifest still references the old layer list. `podman pull image:old-digest` works after commit. Even after promoting `new` → `latest`, the previous digest is recoverable from `index.db` history.

### `POST /v1/exports` — live NFS export (MikroTik path only)

Body: `{ image: "repo:tag", container_id, upper_size_limit?: bytes }`.

```
1. Resolve repo:tag → manifest → ordered layer_digest list
2. mkdir -p exports/<container_id>/upper/
3. Write exports/<container_id>/composed:
     {
       "lowers":[
         "layers/sha256/<layer0_digest>",
         "layers/sha256/<layer1_digest>",
         ...
       ],
       "upper":"exports/<container_id>/upper",
       "upper_size_limit": N
     }
4. Call nextnfs's add_rspacefs_export(name=<container_id>, upper=..., lowers=[...])
5. Return { nfs_url: "nfs://<registry-host>/<container_id>", upper_path }
```

Idempotent on `container_id`: re-calling returns the existing export (and re-attaches it to nextnfs if it was forgotten).

OpenShift never calls this. The endpoint is for mkube on RouterOS.

### `DELETE /v1/exports/<container_id>`

Tear down the NFS export and **immediately** delete `exports/<container_id>/upper/`. Per the policy decision: upper has no retention period. Out-of-band snapshots before delete are the caller's responsibility (or use `/v1/commit` first).

## Garbage collection

Nightly sweep, single-writer (takes `gc.lock`):

```
1. Walk manifests/ → collect every (repo, tag, manifest_digest) and the layer_digests referenced.
2. Walk index.db history → collect prior manifest_digests still within retention window.
3. live_layers = ∪ all referenced layer_digests
4. Walk layers/sha256/ — anything not in live_layers: rm -rf  (also removes the hardlinks).
5. live_content = ∪ all content_shas from entries.json of live_layers + the content_sha of every file currently in any exports/<id>/upper/.
6. Walk blobs/content/sha256/ — anything not in live_content: rm.
7. Walk blobs/oci/sha256/ — anything not referenced by a live manifest: rm.
8. Walk verity/sha256/ — anything not referenced by a live manifest: rm.
```

- Steps 4 and 6 are the actual reclamation.
- A `partial-push.lock` is taken during in-flight pushes so GC doesn't clobber half-uploaded content. GC waits for the push lock.
- A "blob added but layer commit failed" orphan gets reaped on the next sweep. No reference-counting on hot paths.
- Schedule: 02:00 local by default, configurable. Holds `gc.lock` for the duration; new pushes block on the same lock.

## Configuration (`deploy/registry-config.yaml`)

```yaml
rspacefs:
  enabled: true
  storage: /raid1/registry
  push:
    extracted: true              # MANDATORY: accept the fast-path push protocol
  layer_materialize:
    use_reflinks_when_available: true   # try cp --reflink=always; fall back to ln
  exports:
    enabled: true               # nextnfs co-located; expose /v1/exports
    nextnfs_url: "http://localhost:8080"
    upper_retention: 0          # seconds; 0 = delete immediately
  gc:
    enabled: true
    cron: "0 2 * * *"           # nightly at 02:00
    manifest_history_retention_days: 30
  verity:
    build_on_push: true         # cheap, do it always
```

`exports.enabled: false` for non-RouterOS deployments (e.g., OpenShift-only nodes) means the `/v1/exports` route returns 404 and nextnfs doesn't need to be running.

## Implementation order (in mkube)

1. `pkg/registry/storage/content_pool.go` — write/read/dedup `blobs/content/`.
2. `pkg/registry/storage/layer_tree.go` — materialize layer dirs via hardlink/reflink.
3. `pkg/registry/storage/extract.go` — eager tarball extraction worker → content pool.
4. `pkg/registry/api/push_extracted.go` — multipart fast push handler.
5. `pkg/registry/api/commit.go` — `POST /v1/commit`.
6. `pkg/registry/api/exports.go` — `POST /v1/exports`, `DELETE /v1/exports/<id>` (calls into nextnfs).
7. `pkg/registry/gc/sweep.go` — nightly GC.
8. `pkg/registry/verity.go` — rspacefs-verity Merkle build on push.

## Failure modes

| Scenario | Behavior |
|---|---|
| Crash mid-extract | Temp dir orphaned in `blobs/content-staging/<...>`. GC sweep reaps. Push is idempotent — client retries cleanly. |
| Crash mid-commit | New `layers/sha256/<>/` partially created. GC reaps. The new manifest was never written, so client retries. |
| Two concurrent pushes of same digest | Both compute `content_sha`; second one finds blob already present, skips the cp. |
| GC running during a push | Push holds `partial-push.lock`; GC waits. (Pushes are typically seconds; GC waits seconds.) |
| Container writes during a commit | Container's upper is read-once during commit. Subsequent writes don't appear in the snapshot; they remain in the live upper for future commits. |
| `entries.json` corruption | Detectable via `layer_digest` recomputation; force re-extract from `blobs/oci/<>` if available, else mark image broken. |

## Migration plan

1. Land the storage backend + extract worker behind `rspacefs.enabled: false`. mkube-registry behaves as today.
2. Flip `rspacefs.enabled: true` on rose1. Backfill: rebuild materialized layer trees + content pool from existing tarball blobs (background, idempotent).
3. Ship `/v1/commit` and the corresponding `mk image commit <container>` CLI.
4. Ship `/v1/exports`; cross-coordinate with `nextnfs/enhancements/rspacefs-export-type.md`.
5. Ship multipart fast-push handler; cross-coordinate with the (separate) podman/buildah patch.
6. (Separate spec: `mkube/enhancements/nfs-rootdir-containers.md`) Switch mkube container creation to consume `/v1/exports`.

## See also

- `mkube/enhancements/nfs-rootdir-containers.md` — mkube-side consumer of `/v1/exports`
- `nextnfs/enhancements/rspacefs-export-type.md` — nextnfs side that serves the export
- `rspacefs/CLAUDE.md` — work plan items #3 and #4
