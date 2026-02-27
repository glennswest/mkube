# CLAUDE.md — mkube Project

## Build & Deploy

```bash
# Build all binaries (mkube, mkube-update, mkube-registry, installer)
make build-all

# Build mkube only (ARM64 cross-compile)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube/

# Deploy to rose1
make deploy

# Bootstrap a fresh device
make deploy-installer

# Run tests
go test ./...
```

- Always use `podman`, not docker
- Container images use `scratch` base (no OS layer)
- Push to GHCR; registry watcher on device mirrors to local registry
- Local registry at `192.168.200.3:5000` (HTTPS with self-signed CA)

## Architecture

### Binaries
| Binary | Location | Runs on | Purpose |
|--------|----------|---------|---------|
| mkube | `cmd/mkube/` | RouterOS (ARM64) | Main controller |
| mkube-update | `cmd/mkube-update/` | RouterOS (ARM64) | Image update watcher |
| mkube-registry | `cmd/registry/` | RouterOS (ARM64) | Standalone OCI registry |
| installer | `cmd/installer/` | Mac (local) | One-shot bootstrap CLI |

### Key Packages
| Package | Purpose |
|---------|---------|
| `pkg/provider/` | Pod lifecycle, deployments, BMH, consistency, API routes |
| `pkg/network/` | Multi-network IPAM, veth/bridge management |
| `pkg/storage/` | OCI→tarball, volume provisioning, image cache |
| `pkg/store/` | NATS JetStream KV persistence, YAML import/export |
| `pkg/dns/` | microdns REST API client |
| `pkg/dzo/` | DNS Zone Orchestrator (cross-zone management) |
| `pkg/lifecycle/` | Boot ordering, health checks, watchdog |
| `pkg/registry/` | OCI registry implementation |
| `pkg/routeros/` | RouterOS REST API client |
| `pkg/runtime/` | Container runtime abstraction |

### Infrastructure
| Host | IP | Role |
|------|-----|------|
| rose1.gw.lo | 192.168.1.88 | MikroTik ARM64, runs mkube + all containers |
| pvex.gw.lo | 192.168.1.160 | Proxmox node, runs gw microdns (CT 117) |

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
| gw | 192.168.1.52 | gw.lo (external, on pvex) |

## Key Patterns

- **Naming**: `{namespace}_{pod}_{container}` for RouterOS containers, `veth_{ns}_{pod}_{i}` for veths
- **Persistence**: All pod/deployment/configmap state persists in NATS JetStream KV
- **Reconcile**: 10s loop — loads desired state from NATS + boot-order, compares with actual RouterOS containers
- **Staggered restarts**: Pods sharing the same image are restarted one at a time with liveness verification (DNS probes port 53)
- **Image updates**: `vkube.io/image-policy: auto` — reconciler checks registry digest, rolling update on change
- **DNS**: Automatic registration via microdns REST API; aliases, static records, DHCP reservations
- **RouterOS**: Use `remote-image` param for container creation (NOT `tag` — read-only metadata)
- **Scratch containers**: No system root CAs — can't make HTTPS to public registries. Use local registry.
- **kubectl/oc**: `kubectl` hangs against mkube API — use `curl` with JSON or `oc apply`

## Work Plan

### Resolved Issues
- Deployment controller with replica management, rolling updates, DNS round-robin
- DNS port 53 liveness probe and auto-repair
- Staggered container restarts (same-image pods restarted one at a time)
- Container start retry with backoff (MikroTik EOF race)
- Veth name collision fix (truncation 8→15 chars)
- IPAM collision fix (configurable ipamStart/ipamEnd per network)
- Registry standalone extraction with TLS
- Bootstrap via mkube-installer CLI
- Persistent mounts (root-readonly)
- Stale DNS cleanup
- Goroutine leak in consistency checker
- DHCP end-to-end (relay + microdns)
- PXE/UEFI boot support
- PVC support (persistent volumes surviving container recreation/redeploy)
- PVC/Deployment NATS load on boot (were only loaded in deferred path, vanished on every restart)
- Network CRD (cluster-scoped dynamic network definitions in NATS, TOML config generation, migration from config.yaml)
- BMH → Network CRD sync (DHCP reservations auto-synced from BMH to Network CRD on create/update/delete, both data and IPMI networks, per-reservation PXE fields in TOML)
- Cross-namespace BMH dedup (DHCP watcher checks boot MAC, BMC MAC, hostname across all namespaces before creating discovered entries)
- Per-server BMH consistency checks (data/IPMI network validation, reservation presence, duplicate detection, NATS sync)
- Network PATCH handler merge fix (DeepCopy + json.Unmarshal instead of replace)
- Auto-deploy microdns on managed Network CRD create (ConfigMap + pod auto-created, auto-teardown on delete, managed transitions on update/patch, backend-agnostic via ContainerRuntime)
- Registry CRD (cluster-scoped, NATS-backed, CRUD API, watch, table format, config generation, managed auto-deploy/teardown, migration from config.yaml, consistency checks, export/import)
- iSCSI CDROM CRD (cluster-scoped, NATS-backed, CRUD API, watch, table format, ISO upload, subscribe/unsubscribe, RouterOS iSCSI target/LUN auto-config, consistency checks, export/import)

### TODO (priority order)
1. **BareMetalHost Operator (BMO)**: Owns ALL host state and state machines. pxemanager becomes GUI-only (no SQLite state). Architecture:
   - BMH objects in NATS are the source of truth for host registrations, image assignments, MAC discovery, IPMI config
   - BMH is per physical server (one BMH per server, not per NIC/MAC)
   - BMH has explicit fields for data network (network, ip, hostname, PXE) and IPMI network (bmc.network, bmc.mac, bmc.hostname) ✅
   - BMH → Network CRD DHCP reservation sync (create/update/delete) ✅
   - Cross-namespace dedup prevents duplicate discovery ✅
   - Per-server consistency checks (network refs, reservations, duplicates) ✅
   - State transitions are BMH events (Registering → Provisioning → Ready, etc.)
   - IPMI power control + IPMI serial proxy
   - Watch API for real-time updates (pxemanager UI, CLI, other consumers subscribe)
   - Network automation hooks (future)
   - Virtual Redfish interface for hardware control
   - BMH ownership model — users can "build" clusters by requesting hardware
   - New project repo with subprojects, small scratch containers
2. **DNS 2-replica deployment**: Run 2 DNS replicas per zone using Deployment controller. Requires anti-affinity support (future multi-node).
3. **UI Dashboard**: Show containers, logs, memory, events, DNS. API already exposes most data.
4. **Registry push notifications to mkube-update**: Webhook/watch instead of polling.
5. **Track external microdns instances**: gw DNS on pvex needs proper sync.
6. **Fix storage test failures**: `TestEnsureImageCacheHit` and `TestProvisionVolume` in `pkg/storage/manager_test.go`.
7. **PXEManager tftpboot PV**: Move tftpboot to persistent volume (PVC support now available).

### In Progress
<!-- - [ ] (started YYYY-MM-DD) Task description -->

## Testing

```bash
# All tests
go test ./...

# Provider tests only
go test ./pkg/provider/...

# With verbose output
go test -v ./pkg/provider/...
```

Known test failures (pre-existing):
- `pkg/storage/manager_test.go`: `TestEnsureImageCacheHit` (read-only /cache), `TestProvisionVolume` (path mismatch)
