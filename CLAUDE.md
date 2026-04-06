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
| gw | 192.168.1.252 | gw.lo (external, on pvex) |

## Key Patterns

- **Naming**: `{namespace}_{pod}_{container}` for RouterOS containers, `veth_{ns}_{pod}_{i}` for veths
- **Persistence**: All state persists in NATS JetStream KV
- **Reconcile**: 10s loop — desired state (NATS + boot-order) vs actual containers
- **Image updates**: `vkube.io/image-policy: auto` — digest check, rolling update on change
- **DNS**: Automatic registration via microdns REST API
- **RouterOS**: Use `remote-image` for container creation (NOT `tag`)
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
4. **Track external microdns instances**: gw DNS on pvex needs proper sync.
5. **Fix storage test failures**: `TestEnsureImageCacheHit` and `TestProvisionVolume`.
6. **TLS cert rotation**: API to update registry CA+server certs and trigger consumer reload.
7. **microdns resilience**: DNS containers must survive mkube failures independently.
8. **Registry HTTP/2 proper fix**: Find root cause of Go h2 GOAWAY or use reverse proxy.
9. ~~**configstate.gt.lo**: Git-backed config state backup for disaster recovery.~~ **DONE** — `pkg/gitbackup/` via rust4git State API.
10. **Proxmox integration test**: Smoke test `backend: proxmox` against pvex.gw.lo.
11. **Proxmox PVE 9.1+ native OCI**: Pass OCI ref directly to `pct create`.
12. **BMH scheduled power on/off**: Honor `bmh.mkube.io/power-on-days`, `power-on-time`, `power-off-days`, `power-off-time` annotations. Reconcile loop should auto-power-on/off hosts based on day-of-week + time-of-day schedule.

### Completed (recent)
- [x] IPMI boot device control — `pkg/bmc/` package with pure-Go IPMI client. Install images auto-set PXE boot, then switch to disk after DHCP lease detected. Prevents infinite reinstall loop.
- [x] Secret resource support — full CRUD API with AES-256-GCM encrypted-at-rest storage in NATS. Volume mounts, env var injection (Secrets + ConfigMaps), cluster sync, YAML export/import.
- [x] Fix `fixOrphanedVolumeMounts` cross-pod PVC contamination — hardcoded `{ns}-dns-data` for ALL orphaned data mounts, causing netwatch to get DNS pod's PVC. Now derives PVC name from pod name.
- [x] iSCSI-backed PVC provisioning — Rust prototype (tools/iscsi-pvc) + Go integration (pkg/provider/pvc_iscsi.go)
- [x] RouterOS client disk management methods (FindFileDiskByPath, SetISCSIExport, CreateFileDisk, RemoveFileDisk)

### In Progress
- [ ] (started 2026-03-25) End-to-end iSCSI PVC test — deploy a pod with `storageClassName: iscsi` PVC and verify data persistence

### Recently Completed
- [x] Micrologs circuit breaker — skip micrologs after 3 failures for 30s cooldown, 2s timeout persistent client
- [x] Async PVC migration with SSE progress — MigrationTracker, phase-aware copy, SSE streaming, console progress bar
- [x] Agent 24h container cleanup retention — stopped build containers and dangling images preserved for a day before pruning. Supports debugging/inspection.
- [x] Fix gitbackup on deferred NATS boot — deferred NATS path now initializes gitbackup after store connects
- [x] Fix deploy to bake device-specific config + deploy-config for volume-mounted config (`/etc/mkube/` is volume-mounted from device, overriding image-baked config)
- [x] Git-backed config state backup (`pkg/gitbackup/`) — rust4git State API, incremental pushes, debounce, store multi-hook, status/trigger API
- [x] DNS config snapshotter — debounced per-network microdns snapshots to git on every mutation (DHCP pools/reservations, DNS records/forwarders, network updates, BMH changes)
