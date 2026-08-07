# StormBlock Registry (sbregistry + stormblockmk)

**Status:** Proposed (2026-08-07)
**Supersedes:** TODO #3 (registry push notifications to mkube-update), TODO #18
(overlayfs image catalog / zero-copy recreate). Replaces `mkube-update` and the
standalone `mkube-registry` once complete.
**Related:** stormblock repo (`enhancements/stormblockmk.md`), TODO #6
(microdns resilience — control-plane exemption below).

## Motivation

The image pipeline pays for the same bytes over and over. A mkube self-update
today: `podman push` (~30s) → mkube-update *polls* the registry digest every
60s (up to 60s dead) → re-downloads the whole image from a registry on the
same disk (~30–60s) → stop → RouterOS untars the full rootfs onto ext4 on an
ARM64 CPU (~30–60s) → start. **3–6 minutes push-to-serving, ~2 minutes of it
avoidable waiting and copying.** Every pod recreate pays the same untar. ext4
has no reflink, so the clone-catalog variant of TODO #18 was dead on arrival,
and the overlayfs variant is gated on RouterOS support that may never come.

StormBlock already has the needed primitives: thin volumes, **CoW snapshots in
its own Global Extent Map** (above the backing store — ext4's lack of reflink
stops mattering), iSCSI + NVMe-oF/TCP targets, shared-ring IPC.

Design goal: **extraction happens once per image digest; everything else is a
CoW clone + mount.** Push-to-serving ≈ push time + seconds; mkube update
downtime ≈ seconds; pod recreate ≈ clone + mount, no untar.

## Topology — rose-local loopback target

One container on rose1, root-dir on plain `/raid1` (never on its own LUNs),
`start-on-boot`, earliest boot priority. RouterOS's own iSCSI initiator mounts
LUNs served by this container over the gt bridge (loopback TCP to
192.168.200.x:3260). No dependency on the fabric or server9 — the control
path is entirely on-box. (server9/stormblock over 25G is a later, separate
capacity pool for workload data, not for the control plane.)

```
RouterOS boot
  → sbregistry container starts (plain fs, start-on-boot)
      stormd (PID 1)
        ├── stormblockmk   — block engine
        └── sbregistry     — OCI registry + orchestrator
  → iSCSI target up → RouterOS initiator (re)attaches LUNs (/disk)
  → sbregistry starts/verifies mkube (root-dir = CoW clone LUN mount)
  → mkube brings up everything else
```

## Two processes, one container

**stormblockmk** — StormBlock built/profiled for RouterOS containment:
- Backing store: **file-backed slabs** on a mounted `/raid1` path (PVC-style
  mount). No raw devices — RouterOS containers have no /dev passthrough. The
  GEM does CoW above the slab file, so the underlying fs is irrelevant.
- Targets: iSCSI (:3260) first; NVMe-oF/TCP (:4420) when ROS initiator support
  is confirmed ≥7.9 and tested.
- Control surface: shared-ring IPC (unix socket + memfd) consumed by
  sbregistry only. No public API of its own.
- Stays generic StormBlock — "mk" is a build/packaging profile (musl static,
  file-backed default, scratch-friendly), not a fork.

**sbregistry** — the brain, speaking three protocols:
1. **OCI distribution API** (push/pull, `:5000`-compatible) — drop-in for the
   current registry so `podman push registry.gt.lo:5000/x:edge` keeps working
   and pull-based consumers (image-policy auto, PXE hosts via HTTP) still work
   during migration.
2. **Volume API** (for mkube): `POST /volumes/clone {digest}` → returns
   iSCSI target/LUN identifiers ready to attach; `DELETE /volumes/{id}`;
   refcounts; `GET /images/{ref}/digest`.
3. **Notifications + supervision**: webhook to mkube on push (replaces the
   digest poll everywhere); and sbregistry itself **starts/restarts mkube** on
   mkube-image updates (absorbs mkube-update's role — pre-stage golden while
   old mkube runs, then stop → attach clone → start; seconds of downtime).
   Supervision scope is mkube ONLY — everything else stays mkube-orchestrated.

**On push of any image**: store blobs (OCI store on plain fs) → allocate thin
volume via IPC → mkfs (ext4, loop-mounted *inside the container*, needs no
host /dev) → extract flattened rootfs → seal as immutable **golden volume
keyed by manifest digest** → fire webhook. Goldens are never handed out;
consumers only ever get CoW clones. Rollback = clone the previous digest's
golden (kept until GC policy expires it).

## mkube integration

- New root-dir provider in `pkg/storage`/`pkg/runtime/routeros.go`:
  `CreatePod` asks sbregistry for a clone, attaches it via `/disk add
  type=iscsi ...` (new `pkg/routeros` disk API support), waits for the mount,
  points `root-dir` at it, creates the container with **no `file=`** (probe
  P2). `DeletePod`/teardown detaches the disk and releases the clone
  (refcount → GC).
- Per-pod annotation opt-in during rollout: `vkube.io/rootfs: stormblock`
  (default remains tarball) until confidence, then flip the default.
- **Control-plane exemption:** sbregistry itself, and (initially) DNS + NATS,
  stay on plain-fs tarball roots so a registry outage can never take down
  name resolution or the KV store (aligns with TODO #6). Revisit only after a
  long soak.
- PXE media (later phase): sbregistry serves install-ISO LUNs, replacing the
  RouterOS built-in target (`iqn.2000-02.com.mikrotik:fileN`) in BMH
  `root_path` — each PXE host can get its own writable CoW clone of install
  media, which the mikrotik target cannot do.
- **PVCs on stormblock volumes**: a new storage class (`storageClassName:
  stormblock`) provisions a thin volume instead of a `/raid1` directory —
  attach via the same `/disk` initiator flow, mount at the PVC's dst. Wins
  over today's directory-backed PVCs: real size enforcement (thin
  provisioning with a hard cap), **CoW snapshots** (instant backup points,
  and PVC migration becomes snapshot + clone instead of phase-aware file
  copy), per-volume fs isolation (one pod can't wedge another's data with fs
  corruption), and the same volume can later be served to bare-metal hosts
  (server9-hosted pool, phase 6) with an identical API. The existing iSCSI
  PVC prototype (`pkg/provider/pvc_iscsi.go`) is the integration seam — it
  already does initiator attach/mount; it just gains sbregistry as the
  provisioner instead of the RouterOS target. Directory-backed PVCs remain
  the default until the phase-4 soak passes; migration path = mkube's
  existing PVC migrate flow (copy dir → volume) or snapshot-clone for
  volume→volume.

## Phase 0 — capability probe (GATES EVERYTHING, ~an afternoon)

P1. RouterOS `/disk add type=iscsi` to a **container IP on its own bridge**
    (loopback initiator). Must confirm: works at all; **retries/re-attaches
    when the target comes up after boot** (registry starts seconds after ROS);
    reconnect + remount behavior after a target bounce *with a pod running on
    the mount* (the evil twin — a registry crash must not permanently strand
    every LUN-backed pod).
P2. `container/add` with `root-dir` on an iSCSI-disk mount and no
    `file=`/`remote-image=` — the untested pre-populated-root-dir probe
    inherited from TODO #18.
P3. Scale: attach/detach latency via the API, and where ROS gets unhappy on
    LUN count (target ~30–50 disks = one per pod).
P4. mkfs/loop-mount inside a RouterOS container (no host /dev): confirm
    loopback mounts work in-container, or fall back to extracting through a
    userspace ext4 writer in sbregistry (pure-Rust, no mount needed).

Any P1/P2 failure kills the design cheaply; P4 has a software fallback.

## Phases

- **0** Capability probe (above).
- **1** stormblockmk packaging: file-backed slab profile, musl/scratch build,
  stormd config, IPC exposed to sibling process. *(stormblock repo)*
- **2** sbregistry MVP serving **mkube only**: OCI push API + golden build +
  clone hand-off + supervise/update mkube. Retires mkube-update.
  Success metric: push→serving < 60s, mkube downtime < 15s.
- **3** Push webhook to mkube for all images (retires digest polling
  everywhere; TODO #3 done as a side effect).
- **4** Pod root-dirs on clones behind the opt-in annotation; GC/refcounts;
  soak on non-critical pods; then flip default (control plane exempt).
- **5** PXE install-media LUNs; retire mikrotik file targets.
- **5b** `stormblock` storage class for PVCs (thin volume + snapshot support
  via `pvc_iscsi.go` seam); PVC snapshot/clone API surfaced in mkube
  (`POST /pvc/{name}/snapshot`); migration tooling dir→volume.
- **6** Optional: server9 stormblock as a second, fabric-backed pool for bulk
  PVC data (never control plane).

## Failure modes & stances

- **sbregistry crash ⇒ all LUN-backed pods lose disks simultaneously.** This
  container inherits the "must never break" crown from mkube-update: minimal
  dependencies, boring code, stormd auto-restart, and P1's reconnect testing
  must show pods survive a target bounce (fs remount, container restart at
  worst). Control-plane exemption bounds the blast radius.
- **Who watches the watcher:** RouterOS `start-on-boot` + a dumb scheduler
  health script restart the container; sbregistry updates *itself* by
  pre-staging its own new root-dir on plain fs and exec-ing through stormd —
  the one component whose update path stays tarball-simple, by design.
- **Page cache is per-clone** (no shared lowers like overlayfs) — accepted;
  rose RAM headroom to be watched during phase-4 soak.
- **Slab-on-ext4 write amplification** — accepted for the control pool;
  measure during soak; server9 pool (phase 6) is the escape hatch for heavy
  data.
