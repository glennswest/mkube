# `mkube-registry`: tiered master/worker rspacefs storage backend

**Status:** proposed — 2026-05-19 (rev 3, post-architecture-discussion)
**Owner:** glennswest
**Scope:** Replace `mkube-registry`'s single-instance tarball store with a content-addressable, two-tier (master / worker) registry. Masters are the source of record with peer replication; workers are per-node pull-through caches with aggressive GC. Trust is rooted in a cluster TLS CA — no copy/paste of keys, fully automated bootstrap on OCP, mkube, and standalone deployments.

## Goals

1. **Zero-tarball-extract container start** on both MikroTik (NFS root-dir) and OpenShift (kernel overlayfs).
2. **Non-destructive container commit** — running container never stops; rollback is automatic via OCI manifest history.
3. **File-content dedup** across all layers of all images.
4. **HA at the master tier** via peer replication (no NATS, no Raft library — KISS HTTP fan-out + anti-entropy).
5. **Aggressive worker GC**; conservative master GC (workers can lose data; masters are canonical).
6. **Standard OCI distribution v2 compatibility** on the wire.
7. **Mandatory fast push** for clients that support it (podman/buildah extension; separate spec).
8. **CA-rooted identity, fully automated bootstrap.**

Out of scope: NFS in the OpenShift data path (kernel overlayfs there). RouterOS host changes. Container-image format changes (we ride OCI).

## Architecture

```
                  Cluster CA (auto-generated; OCP service-ca / mkube cert
                              infra / standalone bootstrap token)
                                       │
            ┌──────────────────────────┼──────────────────────────┐
            │                          │                          │
        m1 cert                    m2 cert                    m3 cert
       (CN=m1)                    (CN=m2)                    (CN=m3)
            │                          │                          │
   ┌────────▼────────┐        ┌────────▼────────┐        ┌────────▼────────┐
   │ master m1       │◄──────►│ master m2       │◄──────►│ master m3       │
   │ - full content  │  HTTP  │ - full content  │  HTTP  │ - full content  │
   │   pool          │ replica│   pool          │ replica│   pool          │
   │ - all manifests │ fan-out│ - all manifests │        │ - all manifests │
   │ - LWW on tags   │   +    │ - LWW on tags   │        │ - LWW on tags   │
   │ - signs verity  │ anti-  │ - signs verity  │        │ - signs verity  │
   │   w/ master.key │ entropy│   w/ master.key │        │   w/ master.key │
   │ - conservative  │ hourly │ - conservative  │        │ - conservative  │
   │   GC            │        │   GC            │        │   GC            │
   └─────────────────┘        └─────────────────┘        └─────────────────┘
            ▲                          ▲                          ▲
            │       worker certs       │                          │
            │     (CN=w1, w2, ...)     │                          │
            ▼                          ▼                          ▼
   ┌─────────────────┐        ┌─────────────────┐        ┌─────────────────┐
   │ worker w1       │        │ worker w2       │        │ worker w3       │
   │ - LRU content   │        │ - LRU content   │        │ - LRU content   │
   │   cache         │        │   cache         │        │   cache         │
   │ - in-use mfs    │        │ - in-use mfs    │        │ - in-use mfs    │
   │ - aggressive GC │        │ - aggressive GC │        │ - aggressive GC │
   │ - long-polls    │        │ - long-polls    │        │ - long-polls    │
   │   master for    │        │   master for    │        │   master for    │
   │   tag changes   │        │   tag changes   │        │   tag changes   │
   │ - serves        │        │ - serves        │        │ - serves        │
   │   localhost:5000│        │   localhost:5000│        │   localhost:5000│
   └────────┬────────┘        └────────┬────────┘        └────────┬────────┘
            │                          │                          │
       CRI-O / mkube              CRI-O / mkube              CRI-O / mkube
       on this node               on this node               on this node
```

Both master and worker are the **same binary** with `role: master | worker | single` config. `single` collapses both onto one process for dev / homelab / current rose1.

## Storage layout

```
<store>/
  blobs/
    oci/sha256/<digest>           OCI tarball view (for stock pulls)
    content/sha256/<digest>       file-content blob — the dedup pool
  layers/sha256/<digest>/         materialized layer tree (hardlinks into content pool)
    entries.json                  ordered (path, content_sha, mode, uid, gid, mtime)
  manifests/<repo>/<tag>          OCI manifest JSON
    history.json                  tag → digest history (timestamp-ordered, LWW)
  verity/sha256/<digest>.merkle   per-layer Merkle tree + signature
  exports/<container_id>/         (master+worker on same node, or worker only)
    upper/                          per-container writable upper
    composed                        layer-list metadata used by nextnfs
  ca/
    ca.crt                          cluster CA (mode 0644)
    ca.key                          (master only, mode 0600, present only on
                                     the master that initially generated it
                                     or that received it via cluster sync)
    master.crt, master.key          this node's cert (master role)
    worker.crt, worker.key          this node's cert (worker role)
  state/
    replication.queue               pending fan-out ops (peer down → retry)
    index.db                        repo → tag → manifest_digest mapping
    keychain.json                   peer master certs (cached for worker trust)
  gc.lock                           single-writer lock for GC sweep
```

File-content dedup mechanism: every materialized file in `layers/sha256/<digest>/` is a hardlink (or `cp --reflink=always` on btrfs/xfs/apfs) to its `blobs/content/sha256/<content_sha>`. Same content × N images = one inode.

## API surface

### Standard OCI v2 (unchanged on the wire)

`/v2/<repo>/manifests/<tag>`, `/v2/<repo>/blobs/<digest>`, `/v2/<repo>/blobs/uploads/...` — preserved for stock `podman pull/push`, `buildah`, `skopeo`.

### Push (standard, default)

1. `POST /v2/<repo>/blobs/uploads/` and follow-on chunk uploads.
2. On finalize: write tarball to `blobs/oci/sha256/<digest>`.
3. Eager extraction worker streams the tarball through `tar -x`:
   - For each file F: `content_sha = sha256(F)`. If `blobs/content/sha256/<content_sha>` is absent, move F there; else delete the duplicate.
   - Record `(path, content_sha, mode, uid, gid, mtime)`.
4. `layer_digest = sha256(canonical(entries))`. Atomically build `layers/sha256/<layer_digest>/` with hardlinks/reflinks.
5. Compute verity Merkle via `rspacefs-verity::MerkleTree::build_from_vfs`; sign with the node's master cert private key; write `verity/sha256/<layer_digest>.merkle`.
6. Fan-out to peer masters (see Replication, below).

### Push (fast path) — **MANDATORY for v1**

`POST /v2/<repo>/extracted-blobs/uploads/<repo>` — multipart, one part per file (path + POSIX metadata in part headers, raw content as body). Server dedupes against `blobs/content/` as parts stream in. No tar parsing on either side. Replaces ~30% of push cost.

Cross-project spec for the podman/buildah client patch lives in `mkube/enhancements/podman-extracted-push.md` (TBD).

### `POST /v1/commit` — non-destructive snapshot

Body: `{ container_id, image: "repo:tag", base_image: "repo:tag", message }`.

Idempotency key: `(container_id, base_image_digest)` — same key returns the same result.

```
1. Walk the container's upper-dir READ-ONLY.
2. For each file F:
     content_sha = sha256(F)
     if !exists(blobs/content/sha256/<content_sha>):
         cp F → blobs/content/sha256/<content_sha>      (read F, never modify)
3. layer_digest = sha256(canonical(entries))
4. mkdir layers/sha256/<layer_digest>/ + hardlink/reflink each path
5. Build new manifest = base_image's layers + layer_digest
6. Write manifests/<repo>/<tag> = new_manifest; append to history.json
7. Sign verity Merkle; fan-out replication.
8. Background: produce blobs/oci/sha256/<layer_digest> for OCI-pull compat.
```

The running container is never touched. New layer is a content-snapshot of upper at commit time. Container's subsequent writes stay in its live upper for the next commit.

Rollback is free: previous manifest digest still resolves; `podman pull image@sha256:<old>` works.

### `POST /v1/exports` — live NFS export (MikroTik path only)

Body: `{ image, container_id, upper_size_limit? }`.

Returns `{ nfs_url, upper_path }`. Idempotent on `container_id`.

Implemented in two parts: the registry resolves manifests → layer dirs and writes `exports/<container_id>/composed`; nextnfs serves the actual NFSv4 (see `nextnfs/enhancements/rspacefs-export-type.md`).

Lives on the node that will run the container (typically a worker). On rose1 today, master+worker collapse onto one node.

### `DELETE /v1/exports/<container_id>`

Tear down the NFS export and immediately `rm -rf exports/<container_id>/upper/`. Per the agreed policy: no retention; if you want to keep the diff, `POST /v1/commit` first.

## Master ↔ master replication (peer-replicated, no consensus)

### Fan-out on write

When master m1 accepts a push or commit:
1. m1 writes locally.
2. m1 fans out `POST /peer/replicate` to every other peer master in background; returns success to client without waiting.
3. Failed peers go into `state/replication.queue` for retry (exponential backoff).
4. Peer receiving `/peer/replicate` writes locally (idempotent on content_sha + manifest digest).

### LWW for tag updates

Concurrent writes to the same tag on different masters: each manifest update carries a millisecond timestamp. Receiving master compares to local; keeps the newer one. Older digest's content blobs aren't deleted (manifest history retention applies); just the tag pointer changes.

Operationally rare. Clients that need consistency use digest references (`image@sha256:...`), not floating tags.

### Anti-entropy

Hourly, each master picks a random peer and exchanges:
- `(repo, tag, digest, timestamp)` tuples for every known tag.
- `(content_sha)` set summaries (e.g., Merkle-of-Merkles, or paginated lists with cursors).

Older entries get adopted from peer; missing entries pulled. Converges anything missed by the fan-out queue.

### No quorum, no leader election, no NATS

Each master is a fully autonomous peer. Network partitions: both sides keep accepting writes; reconcile on heal (LWW). All-masters-down: nothing to serve, but worker caches keep running containers alive.

## Worker behavior

### Pull

```
CRI-O / mkube calls worker localhost:5000:
  1. Check local content pool — every layer's blobs accounted for + signatures valid?
  2. If yes: serve. Done.
  3. If no: pick a master (round-robin); pull missing blobs.
  4. Verify signatures against cached cluster CA.
  5. Materialize layer tree (hardlink/reflink into local pool).
  6. Serve.

If no master reachable:
  - Cached images still work.
  - Cache miss returns 503; CRI-O retries; client backs off.
```

### Tag invalidation

Worker holds an HTTP long-poll connection to each known master:
```
GET /v1/watch?since=<unix_ms>
  → returns event stream of (repo, tag, digest, timestamp) on change.
```
Worker disconnects/reconnects on errors. Falls back to 30s TTL on cached manifests in case all long-polls drop.

### Commit forwarding

Worker accepts `POST /v1/commit`, stages content blobs locally (so the commit's data survives master outage), then forwards manifest creation to a master. Master fans out via the normal replication.

### Worker GC (aggressive)

```yaml
gc:
  blob_eviction_policy: "lru"
  max_disk_usage_pct: 80
  reclaim_to_pct: 70
  retain_running_containers: true      # never evict a blob a current container needs
  schedule: "hourly"
```

The worker can lose data freely — the master always has it. Anti-fragile by design.

## Master GC (conservative)

```yaml
gc:
  manifest_history_retention: 365d
  blob_eviction_policy: "no_orphans"   # only blobs no manifest references
  min_replicas_before_delete: 2        # never delete the last copy
  peer_check_before_delete: true        # ask peers before final unlink
  schedule: "weekly"
```

GC walks all manifests in history window, computes the live set, asks peers "are you about to GC this too?" before unlinking, then deletes.

## Identity & trust — cluster CA, fully automated

**Trust root = a single cluster CA cert.** All masters and workers have certs issued by this CA. Verifying anyone's identity = standard X.509 chain validation against the CA.

### Bootstrap mode 1: OCP / K8s

Operator deploys via `oc apply -f rspacefs.yaml`:
```yaml
apiVersion: v1
kind: Service
metadata:
  name: rspacefs-master
  annotations:
    service.beta.openshift.io/serving-cert-secret-name: rspacefs-master-tls
spec:
  selector: { app: rspacefs-master }
  ports: [{ port: 5000 }]
---
# Master Deployment mounts /etc/registry/tls from the secret.
# Worker DaemonSet does the same.
# OCP's service-ca operator issues + rotates certs automatically.
# CA bundle mounted via the standard openshift-service-ca configmap.
```

**Zero copy/paste.** Operator runs `oc apply`; the platform handles everything.

### Bootstrap mode 2: mkube cluster

mkube already has cluster-bootstrap machinery (`pkg/cluster/`). The CA cert + key are part of cluster state (NATS-backed). First mkube-registry node generates the CA on startup if absent; new masters joining the mkube cluster receive it via the existing cluster-sync. Workers minted from CA on demand.

**Operator runs `make deploy` exactly like today.** No keys handled.

### Bootstrap mode 3: standalone

Auto-generated CA + short-lived join tokens:
```
First master start (no CA exists):
  1. Generate cluster CA + own master cert.
  2. Log: "Cluster bootstrapped. CA fingerprint sha256:abcd..."

Second master joining:
  $ rspacefs-master join --token=<HMAC token, 1h TTL> --upstream=https://m1:5000
  → m1 verifies token, issues master cert + CA to m2 over the connection
  → m2 persists, becomes peer

Worker joining:
  $ rspacefs-worker join --token=<...> --upstream=https://m1:5000
  → same pattern, gets worker cert + CA
```

Tokens live in deploy automation (Ansible vault, cloud-init secrets, etc.). Operator runs the installer; tokens are consumed once. **No key value displayed.**

### Cert lifetime & rotation

- **Default cert lifetime: 90 days.** Matches OCP service-ca defaults.
- **Renewal**: each node auto-renews at 75% of lifetime (≈ day 67). Renewal flow:
  - Mode 1 (OCP): service-ca operator does it; nothing for us to do.
  - Mode 2 (mkube): mkube cluster CA reissues; node picks up new cert.
  - Mode 3 (standalone): node calls back to a peer master with current cert; CA reissues.
- **Old cert grace window: 30 days after expiry.** Workers retain expired certs in their local cert store so old verity-signed content stays trusted; only NEW signatures from an expired cert are rejected.

### Revocation (emergency)

`mkube-registry cert revoke <master_id>` — broadcast revocation event to all peers + workers; revoked cert added to a local CRL; verifiers reject. Required for v1 (compromised key shouldn't wait 90 days to retire).

### Cert library

- **rustls** for TLS server / client.
- **rcgen** for CA issuance (small, pure-Rust X.509 generator).
- **Ed25519** signing keys.
- No OpenSSL dependency.

## Verity signing flow

```
Master m1 builds a verity tree for layer L:
  hash_to_sign = sha256(verity_root_hash || layer_digest || timestamp || master_id="m1")
  signature   = Ed25519_sign(master_cert.priv, hash_to_sign)
  write verity/sha256/<L>.merkle = {
    root_hash, layer_digest, timestamp,
    master_id: "m1",
    cert_fingerprint: sha256(master_cert),
    signature: <hex>,
  }
  + retain master_cert in <store>/state/keychain.json for workers to fetch.

Worker pulls + verifies:
  1. Look up cert by fingerprint in local keychain (long-poll updates this).
  2. Verify cert chains to cluster CA.
  3. Check cert not revoked (CRL).
  4. Verify Ed25519 signature.
  5. Begin block-level verity verification on file reads (rspacefs-verity).
```

Content signed by a now-expired cert: still verifies as long as the cert is in the worker's history (we never purge keychain entries — they're tiny).

## Failure modes

| Scenario | Behavior |
|---|---|
| Master crash | Other masters keep serving (reads + writes). Dead master catches up via anti-entropy on restart. |
| Network partition | Both sides serve; reconcile on heal via LWW. |
| Worker disk wipe | Worker re-pulls from master on next container start. Self-healing. |
| Concurrent commit to same tag | LWW; one wins silently. Use digest refs for safety-critical pushes. |
| Crash mid-extract | Temp dir orphaned; GC sweep reaps. Push is idempotent. |
| Crash mid-commit | Partial layer dir; GC reaps. Manifest never written. |
| CA cert compromise | Operator: `mkube-registry cert revoke <id>`; broadcast; rotate. |
| Last master alive crashes during a write | That write is lost (no peer received the fan-out). Client retries — accepted by next available master. For higher durability: future option to wait for at least one peer ack. |

## Configuration

```yaml
# master config
role: master
storage: /raid1/registry
cluster:
  name: rose1-cluster
  peers:
    - https://m1.rose1.gw.lo:5000   # populated automatically once joined
    - https://m2.rose1.gw.lo:5000
ca:
  path: /raid1/registry/ca           # auto-generated if missing in standalone mode
  cert_lifetime: 90d
  renewal_threshold: 75%
  retention_grace: 30d
replication:
  fanout_timeout: 5s
  retry_backoff: ["10s", "1m", "10m", "1h"]
anti_entropy:
  schedule: "0 * * * *"             # hourly
gc:
  manifest_history_retention: 365d
  schedule: "0 3 * * 0"             # weekly Sunday 03:00
  min_replicas_before_delete: 2
verity:
  enabled: true
exports:
  enabled: false                    # only the node co-located with nextnfs flips this
  nextnfs_url: "http://localhost:8080"
```

```yaml
# worker config
role: worker
storage: /var/lib/rspacefs
masters: ["https://rspacefs-master.cluster.svc:5000"]   # k8s service form; static list in standalone
ca:
  bundle: /etc/registry/tls/ca.crt   # mounted by platform / installer
  cert_lifetime: 90d
gc:
  blob_eviction_policy: lru
  max_disk_usage_pct: 80
  reclaim_to_pct: 70
  retain_running_containers: true
  schedule: "0 * * * *"             # hourly
watch:
  long_poll_timeout: 60s
  ttl_fallback: 30s
```

```yaml
# single-node (rose1 today)
role: single                         # master+worker collapsed
storage: /raid1/registry
cluster:
  name: rose1
  peers: []                          # solo
ca:
  path: /raid1/registry/ca
exports:
  enabled: true                      # nextnfs co-located
  nextnfs_url: "http://localhost:8080"
```

## Implementation order

1. **Storage primitives** — `pkg/registry/storage/content_pool.go`, `layer_tree.go`, `extract.go`. Hardlink/reflink materialization. Independent of master/worker role.
2. **`POST /v1/commit`** — non-destructive snapshot. Single-node mode.
3. **Multipart fast push** — `pkg/registry/api/push_extracted.go`.
4. **CA infrastructure** — `pkg/registry/ca/`: auto-generation, cert issuance, renewal, revocation. Tested in standalone mode.
5. **Peer replication** — fan-out queue + `/peer/replicate` endpoint + anti-entropy.
6. **Verity signing** — bind master cert to verity Merkle output.
7. **Worker mode** — long-poll watch, LRU cache, commit forwarding.
8. **`POST /v1/exports`** — together with `nextnfs/enhancements/rspacefs-export-type.md`.
9. **GC** — master (conservative) and worker (aggressive) sweepers.
10. **OCP integration** — Helm chart / Kustomize templates using service-ca annotations.

Each step is independently shippable. `rose1` runs in `role: single` from step 1; gains features as they land.

## Migration plan

1. Land steps 1–4 behind `rspacefs.enabled: false`. mkube-registry behaves as today.
2. Flip `rspacefs.enabled: true` on rose1 (single mode). Backfill: rebuild content pool + layer trees from existing tarball blobs.
3. Ship `POST /v1/commit`; add `mk image commit <container>` CLI.
4. Ship `POST /v1/exports` (concurrent with nextnfs's rspacefs export type).
5. Switch mkube container creation to consume `/v1/exports` (separate spec: `mkube/enhancements/nfs-rootdir-containers.md`).
6. Ship multi-master / worker tier when a second mkube node joins (stormbase deployment).
7. Ship the OCP path independently once mkube-registry is proven on rose1.

## See also

- `mkube/enhancements/nfs-rootdir-containers.md` — mkube container creation switch
- `nextnfs/enhancements/rspacefs-export-type.md` — nextnfs side of `/v1/exports`
- `rspacefs/CLAUDE.md` — work plan items #3 (nextnfs extension) and #4 (this spec)

## Future / explicitly deferred

- **`rspacefs-snapshotter`** — CRI-O snapshotter for the OpenShift *node-local* path. Lets OpenShift get the per-node content-pool / hardlink benefits without making rspacefs-registry the cluster registry. Separate project, separate spec.
- **HSM / TPM-backed master keys** — pluggable signer interface; out of v1.
- **GC across cluster with global reference counting** — current design is conservative (peer-check before delete); reference counting would be tighter but requires more bookkeeping.
- **Manifest signature** (beyond verity tree) — sign the OCI manifest itself for end-to-end attestation. Cosign-compatible. Future.
