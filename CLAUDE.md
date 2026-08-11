# CLAUDE.md — mkube Project

## Build & Deploy

```bash
make build-all                                   # All binaries
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube/  # mkube only
make deploy                                      # Deploy to rose1
make deploy-installer                            # Bootstrap fresh RouterOS device
go test ./...                                    # Run tests
```

- Always use `podman`, not docker
- Container images use `scratch` base (no OS layer)
- Push to GHCR; registry watcher mirrors to local registry at `192.168.200.3:5000`

## Architecture

### Binaries
| Binary | Location | Runs on | Purpose |
|--------|----------|---------|---------|
| mkube | `cmd/mkube/` | RouterOS (ARM64), Proxmox (x86_64) | Main controller |
| mkube-update | `cmd/mkube-update/` | RouterOS (ARM64) | Image update watcher |
| mkube-registry | `cmd/registry/` | RouterOS (ARM64) | Standalone OCI registry |
| installer | `cmd/installer/` | Mac (local) | One-shot RouterOS bootstrap CLI |
| pve-deploy | `cmd/pve-deploy/` | Mac (local) | Deploy OCI images as Proxmox LXC |
| mkube-boot | `cmd/mkube-boot/` | Proxmox LXC (x86_64) | Bootstrap mkube on Proxmox |
| mkube-agent | `cmd/mkube-agent/` | CoreOS (x86_64) | Job execution agent for bare metal |

### Key Packages
| Package | Purpose |
|---------|---------|
| `pkg/console/` | Built-in web dashboard UI (Dracula theme) |
| `pkg/provider/` | Pod lifecycle, deployments, BMH, consistency, API routes |
| `pkg/network/` | Multi-network IPAM, veth/bridge management |
| `pkg/storage/` | OCI→tarball, volume provisioning, image cache |
| `pkg/store/` | NATS JetStream KV persistence, YAML import/export |
| `pkg/dns/` | microdns REST API client |
| `pkg/dzo/` | DNS Zone Orchestrator (cross-zone management) |
| `pkg/lifecycle/` | Boot ordering, health checks, watchdog |
| `pkg/registry/` | OCI registry implementation |
| `pkg/routeros/` | RouterOS REST API client |
| `pkg/proxmox/` | Proxmox VE REST API client, VMID allocator, OCI→LXC converter |
| `pkg/pvectl/` | Proxmox LXC deploy library |
| `pkg/runtime/` | Container runtime abstraction (RouterOS, StormBase, Proxmox) |
| `pkg/nats/` | Embedded NATS server (in-process JetStream) |
| `pkg/cluster/` | Multi-node clustering (peer health, push sync, full resync) |
| `pkg/diskimg/` | Pure Go disk image converters (VMDK, QCOW2, VHD → raw) |
| `pkg/podman/` | Pure Go Podman REST API client via Unix socket |
| `pkg/bmc/` | IPMI BMC client for power control and boot device management |
| `pkg/gitbackup/` | Git-backed config state backup via rust4git State API |

### Backends
| Backend | Config key | Runtime adapter | Network driver |
|---------|-----------|-----------------|----------------|
| RouterOS | `backend: routeros` (default) | `pkg/runtime/routeros.go` | `pkg/network/driver/routeros.go` |
| StormBase | `backend: stormbase` | `pkg/stormbase/client.go` | `pkg/network/driver/stormbase.go` |
| Proxmox | `backend: proxmox` | `pkg/runtime/proxmox.go` | `pkg/network/driver/proxmox.go` |

### Infrastructure
| Host | IP | Role |
|------|-----|------|
| rose1.gw.lo | 192.168.1.1 | MikroTik ARM64, runs mkube + all containers |
| pvex.gw.lo | 192.168.1.160 | Proxmox node, gw microdns (CT 117) |

### Container IPs (gt network)
| Container | IP | Notes |
|-----------|-----|-------|
| rose1 (gw) | 192.168.200.1 | Gateway |
| mkube | 192.168.200.2 | API on :8082 |
| registry | 192.168.200.3 | HTTPS :5000 |
| mkube-update | 192.168.200.5 | — |
| NATS | 192.168.200.10 | :4222 |
| gt DNS | 192.168.200.199 | microdns |

### DNS Servers
| Network | DNS IP | Zone |
|---------|--------|------|
| gw | 192.168.1.252 | gw.lo (mkube container `gw/dns`, microdns) |
| g8 | 192.168.8.252 | g8.lo |
| g9 | 192.168.9.252 | g9.lo |
| g10 | 192.168.10.252 | g10.lo |
| g11 | 192.168.11.252 | g11.lo |
| gt | 192.168.200.199 | gt.lo |
| g16 | 192.168.31.252 | g16.lo (flat /20 core fabric via dsw1 — see below) |

### g10 → g16 physical collapse (planning reference, 2026-08-06)

The Dell core switch `dsw1` (`../dellsw`) now carries a flat **`192.168.16.0/20`**
fabric. In the "g10 collapse", the 40G switch (`switch10`/CRS326) and the
bare-metal nodes behind it moved **off** rose1's `bridge-g10` (`qsfp28-2-1`)
**onto** dsw1, so those physical nodes are now L2-adjacent to the /20, not g10.

rose1's `bridge-g10` and its mkube containers (incl. `g10_dns_microdns`) are
**unchanged** — only the physical nodes moved. **The g10 entry in
`deploy/rose1-config.yaml` is retained on purpose as the migration reference**;
do not delete it until the code changes below are planned and done.

Mapping to work through when repointing the bare-metal/PXE path to g16:

| Concern | g10 (current config, kept as reference) | g16 (new physical home) |
|---|---|---|
| CIDR / gw | `192.168.10.0/24` / `192.168.10.1` | `192.168.16.0/20` / `192.168.16.1` |
| DNS/DHCP | `192.168.10.252` (g10.lo) | `192.168.31.252` (g16.lo), relay `relay-g16` |
| DHCP range | `192.168.10.100–199` | hosts from `.16.10`; DHCP `.16.100+` |
| IPAM | `192.168.10.200–250` | TBD in a g16 `networks:` entry |
| PXE `nextServer` | `192.168.10.200` / `192.168.10.9` | must be provided on g16 |
| `pxeManagerURL` | `http://pxe.g10.lo:8080` | `pxe.g16.lo` equivalent, TBD |
| `iscsiPortalIP` | `192.168.10.1` (node-local on g10) | still routable via rose1, no longer node-local |

Open decision: give g16 a full `networks:` entry (IPAM/DHCP/PXE) mirroring g10,
or repoint g10's. Not decided/changed — flagged for the owner. See CHANGELOG.

## Key Patterns

- **Naming**: `{namespace}_{pod}_{container}` for RouterOS containers, `veth_{ns}_{pod}_{i}` for veths
- **Persistence**: All state persists in NATS JetStream KV
- **Reconcile**: 10s loop — desired state (NATS + boot-order) vs actual containers
- **Image updates**: `vkube.io/image-policy: auto` — digest check, rolling update on change
- **DNS**: Automatic registration via microdns REST API
- **RouterOS**: Use `remote-image` for container creation (NOT `tag`)
- **RouterOS transport**: Native API (port 8728) via go-routeros/v3, HTTP only for file uploads
- **Scratch containers**: No system root CAs — use local registry only
- **API access**: `kubectl` hangs — use `curl` with JSON or `mk` alias

## API Reference

See [docs/api.md](docs/api.md) for the full REST API reference and `mk` CLI shorthand.

## Testing

```bash
go test ./...                        # All tests
go test ./pkg/provider/...           # Provider tests
go test ./pkg/proxmox/...            # Proxmox tests
go test -v ./pkg/provider/...        # Verbose
```

Known test failures (pre-existing):
- `pkg/storage/manager_test.go`: `TestProvisionVolume` (`TestEnsureImageCacheHit` fixed 2026-06-30 by the digest-validated cache)

## Work Plan

### Current Version: `v6.3.0`

### TODO (priority order)
1. **BareMetalHost Operator (BMO)**: Full host state machine, serial proxy, Redfish, ownership model. Separate project repo. (IPMI power control now built into mkube via `pkg/bmc/`.)
2. **DNS 2-replica deployment**: Per zone via Deployment controller. Requires anti-affinity (multi-node).
3. **Registry push notifications to mkube-update**: Webhook/watch instead of polling. **Superseded by the [stormblock-registry](https://github.com/glennswest/stormblock-registry) project (`docs/spec.md`)** (sbregistry absorbs mkube-update; fires webhooks by construction) — only do standalone if that spec stalls.
4. **Fix storage test failures**: `TestProvisionVolume` (volume path mismatch). `TestEnsureImageCacheHit` fixed 2026-06-30 by the digest-validated cache.
5. **TLS cert rotation**: API to update registry CA+server certs and trigger consumer reload.
6. **microdns resilience**: DNS containers must survive mkube failures independently.
7. **Registry HTTP/2 proper fix**: Find root cause of Go h2 GOAWAY or use reverse proxy.
8. **Proxmox integration test**: Smoke test `backend: proxmox` against pvex.gw.lo.
9. **Proxmox PVE 9.1+ native OCI**: Pass OCI ref directly to `pct create`.
10. **BMH scheduled power on/off**: Honor `bmh.mkube.io/power-on-days`, `power-on-time`, `power-off-days`, `power-off-time` annotations. Reconcile loop should auto-power-on/off hosts based on day-of-week + time-of-day schedule.
11. ~~**RouterOS native API reconnect race**~~ **FIXED 2026-08-09** (`pkg/routeros/client.go` + `third_party/routeros/` fork): TWO upstream go-routeros bugs were the real root cause. (a) `RunArgsContext` registered the reply tag AFTER sending the request — on a LAN RouterOS a fast reply beat the registration, was dropped by the async loop, and the caller hung forever (all 16 concurrent CreatePods wedged silently 2026-08-09 23:27Z). (b) A request's ctx expiry called `reader.Cancel()` on the SHARED reader → io.EOF → async loop teardown → every in-flight request failed (the 2026-05-15 and 2026-08-09 22:57Z "unreachable after reconnect" mass-CreateFailed cascades). Fixed in the in-tree fork (`replace` directive): register-before-send, abandon-own-tag-only on ctx expiry. Client side: `nativeRun` no longer treats ctx deadline as connection death, retries real connection errors 4× with backoff, forces reconnect only after 3 consecutive deadline expiries, and caps deadline-less requests at 3 min so lost replies can never hang a worker again.
12. **CreatePod must clean up partial root-dir on failure** (`pkg/provider/provider.go`): When veth/mount allocation or container/add fails mid-CreatePod, the tarball-extracted `/raid1/images/<name>` root-dir is left behind. The next reconcile retry hits RouterOS error `root-dir overlap with /raid1/images/<name>`, requiring manual cleanup or a fallback path. Observed 2026-05-15 16:38Z–16:40Z on g11/ipmiserial, gt/pvc-test, g9/dns, gt/dns immediately following the reconnect race in #11. Failure path should `RemoveDirectory(rootDir)` so retries are idempotent.
13. **Pluggable store backend interface** (`pkg/store/`): Abstract the persistence layer behind a `Backend` interface so NATS JetStream KV is one implementation among several. Goal: allow swapping in an etcd-compatible backend (e.g. an in-house Rust etcd) without touching call sites. Config key `store.backend: nats|etcd` selects implementation at startup; NATS remains the default. Motivation: stack consolidation onto in-house components and a path toward kubectl/k8s tool compatibility via a future kube-apiserver shim. Note: etcd alone does not yield kubectl compat — the apiserver translation layer is separate work and is NOT in scope for this TODO. Scope here is strictly the backend seam: define `Backend` interface (Get/Put/Delete/Watch/List/CAS), refactor existing NATS code to satisfy it, add config plumbing, and document the contract for future implementers.
14. **Switch default store backend to fastetcd**: Cutover from NATS JetStream KV to the in-house Rust etcd replacement (fastetcd) as the default `store.backend`. Depends on #13 (backend interface) and on fastetcd reaching production readiness (durability under power loss, crash recovery, multi-writer correctness, watch fan-out, lease semantics). Plan: ship fastetcd backend as opt-in alongside NATS; soak on one or two non-critical mkube instances; add a one-shot YAML export/import migration path so existing clusters can move their state without data loss; flip the default once parity is demonstrated. Keep NATS implementation in tree for at least one release after the flip as a fallback. Out of scope: removing NATS entirely (it's still used for embedded messaging beyond KV) — only the KV role moves.
15. **kube-apiserver compatibility shim**: Implement a kube-apiserver-shaped REST surface in front of mkube's store so `kubectl`, Helm, ArgoCD, and other k8s ecosystem tools can target mkube directly. Depends on #13 (backend interface) and ideally #14 (fastetcd default, since the shim's watch/MVCC/lease assumptions map cleanly onto etcd semantics). Scope: translate kube REST verbs (GET/LIST/WATCH/POST/PUT/PATCH/DELETE) and resourceVersion/MVCC semantics onto the `Backend` interface; expose `/api`, `/apis`, `/openapi/v2`, `/version` discovery endpoints; serve at least core/v1 (Pod, Service, ConfigMap, Secret, Namespace) and mkube's existing custom resources via CRD-style registration; TLS + client cert auth compatible with kubeconfig. Out of scope initially: full admission webhook chain, RBAC enforcement, audit logging. Document which kubectl subcommands are supported and which are not.

16. **Pod update wedges (partially fixed 2026-08-06)**: The two root causes shipped: `handlePatchPod` now does a real merge patch (it used to persist the patch body as a full replace — an annotations-only patch stored a pod with **no containers**, stranding it), and `UpdatePod` got an explicit network/static-ip change path (teardown on old network + CreatePod on new). **Still open:** (a) `images/redeploy` / stale-image goroutines can hang inside `blueGreenUpdate` (observed: cloudid redeploy stuck >10 min with no events) and **leak the `redeploying` flag**, after which the reconciler permanently skips the pod — flag needs a deadline/defer-clear and the stuck pull/staging needs a timeout; (b) synchronous CreatePod/UpdatePod in HTTP handlers exceeds client timeouts (curl sees 000) — should enqueue to the pod worker and return 202; (c) after a PATCH → UpdatePod fallback recreate, the in-memory tracked pod (`p.pods`) is not refreshed from the merged/persisted object — GET serves stale annotations and the 3c auto-update check misses a newly added `image-policy: auto` until the next mkube restart (observed 2026-08-07 enabling self-update on configman: store had the annotation, memory didn't); (d) **CreatePod can complete with volumeMounts silently missing** — observed 2026-08-08 on infra/stormblockmk recreate: container came up Running with ZERO mount entries (PVC absent), the storage engine inside formatted fresh state on container tmpfs. CreatePod must verify every spec volumeMount landed on the runtime (mounts list) and fail/retry otherwise.
17. **DNS failover IP per server (a/b NIC load balance)** (`pkg/provider/baremetalhost.go` + microdns): For each BMH with secondary NICs, also register a bare `serverN.<zone>` record carrying all NIC IPs (a + b), with microdns health checks (ping) so only live NICs are served. **Default/preferred answer is the lower IP (a-side)**; fail over to the b-side when a is down. microdns already supports record-level `health_check` and load balancing — wire BMH sync to create/maintain the aggregate record. (Requested 2026-08-06.)
18. **Overlayfs image catalog (zero-copy recreate)** — **superseded by the [stormblock-registry](https://github.com/glennswest/stormblock-registry) project** (CoW at the StormBlock GEM layer; no RouterOS overlay dependency, ext4 no-reflink moot). Kept for context: (`pkg/storage/`, `pkg/runtime/routeros.go`): Eliminate the per-pod tarball untar on `CreatePod`/recreate by sharing a single read-only golden rootfs per image digest as an overlay *lower*, with a per-pod writable *upper* — so a recreate is a mount, not a copy. This is the "avoid it all" endgame for the prepull→pre-extract→catalog plan. **Owner has a solution built (not yet integrated — deferred, 2026-06-30).** Context already shipped: (Tier 1) `runImageStager` prepull keeps pulls off the recreate critical path, and the digest-validated cache (`<tarball>.digest` sidecar) reuses staged tarballs across restarts — together these drop a cold recreate to a ~1s untar. The catalog's *clone-per-pod* variant (reflink a golden dir) is **ruled out on current storage**: `/raid1` is **ext4** (confirmed live via `/api/v1/storagepools`), which has no reflink/FICLONE, so a clone is a full byte copy ≈ the untar it would replace — no win. Overlayfs sidesteps this (no copy at all, shared page cache across pods of the same image). Hard dependency: **RouterOS overlay/layered-image support** for container `root-dir` (a pre-extracted lower + writable upper) — confirm what ROS exposes, or back it with stormfs. Alternative path if overlay is unavailable: reformat `/raid1` to a reflink-capable fs (XFS-reflink/btrfs/ZFS) to reopen the clone-catalog approach. A capability probe (RouterOS accepting a container created against a pre-populated `root-dir` with no `file=`/`remote-image=`) is still untested — gate integration on it. Maps to the `[[borg-pattern-at-vfs-layer]]` VFS-layer materialization model.

### In Progress
- [ ] (2026-08-10) **CoW catalog — blocked on RouterOS image extraction onto network disks.** Model proven: a container runs its binary from a mounted stormblock clone (5 KB generic docker-save stub as `file=` + clone at `/payload` + entrypoint rewritten). Pipeline implemented (`pkg/provider/cow_catalog.go`, `vkube.io/image-mode: cow`): golden create→format→seed→seal, per-pod clone with its own UUID/label/clean state, attach, mount, container start. **Storage is NOT the problem** — `POST /api/v1/probes/datapath` writes stamped blocks through `iscsi-pvc` and verifies 64/64 good in a fresh session, after export withdrawal, and **after seal → `from_template` clone**; thin volumes, partial slab slots, snapshots and clones are byte-exact. **What fails is RouterOS writing a filesystem onto a mounted network volume**: allocation reaches the full image size, then the volume will not mount (`fs=-`) and clones of it mount empty. Ruled out: writeback settle, clean-flag restore, eject (hardware-only), unmount (absent), disable (no effect), SYNCHRONIZE CACHE (succeeds, no effect), duplicate fs identity. Related: the "RouterOS never mounts an NVMe-TCP disk" claim was retracted 2026-08-11 (a port bug on our side) — so this seeding failure is worth **re-testing over NVMe**, which is a different write path than the iSCSI one it was observed on. **Next experiment:** seed by *copying* rather than extracting — RouterOS extracts happily to `/raid1` (a hardware disk, which is how every pod works today), so run a container with `/raid1/<golden-src>` and the clone both mounted and `cp -a` between them; container writes to PVC mounts already appear durable, so this may sidestep the extraction path entirely. rose1 runs `goldenSource: sbregistry`. Probes: `/api/v1/probes/{cow,nvme,layers,layerdir,format,lsmount,inspect,datapath}`.
- [ ] (2026-08-06) **g16 follow-ups**: `fedora-siov` (Mellanox b8:59:9f:52:23:46) identified as the **pvex Mellanox card** — an SR-IOV test VM on the Proxmox node; owner will convert it to a plain Linux NIC (MAC will change), then give it a static below 16.100; discover server7's b-port MAC and fill the reserved 192.168.16.113; TODO #16 (network-annotation strand bug) and #17 (failover DNS per server, prefer lower IP).

### Recently Completed
- [x] (2026-08-11) **stormblock path is NVMe-only, end to end.** `transport: nvme-tcp` live; volume → export → attach → `fs=ext4 mount=nvme-tcp1 block-device=true` verified against stormblockmk 0.4.1. The tool is `nvme-pvc` with a hand-written NVMe/TCP initiator (`tools/nvme-pvc/src/nvme.rs`: ICReq, fabrics Connect with in-capsule data, CC.EN + CSTS.RDY, Identify Namespace, Read/Write/Flush on a dedicated I/O queue) — datapath probe verified 64/64 stamped blocks across write, fresh session and after detach. mkube no longer formats at all: the registry writes and seals a template and every PVC is a CoW clone of it (a raw volume comes back `fs=-`), which also removed the post-format detach/re-attach. iSCSI kept ON PURPOSE for outward-facing use: `ISCSICdrom` (external virtual media) and `pvc_iscsi.go`/`ISCSIDisk` (non-rose consumers).
- [x] (2026-08-11) **Deploys stopped costing a five-minute outage.** Two paths recreated the mkube container — the update path (stop→remove→create) and the socket watchdog, which polls every 3s and read the deliberate removal as a crash. Both issued `/container/add`; the loser died on `root-dir overlap` and mkube stayed down for five minutes each deploy. `replaceContainer` now holds a swap mark and the watchdog skips a container mid-replacement; first deploy after the fix verified in <90s (previously always timed out). `hack/pull-and-deploy.sh` had the same class of bug in `wait_state()` and is fixed too.
- [x] (2026-08-11) **NVMe-TCP switchover complete — zero iSCSI on the PVC path.** The blocker recorded here for a day ("RouterOS 7.22.2 attaches NVMe but never mounts it") was **our bug**: stormblockmk publishes every export on its own port and `attachStormblockDisk` dropped the port for NVMe only, so the initiator went to RouterOS's default 4420 and asked there for an NQN that port never serves. It attached, then sat at `state=I/O error, block-device=false, read-ops=0` — which looks exactly like a missing block layer. With the port passed, `POST /api/v1/probes/nvmemount` mounts one volume over BOTH transports in a single run (`iscsi fs=ext4 mount=iscsi16`, `nvme-tcp fs=ext4 mount=nvme-tcp1 state=live`). `transport: nvme-tcp` is live. The tool is now `nvme-pvc` with a real NVMe/TCP initiator (`tools/nvme-pvc/src/nvme.rs`) and the iSCSI one is deleted; format/flush/reid no longer create a temporary iSCSI export to work around it.
- [x] (2026-08-10) **stormblock PVC end-to-end (v6.3.0)** — `storageClassName: stormblock` works: provision (Bearer token from Secret) → per-volume portal export → attach (port-split fix) → ext4 format (device block size from READ CAPACITY — was hardcoded 512 vs stormblockmk's 4096) → re-attach probe kick → RouterOS mount → pod Running with the volume. Idempotent reattach on recreate in <1s via annotations; rollback (disk+volume, force=true) on every failure path. Final blocker was `GetISCSIDisk` only searching file-backed disks so attached targets were invisible to the mount wait. Supersedes the old "End-to-end iSCSI PVC test" item (same chain, tested harder).
- [x] (2026-08-10) **DNS client health-poll alive gate** (`pkg/dns/client.go`, v6.2.1) — removed BeginBatch/EndBatch batch mode (record cache + `failedEndpoints` blacklist): client-global state raced by three concurrent batchers produced ~9,100 false "previously failed this batch" skips for healthy g8/gw endpoints and left those zones unregistered most cycles. Endpoint ops now gate on `endpointAlive` — a 15s-TTL-cached `GET /api/v1/health` probe (1.5s timeout); only up/down transitions are logged. microdns-side spec (health should reflect DB readability) filed in `microdns/enhancements/health-reflects-database.md`.
- [x] (2026-08-06) **g16 PXE + fleet renumbering** — bare-metal fleet moved from g10 onto the g16 flat /20 fabric, PXE chain verified end-to-end (server9 PXE→agent→inventory on g16). Address plan: hosts own 16.x–29.x (blades server1–8 a/b pairs at 16.100–115, all NICs actually 25G+ on dsw1; storage server9 R730xd at 16.120/121 — dual-port Mellanox CX-4 Lx 25G, MegaRAID SAS-3 3008, 13×1.1T), dynamic DHCP only 192.168.30.0/24, services at top of /20 in 31.x (mkube IPAM 31.10–199, pxe/bmh-operator 31.200, fastregistry 31.201, DNS 31.252). `BMHNICSpec.Hostname` override shipped (serverNb names); BMH specs own all reservations + DNS. Services moved off g10 (bmh-operator, fastregistry — TFTP now fabric-local). Console server for the fleet: `http://192.168.11.200` (ipmiserial).
- [x] mkube-update native API migration — `cmd/mkube-update/main.go` now uses `pkg/routeros.Client` directly. Removes the local rosGET/rosPost/rosCreateScript helpers and the dedicated REST HTTP client. Single TCP connection on port 8728 with auto-reconnect. Stops the last source of REST session pile-up on rose1 (mkube-update was the only remaining REST consumer after the mkube migration).
- [x] Native API migration — RouterOS client migrated from REST API to native binary protocol (port 8728) via `go-routeros/routeros/v3`. Eliminates REST session leak bug. Lazy connect with auto-reconnect. HTTP retained only for UploadFile.
- [x] Pod Worker + DNS recovery — serialized pod lifecycle queue, mount filter fix, DNS pods stable. 42 zombie REST sessions remain from pre-migration but no new ones created.
- [x] PVC mount preservation — `ReconcileMounts` never auto-deletes PVC-backed mounts, preventing data loss on container recreation.
- [x] Git-backed config state backup (`pkg/gitbackup/`) — rust4git State API, incremental pushes, debounce, DNS config snapshotter.
- [x] IPMI boot device control — `pkg/bmc/` package. Install images auto-set PXE boot, then switch to disk after DHCP lease detected.
- [x] Secret resource support — AES-256-GCM encrypted-at-rest in NATS. Volume mounts, env var injection, cluster sync, YAML export/import.
- [x] iSCSI-backed PVC provisioning — Rust prototype + Go integration (`pkg/provider/pvc_iscsi.go`).
- [x] Auto-repair DHCP relay NAT exemption — `ensureDHCPRelayNAT()` inserts `srcnat accept` before masquerade rules.
- [x] PXE boot fix — bmh-operator moved to g10 network where DHCP nextServer points.
- [x] Async PVC migration with SSE progress — MigrationTracker, phase-aware copy, console progress bar.
