# RouterOS Container / ROSE Changes Relevant to mkube

Gathered 2026-08-10 from official MikroTik changelogs (`download.mikrotik.com/routeros/<ver>/CHANGELOG`), covering every 7.x release across all four streams.

**Stream status as of 2026-08-10**

| Stream | Version |
|---|---|
| long-term | 7.21.5 |
| stable | 7.23.3 |
| testing | 7.24rc3 |
| development | 7.24beta3 |

---

## Highest-impact items for mkube

### 7.24 (testing/development — not yet stable)
- **Initial support for RKE2** — MikroTik is adding native Kubernetes (RKE2) support. Directly overlaps with mkube's territory; watch closely.
- **Privileged mode** for containers.
- `container save` command — export container images from the device.
- Per-container and global **`swap-max`** limits, `swap-current` usage reporting.
- Health checks: reduced flash writes when running health checks (implies health-check machinery is active).
- `start-on-boot` now retries on certain startup errors (previously could give up).
- Container refuses to start with an empty default DNS list and no DNS override.
- Environment variables no longer printed in the log at container startup (log-scraping behavior change).
- Shell gets `TERM=xterm` default.

### 7.23 (current stable) — restart/health/memory orchestration primitives
- **`restart-policy=no/always/on-failure`**, `restart-count`, `restart-interval`, `restart-max-count`, **`stop-on-unhealthy`** — RouterOS now does restart supervision natively. mkube's own restart logic may fight or duplicate this; decide who owns restarts.
- **OOM-kill detection** — containers killed by the OOM killer are detected and shown.
- `memory-max` settable globally and per container.
- Layer hygiene: cleanup of layers for non-existing containers; layer size calculation fixed; container size + data size reported.
- `noexec` mount option; user mounts may override `/sys` and `/dev`; `/dev/net/tun` permission update.
- Adding a container now **fails if `root-dir` already exists** (validation change — affects programmatic creation).
- `/app` gained `network-outgoing-access=yes/no` (egress blocking) and more bundled apps (portainer, dockge, komodo, etc.).
- Route: reverted to old routing-rule priorities for containers (a 7.22 change was rolled back — container routing differed in 7.22.x).
- SMB server no longer starts on container interfaces.
- 7.23.2: fixed missing `config.json` when upgrading from ≤7.20.8 (same fix as 7.21.5).

### 7.22 — repull automation
- **Automatic stop/repull/start when `remote-image` changes** or on repull — native image-update flow; mkube's update flow should account for it.
- zstd layer extraction support.
- **7.22.2: fixed losing container after reboot** — avoid 7.22.0/7.22.1 for any deployment.
- rose-storage: XFS support.
- Container shell now uses user-defined envs/envlist.

### 7.21 (current long-term) — big config-model changes
- **BREAKING-ish: mounts converted to mountlists** — old mount name becomes list name; a list name can map to multiple mounts. Any mkube code generating `/container` mount config must handle the new model.
- **`/app` menu introduced** (simple containerized app install; needs `container` device-mode).
- New commands: `kill` (signals), `run` (interactive), `update`, `stop-time` setting.
- CPU: usage reporting, option to limit CPUs used by containers.
- `hosts` setting; extra ENV vars directly on the container; enable/disable individual envs and mounts; mounts directly on container.
- **Per-container `layer-dir`** — separate layer stores per container set; `layer-dir` may not sit inside a container's root-dir.
- `image-id` field; **image import data stored → containers survive netinstall**.
- Root-dir size and volume size calculation.
- veth: `container-mac-address` setting, `dhcp` auto-config setting, static DHCPv4 leases allowed on VETH interfaces.
- `disk`: type=file devices without rose-storage (file-based swap).
- Patch-release gotchas in this stream:
  - 7.21.1: app auto-update now **off by default**; fixed containers not starting with large mounts.
  - 7.21.2: **default registry changed to docker.io** (was lscr.io since 7.18); **tmpfs no longer mounted on `/tmp` and `/run` by default**; container won't start if any volume is unmounted; `shm_size` support; nftables/iptables "Message too long" fix; mounts writable by user.
  - 7.21.4: fixed container not starting after upgrade when `root-dir` was unset.
  - 7.21.5: fixed missing `config.json` when upgrading from ≤7.20.8.

### 7.20 — cgroups, exec, logs, devices
- **cgroup support: cpuset, cpu, memory, pids**; per-container memory limiting and monitoring.
- **`/container/log` menu** (100 messages kept per container) — native log capture.
- **Exec into containers**: `/container/shell cmd= user=`.
- `repull` command added.
- **Multiple VETHs per container**; in-container interface name now matches RouterOS name.
- veth `dhcp=yes/no` and `mac-address` properties; stable MACs (container side = RouterOS side +1).
- Device passthrough (`device` option from `/system/hardware`), direct hardware access, KVM available inside containers (QEMU accel on arm64/x86).
- Mount improvements: read-only mounts, single-file mounts, multiple envlists.
- Container-in-container initial support; SCTP support.
- `config.json` exposed to user; explicit `stopped` flag; duplicate `root-dir` disallowed; `root-dir=/` disallowed.
- **Containers terminated cleanly on shutdown** (allowed to clean up).
- check-certificate enabled by default for new remote imports.
- 7.20.8: nftables/iptables "Message too long" fix (backport of the 7.21.2 fix).

### 7.19 — device-mode + naming
- **New `rose` device-mode with `container` feature enabled by default** — device-mode gates whether containers can run at all; mkube provisioning must ensure the right mode.
- Container rename allowed; human-readable names derived from remote-image/file.
- `/ip/service` now shows all TCP/UDP ports including ports open inside containers.
- 7.19.1: container stability improvements.
- **7.19.4: packet sockets in containers no longer disable RouterOS fastpath/fasttrack** — performance-relevant if any mkube workload opens raw/packet sockets.
- rose-storage: Btrfs balance/degraded-mount, RAID default device renamed `raid` → `raid-array` (naming break for scripts).

### 7.18 — registry behavior
- Default `registry-url=https://lscr.io` (later changed to docker.io in 7.21.2).
- HTTP redirects allowed when accessing registries; registry can be specified in `remote-image`; improved arch selection.
- **Layer unpack now defaults to the parent directory of `root-dir`** — layers download directly onto the target disk (affects disk-space planning on flash vs. external disk).
- Swap without container package; rose-storage gained full advanced Btrfs feature set (multi-disk, subvolumes, snapshots, send/receive, compression).
- 7.18.2: fixed repository-name handling breaking redirects with basic auth (also in 7.19).

### 7.17 and earlier (foundation)
- 7.17: `.tar.gz` import; UID/GID range fix; `start-on-boot` stability; SWAP support on block devices (with container package); quieter start/stop logging unless enabled.
- 7.16: VETH address cleared on container exit; interface marked running only when in use.
- 7.15: `ram-high` validation.
- 7.14: VETH management responsiveness/reliability; `/container/shell` restricted to users with write permission; ROSE SMB replaced legacy SMB.
- 7.12: WinBox multi-address + IPv6 on VETH.
- 7.11: **IPv6 on VETH**; **overlayfs layers option**; volume-mount ownership adjustment outside container UID range; duplicate image-name fix; hosts-file IP fix; `container` profile classifier.
- 7.10: OCI manifest pull fix; default internal env values.
- 7.9: OCI manifest support in pull.
- 7.8: **rose-storage package introduced**; registry authentication; multi-container start-on-boot fix; ownership fixes after upgrade.
- 7.7: tmpfs for ram/tmp dirs; Dockerfile user/group handling; tar extraction fixes.
- 7.6: `start-on-boot` added; live parameter changes; unauthenticated registry fix.
- 7.5: container support first shipped in stable (ARM, ARM64, x86).
- 7.4: container package in testing channel only.

---

## Upgrade traps to remember

- **≤7.20.8 → 7.21+/7.22/7.23**: missing `config.json` bug; fixed in 7.21.5 / 7.23.2 / 7.24. Upgrade targets should be at least those patch levels.
- **Unset `root-dir`**: containers could fail to start after upgrade (fixed 7.21.4 / noted again in 7.22).
- **7.22.0/7.22.1**: containers could be lost after reboot (fixed 7.22.2) — do not deploy these.
- **7.21.2 behavior changes**: default registry → docker.io; `/tmp` and `/run` no longer tmpfs by default; container refuses to start with unmounted volumes.
- **7.21 mounts → mountlists** conversion.
- **7.22 vs 7.23 container routing-rule priorities** differ (7.23 reverted 7.22's change).
- **7.20+**: check-certificate defaults to yes for new remote imports — matters for the local mkube registry if it serves HTTP/self-signed TLS.
- **7.18+**: layer unpack location moved to parent dir of `root-dir`.

## Opportunities for mkube

- Native primitives now exist that mkube can delegate to or must reconcile with: restart policies + health checks (7.23/7.24), per-container cgroup limits (7.20/7.21/7.23), OOM detection (7.23), container logs (7.20), exec (7.20), repull-on-image-change (7.22), swap limits (7.24), privileged mode (7.24), `container save` (7.24).
- `/ip/service` port visibility (7.19) and per-container CPU/memory stats + graphs (7.21/7.23) are useful for mkube monitoring without agents.
- Netinstall-surviving image import data (7.21) improves node recovery stories.
- **RKE2 support landing in 7.24** is the strategic item: MikroTik is moving toward first-class Kubernetes on RouterOS.
