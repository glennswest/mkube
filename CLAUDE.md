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
| gt | 192.168.200.199 | gt.lo |
| g10 | 192.168.10.252 | g10.lo |
| g11 | 192.168.11.252 | g11.lo |
| gw | 192.168.1.252 | gw.lo (mkube container `gw/dns`, microdns) |

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
- `pkg/storage/manager_test.go`: `TestEnsureImageCacheHit`, `TestProvisionVolume`

## Work Plan

### Current Version: `v6.0.0`

### TODO (priority order)
1. **BareMetalHost Operator (BMO)**: Full host state machine, serial proxy, Redfish, ownership model. Separate project repo. (IPMI power control now built into mkube via `pkg/bmc/`.)
2. **DNS 2-replica deployment**: Per zone via Deployment controller. Requires anti-affinity (multi-node).
3. **Registry push notifications to mkube-update**: Webhook/watch instead of polling.
4. **Fix storage test failures**: `TestEnsureImageCacheHit` and `TestProvisionVolume`.
5. **TLS cert rotation**: API to update registry CA+server certs and trigger consumer reload.
6. **microdns resilience**: DNS containers must survive mkube failures independently.
7. **Registry HTTP/2 proper fix**: Find root cause of Go h2 GOAWAY or use reverse proxy.
8. **Proxmox integration test**: Smoke test `backend: proxmox` against pvex.gw.lo.
9. **Proxmox PVE 9.1+ native OCI**: Pass OCI ref directly to `pct create`.
10. **BMH scheduled power on/off**: Honor `bmh.mkube.io/power-on-days`, `power-on-time`, `power-off-days`, `power-off-time` annotations. Reconcile loop should auto-power-on/off hosts based on day-of-week + time-of-day schedule.
11. **RouterOS native API reconnect race** (`pkg/routeros/client.go`): When the native API connection drops and the client auto-reconnects, the first request after reconnect returns "RouterOS API unreachable after reconnect" instead of succeeding or transparently retrying. Observed 2026-05-15 16:34Z: a transient API drop cascaded into 9 simultaneous CreateFailed events across infra/bmh-operator, g8/dns, gt/minio, infra/configman, gt/nats, infra/stormstar, infra/git, gw/dns, infra/cloudid. Reconnect should drain/retry the in-flight request rather than surface the transient error to callers.
12. **CreatePod must clean up partial root-dir on failure** (`pkg/provider/provider.go`): When veth/mount allocation or container/add fails mid-CreatePod, the tarball-extracted `/raid1/images/<name>` root-dir is left behind. The next reconcile retry hits RouterOS error `root-dir overlap with /raid1/images/<name>`, requiring manual cleanup or a fallback path. Observed 2026-05-15 16:38Z–16:40Z on g11/ipmiserial, gt/pvc-test, g9/dns, gt/dns immediately following the reconnect race in #11. Failure path should `RemoveDirectory(rootDir)` so retries are idempotent.
13. **Pluggable store backend interface** (`pkg/store/`): Abstract the persistence layer behind a `Backend` interface so NATS JetStream KV is one implementation among several. Goal: allow swapping in an etcd-compatible backend (e.g. an in-house Rust etcd) without touching call sites. Config key `store.backend: nats|etcd` selects implementation at startup; NATS remains the default. Motivation: stack consolidation onto in-house components and a path toward kubectl/k8s tool compatibility via a future kube-apiserver shim. Note: etcd alone does not yield kubectl compat — the apiserver translation layer is separate work and is NOT in scope for this TODO. Scope here is strictly the backend seam: define `Backend` interface (Get/Put/Delete/Watch/List/CAS), refactor existing NATS code to satisfy it, add config plumbing, and document the contract for future implementers.
14. **Switch default store backend to fastetcd**: Cutover from NATS JetStream KV to the in-house Rust etcd replacement (fastetcd) as the default `store.backend`. Depends on #13 (backend interface) and on fastetcd reaching production readiness (durability under power loss, crash recovery, multi-writer correctness, watch fan-out, lease semantics). Plan: ship fastetcd backend as opt-in alongside NATS; soak on one or two non-critical mkube instances; add a one-shot YAML export/import migration path so existing clusters can move their state without data loss; flip the default once parity is demonstrated. Keep NATS implementation in tree for at least one release after the flip as a fallback. Out of scope: removing NATS entirely (it's still used for embedded messaging beyond KV) — only the KV role moves.
15. **kube-apiserver compatibility shim**: Implement a kube-apiserver-shaped REST surface in front of mkube's store so `kubectl`, Helm, ArgoCD, and other k8s ecosystem tools can target mkube directly. Depends on #13 (backend interface) and ideally #14 (fastetcd default, since the shim's watch/MVCC/lease assumptions map cleanly onto etcd semantics). Scope: translate kube REST verbs (GET/LIST/WATCH/POST/PUT/PATCH/DELETE) and resourceVersion/MVCC semantics onto the `Backend` interface; expose `/api`, `/apis`, `/openapi/v2`, `/version` discovery endpoints; serve at least core/v1 (Pod, Service, ConfigMap, Secret, Namespace) and mkube's existing custom resources via CRD-style registration; TLS + client cert auth compatible with kubeconfig. Out of scope initially: full admission webhook chain, RBAC enforcement, audit logging. Document which kubectl subcommands are supported and which are not.

### In Progress
- [ ] (started 2026-03-25) End-to-end iSCSI PVC test — deploy a pod with `storageClassName: iscsi` PVC and verify data persistence

### Recently Completed
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
