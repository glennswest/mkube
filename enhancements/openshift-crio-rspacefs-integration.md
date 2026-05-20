# OpenShift / podman / CRI-O integration via containers-storage `mount_program`

**Status:** proposed — 2026-05-20
**Owner:** glennswest
**Scope:** Make `rspacefs-mount` the FUSE mount helper that containers-storage shells out to when assembling container rootfs on OpenShift, podman, and bare CRI-O. Uses the **existing** containers-storage configuration knobs (`mount_program` + `additionalimagestores`) — no new APIs, no CRDs, no kernel patches. OCI image distribution stays unchanged.

This replaces an earlier (deleted) spec that invented a custom registry backend. That was the wrong abstraction; the right extension point for the container runtime is `mount_program`.

## Background

containers-storage is the Go library that `podman`, `buildah`, `skopeo`, and CRI-O all use to manage on-disk container storage. Its `/etc/containers/storage.conf` exposes two pre-existing knobs that together cover everything rspacefs needs to expose:

- **`mount_program`** — path to a user-space binary that mounts the container rootfs in place of the kernel `mount -t overlay` call. The reference implementation is `fuse-overlayfs`; argv layout is overlay-mount-style: `mount_program [-o lowerdir=X:Y,upperdir=A,workdir=B] /mountpoint`.
- **`additionalimagestores`** — list of additional **read-only** image stores containers-storage will consult for layers. Lets a node serve pre-staged layers without going to the registry.

There has been upstream discussion of a proper snapshotter interface in CRI-O (analogous to containerd's snapshotter plugin model). It has not landed. Until it does, `mount_program + additionalimagestores` is the supported plug point.

## Architecture

```
                        OpenShift / podman / CRI-O node
   ┌─────────────────────────────────────────────────────────────┐
   │  CRI-O / podman / buildah                                    │
   │       │                                                      │
   │       ▼ image management (pull, store, mount)                │
   │  containers-storage  ← reads /etc/containers/storage.conf    │
   │       │                                                      │
   │       │ mount_program = /usr/bin/rspacefs-mount-shim         │
   │       │ additionalimagestores = ["/var/lib/rspacefs/store"]  │
   │       ▼                                                      │
   │  rspacefs-mount  ← assembles layered rootfs via FUSE         │
   │       │                                                      │
   │       │ (lower layer fds; FUSE passthrough on kernel ≥ 6.9)  │
   │       ▼                                                      │
   │  /var/lib/containers/storage/overlay/<id>/merged             │
   │       │                                                      │
   │       │ container runtime pivot_root's here                  │
   │       ▼                                                      │
   │  Running container (sees a normal rootfs)                    │
   └─────────────────────────────────────────────────────────────┘
```

No custom registry. No custom kube API. The OCI image registry (Quay, internal OCP registry, GHCR, anything) stays exactly as it is.

## What rspacefs-mount needs

Today's CLI is `rspacefs-mount --upper DIR --lower DIR... [--lower-verified-pinned DIR=MFS=TREE] MOUNTPOINT`. The `mount_program` contract is overlay-mount-style.

Two options:

### A. Native argv mode in `rspacefs-mount`

Detect overlay-mount-style argv and parse it natively:

```
rspacefs-mount -o lowerdir=L1:L2:L3,upperdir=U,workdir=W /mnt/point
```

Maps to:
- `--upper U` ← `upperdir=`
- `--lower L1 --lower L2 --lower L3` ← `lowerdir=` (top-down order, `:`-separated)
- `--upper-work W` ← `workdir=` (we don't use a work dir; just accept and ignore)

Plus: respect `volatile` mount option as a hint (skip fsync — perf), and `allow_other`, etc.

For verity-pinned lowers, an out-of-band hint: an environment variable `RSPACEFS_VERITY_LOWERS` containing a JSON map of `{lower_dir: {manifest, tree}}`. Set by the node operator's systemd unit / DaemonSet that provisions storage.conf.

### B. Shim script `rspacefs-mount-shim`

A small shell or Rust wrapper that translates overlay-style argv into `rspacefs-mount`'s native flags. Less invasive to rspacefs itself, but adds a process.

**Recommendation:** ship native mode (A) — it's ~50 lines of clap arg parsing, no extra process. Keep `--upper`/`--lower` style as the friendly alternative for direct invocation.

## OpenShift deployment

A DaemonSet on every compute node:

1. **Installs the binary** — `/usr/bin/rspacefs-mount` (statically linked musl, single file).
2. **Configures `/etc/containers/storage.conf`** — drops in:
   ```toml
   [storage]
   driver = "overlay"
   [storage.options]
   mount_program = "/usr/bin/rspacefs-mount"
   additionalimagestores = ["/var/lib/rspacefs/store"]
   mountopt = "nodev"
   ```
3. **Prepares the additional store** — `/var/lib/rspacefs/store/` initially empty; populated by image pulls (containers-storage does this).
4. **Optionally stages verity manifests** — a sidecar that, when a layer is pulled, computes (or pulls) the per-layer verity manifest and writes it to a known sidecar location. `rspacefs-mount` then runs the lower in pinned-verified mode if a manifest is present.
5. **Restarts CRI-O** with the new config (rolling, drained nodes).

## What the user / operator does

Almost nothing. From the cluster operator's perspective:

```sh
oc apply -f rspacefs-installer.yaml
```

DaemonSet rolls out, every node has the binary + storage.conf entry. No application changes. No new API to learn. Existing `podman build`, `podman pull`, `oc apply` workflows are unchanged.

## What rspacefs gives OpenShift here

Reordering by impact:

| Win | Why |
|---|---|
| **Rootless containers without `CAP_SYS_ADMIN`** | Kernel overlayfs needs caps; `mount_program` runs in userspace. The standard use case for fuse-overlayfs; rspacefs picks it up. |
| **Tamper-evident lower layers (optional)** | Pin a verity manifest per layer; reads through rspacefs-mount verify each block. |
| **Reads bypass the daemon on kernel ≥ 6.9** | rspacefs-mount uses FUSE passthrough for non-verified files (already shipping). Near-native read speed. |
| **Symlinks / xattrs / mode bits preserved through the mount** | rspacefs-mount handles all of these (already shipping). |
| **Streaming reads** | Large lower-layer files (libs, binaries) don't get buffered in memory (already shipping). |

What rspacefs does NOT change in this story:

- **Image distribution** — registry is whatever the cluster admin already runs.
- **The pull path** — `podman pull` works unchanged.
- **The push path** — `podman push` / `buildah` write standard tarballs to the registry.
- **CRI-O / kubelet / k8s API** — unchanged.
- **`podman commit`** — still works (tarballs the diff). Faster commit is a separate spec; out of scope here.

## Long-term: a real CRI-O snapshotter

Upstream conversation has floated a CRI-O snapshotter interface modeled on containerd's. When that lands, rspacefs becomes a CRI-O snapshotter plugin — cleaner integration, no `mount_program` shim. Not on our critical path; not blocking.

## Implementation order

1. **Add native overlay-style argv mode to `rspacefs-mount`** (~50 LOC):
   - Recognize `-o ...` form
   - Parse `lowerdir`, `upperdir`, `workdir` (workdir is a no-op for us — accept and ignore)
   - Recognize/honor `volatile`, `allow_other`, `auto_unmount`
   - Read `RSPACEFS_VERITY_LOWERS` env for pinned-verity hints (JSON map)
2. **Test against real containers-storage**:
   - Set up a Fedora node (we have dev.g8.lo)
   - Install `rspacefs-mount` binary
   - Edit `/etc/containers/storage.conf` with `mount_program = /usr/bin/rspacefs-mount`
   - `podman pull alpine:latest`
   - `podman run --rm alpine:latest /bin/sh -c "echo hello"`
   - Verify the mount happens, run completes, unmount fires
3. **Write a DaemonSet** for OpenShift deployment (`deploy/openshift-rspacefs-daemonset.yaml`).
4. **Document the verity-staging sidecar pattern** (separate spec — `openshift-verity-staging.md`).
5. **Track upstream** CRI-O snapshotter discussion; switch when available.

## File locations referenced

- `rspacefs/crates/rspacefs-fuse/src/main.rs` — extend CLI parser
- `mkube/enhancements/openshift-crio-rspacefs-integration.md` — this spec
- `mkube/enhancements/nfs-rootdir-containers.md` — the MikroTik counterpart (NFS root-dir)
- `mkube/enhancements/mkube-boot-from-registry.md` — mkube self-boot pivot
- `nextnfs/enhancements/rspacefs-export-type.md` — nextnfs side of the MikroTik path

## Out of scope

- **Faster `podman commit`** — covered separately. Containers-storage doesn't currently support non-tarball commit. Separate problem; would need a buildah/podman extension.
- **Image distribution changes** — none. Standard OCI v2 stays.
- **Kubernetes / OpenShift API additions** — none. No CRDs. No custom operators beyond the DaemonSet that drops in storage.conf.
- **MikroTik / RouterOS** — different integration (NFS root-dir). See `nfs-rootdir-containers.md`.
