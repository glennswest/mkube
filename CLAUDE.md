# CLAUDE.md — mkube Project

## Build & Deploy

```bash
# Build all binaries (mkube, mkube-update, mkube-registry, installer, pve-deploy)
make build-all

# Build mkube only (ARM64 cross-compile)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube/

# Deploy to rose1
make deploy

# Bootstrap a fresh device (RouterOS)
make deploy-installer

# Proxmox LXC deployment
make build-pve-deploy                        # Build pve-deploy CLI
make deploy-pvex-registry                    # Deploy registry to CT 119
make deploy-pvex-mkube                       # Deploy mkube to CT 121
pve-deploy --config deploy/pvex-registry.yaml  # Direct CLI usage

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
| mkube | `cmd/mkube/` | RouterOS (ARM64), Proxmox (x86_64), StormBase | Main controller |
| mkube-update | `cmd/mkube-update/` | RouterOS (ARM64) | Image update watcher |
| mkube-registry | `cmd/registry/` | RouterOS (ARM64) | Standalone OCI registry |
| installer | `cmd/installer/` | Mac (local) | One-shot RouterOS bootstrap CLI |
| pve-deploy | `cmd/pve-deploy/` | Mac (local) | Deploy OCI images as Proxmox LXC containers |
| mkube-boot | `cmd/mkube-boot/` | Proxmox LXC (x86_64) | Bootstrap mkube infrastructure on Proxmox |
| mkube-agent | `cmd/mkube-agent/` | CoreOS (x86_64) | Job execution agent for bare metal hosts |

### Key Packages
| Package | Purpose |
|---------|---------|
| `pkg/console/` | Built-in web dashboard UI (stormd extension, Dracula theme) |
| `pkg/provider/` | Pod lifecycle, deployments, BMH, consistency, API routes |
| `pkg/network/` | Multi-network IPAM, veth/bridge management |
| `pkg/storage/` | OCI→tarball, volume provisioning, image cache |
| `pkg/store/` | NATS JetStream KV persistence, YAML import/export |
| `pkg/dns/` | microdns REST API client |
| `pkg/dzo/` | DNS Zone Orchestrator (cross-zone management) |
| `pkg/lifecycle/` | Boot ordering, health checks, watchdog |
| `pkg/registry/` | OCI registry implementation |
| `pkg/routeros/` | RouterOS REST API client |
| `pkg/proxmox/` | Proxmox VE REST API client, VMID allocator, OCI→LXC template converter |
| `pkg/pvectl/` | Proxmox LXC deploy library (OCI binary extraction, SSH/pct helpers, systemd install) |
| `pkg/stormbase/` | StormBase gRPC client |
| `pkg/runtime/` | Container runtime abstraction (RouterOS, StormBase, Proxmox adapters) |
| `pkg/nats/` | Embedded NATS server (in-process JetStream) |
| `pkg/cluster/` | Multi-node clustering (peer health, push sync, full resync) |
| `pkg/diskimg/` | Pure Go disk image converters (VMDK, QCOW2, VHD → raw) |

### Backends
| Backend | Config key | Runtime adapter | Network driver | Init function |
|---------|-----------|-----------------|----------------|---------------|
| RouterOS | `backend: routeros` (default) | `pkg/runtime/routeros.go` | `pkg/network/driver/routeros.go` | `runRouterOS()` |
| StormBase | `backend: stormbase` | `pkg/stormbase/client.go` | `pkg/network/driver/stormbase.go` | `runStormBase()` |
| Proxmox | `backend: proxmox` | `pkg/runtime/proxmox.go` | `pkg/network/driver/proxmox.go` | `runProxmox()` |

### Infrastructure
| Host | IP | Role |
|------|-----|------|
| rose1.gw.lo | 192.168.1.88 | MikroTik ARM64, runs mkube + all containers (RouterOS backend) |
| pvex.gw.lo | 192.168.1.160 | Proxmox node, runs gw microdns (CT 117), Proxmox backend target |

### Proxmox LXC Allocation (pvex, gw network)
| VMID | Hostname | IP | Purpose |
|------|----------|----|---------|
| 117 | dns-gw | 192.168.1.52 | gw microdns (existing) |
| 119 | registry | 192.168.1.161 | OCI registry |
| 120 | mkube-boot | 192.168.1.162 | Bootstrap container |
| 121 | mkube | 192.168.1.163 | mkube controller |

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
- **Reconcile**: 10s loop — loads desired state from NATS + boot-order, compares with actual containers
- **Staggered restarts**: Pods sharing the same image are restarted one at a time with liveness verification (DNS probes port 53)
- **Image updates**: `vkube.io/image-policy: auto` — reconciler checks registry digest, rolling update on change
- **DNS**: Automatic registration via microdns REST API; aliases, static records, DHCP reservations
- **RouterOS**: Use `remote-image` param for container creation (NOT `tag` — read-only metadata)
- **Proxmox**: LXC containers via REST API, PVE API token auth, VMID range allocation (e.g. 200-299), mounts accumulated and applied at creation time, OCI→rootfs .tar.gz conversion for vztmpl upload, async task polling for all mutating ops
- **Scratch containers**: No system root CAs — can't make HTTPS to public registries. Use local registry.
- **kubectl/oc**: `kubectl` hangs against mkube API — use `curl` with JSON or `oc apply`

## mkube REST API Reference

Base URL: `http://192.168.200.2:8082`

### Pods (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/pods` | List all pods |
| `GET` | `/api/v1/namespaces/{ns}/pods` | List pods in namespace |
| `GET` | `/api/v1/namespaces/{ns}/pods/{name}` | Get pod |
| `GET` | `/api/v1/namespaces/{ns}/pods/{name}/status` | Get pod status |
| `POST` | `/api/v1/namespaces/{ns}/pods` | Create pod |
| `PUT` | `/api/v1/namespaces/{ns}/pods/{name}` | Update pod |
| `PATCH` | `/api/v1/namespaces/{ns}/pods/{name}` | Patch pod |
| `DELETE` | `/api/v1/namespaces/{ns}/pods/{name}` | Delete pod |
| `POST` | `/api/v1/namespaces/{ns}/pods/{name}/migrate` | Migrate pod to another node |
| `GET` | `/api/v1/namespaces/{ns}/pods/{name}/log` | Get pod logs |

### Deployments (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/deployments` | List all deployments |
| `GET` | `/api/v1/namespaces/{ns}/deployments` | List deployments in namespace |
| `GET` | `/api/v1/namespaces/{ns}/deployments/{name}` | Get deployment |
| `POST` | `/api/v1/namespaces/{ns}/deployments` | Create deployment |
| `PUT` | `/api/v1/namespaces/{ns}/deployments/{name}` | Update deployment |
| `DELETE` | `/api/v1/namespaces/{ns}/deployments/{name}` | Delete deployment |

### ConfigMaps (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/namespaces/{ns}/configmaps` | List configmaps |
| `GET` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Get configmap |
| `POST` | `/api/v1/namespaces/{ns}/configmaps` | Create configmap |
| `PUT` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Update configmap |
| `PATCH` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Patch configmap |
| `DELETE` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Delete configmap |

### PersistentVolumeClaims (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/persistentvolumeclaims` | List all PVCs |
| `GET` | `/api/v1/namespaces/{ns}/persistentvolumeclaims` | List PVCs in namespace |
| `GET` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Get PVC |
| `POST` | `/api/v1/namespaces/{ns}/persistentvolumeclaims` | Create PVC |
| `PUT` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Update PVC |
| `DELETE` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Delete PVC |

### Networks (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/networks` | List all networks |
| `GET` | `/api/v1/networks/{name}` | Get network (status enriched with pod count + DNS liveness) |
| `POST` | `/api/v1/networks` | Create network (auto-provisions infra on managed backends) |
| `PUT` | `/api/v1/networks/{name}` | Replace network |
| `PATCH` | `/api/v1/networks/{name}` | Merge-patch network |
| `DELETE` | `/api/v1/networks/{name}` | Delete network (409 if pods reference it) |
| `GET` | `/api/v1/networks/{name}/config` | Generated microdns TOML config |
| `POST` | `/api/v1/networks/{name}/smoketest` | On-demand DNS/DHCP smoke test |

**Network types**: `data`, `ipmi`, `management`, `boot`, `storage`, `user`, `external`

**NetworkSpec fields**: `type`, `pairNetwork` (companion data↔IPMI link), `bridge`, `cidr`, `gateway`, `vlan`, `router`, `dns`, `dhcp`, `ipam`, `externalDNS`, `managed`, `provisioned`, `staticRecords`

### Registries (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/registries` | List all registries |
| `GET` | `/api/v1/registries/{name}` | Get registry |
| `POST` | `/api/v1/registries` | Create registry |
| `PUT` | `/api/v1/registries/{name}` | Update registry |
| `PATCH` | `/api/v1/registries/{name}` | Patch registry |
| `DELETE` | `/api/v1/registries/{name}` | Delete registry |
| `GET` | `/api/v1/registries/{name}/config` | Generated registry config |

### iSCSI CDROMs (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/iscsi-cdroms` | List all iSCSI CDROMs |
| `GET` | `/api/v1/iscsi-cdroms/{name}` | Get iSCSI CDROM |
| `POST` | `/api/v1/iscsi-cdroms` | Create iSCSI CDROM |
| `PUT` | `/api/v1/iscsi-cdroms/{name}` | Update iSCSI CDROM |
| `PATCH` | `/api/v1/iscsi-cdroms/{name}` | Patch iSCSI CDROM |
| `DELETE` | `/api/v1/iscsi-cdroms/{name}` | Delete iSCSI CDROM |
| `POST` | `/api/v1/iscsi-cdroms/{name}/upload` | Upload ISO |
| `POST` | `/api/v1/iscsi-cdroms/{name}/subscribe` | Subscribe host to CDROM |
| `POST` | `/api/v1/iscsi-cdroms/{name}/unsubscribe` | Unsubscribe host |
| `GET` | `/api/v1/iscsi-cdroms/{name}/files` | Read file from ISO |
| `POST` | `/api/v1/iscsi-cdroms/{name}/derive` | Derive new CDROM from existing |

### iSCSI Disks (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/iscsi-disks` | List all iSCSI disks |
| `GET` | `/api/v1/iscsi-disks/{name}` | Get iSCSI disk |
| `POST` | `/api/v1/iscsi-disks` | Create iSCSI disk (clones from source) |
| `PATCH` | `/api/v1/iscsi-disks/{name}` | Patch iSCSI disk (host, description, grow size) |
| `DELETE` | `/api/v1/iscsi-disks/{name}` | Delete iSCSI disk |
| `POST` | `/api/v1/iscsi-disks/{name}/clone` | Clone disk to new instance |
| `POST` | `/api/v1/iscsi-disks/{name}/resize` | Grow disk (grow only) |
| `GET` | `/api/v1/iscsi-disks/capacity` | Disk usage stats |

### BootConfigs (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/bootconfigs` | List all boot configs |
| `GET` | `/api/v1/bootconfigs/{name}` | Get boot config |
| `POST` | `/api/v1/bootconfigs` | Create boot config |
| `PUT` | `/api/v1/bootconfigs/{name}` | Update boot config |
| `PATCH` | `/api/v1/bootconfigs/{name}` | Patch boot config |
| `DELETE` | `/api/v1/bootconfigs/{name}` | Delete boot config |
| `GET` | `/api/v1/bootconfigs/{name}/serve` | Serve rendered config to booting host |
| `GET` | `/api/v1/bootconfigs/{name}/files/{filename}` | Serve attached file |
| `POST` | `/api/v1/bootconfigs/{name}/files/{filename}` | Upload file attachment |
| `GET` | `/api/v1/bootconfig` | Lookup boot config by source IP → BMH → bootConfigRef |
| `POST` | `/api/v1/boot-complete` | Signal boot completion (source IP auth) |

### BareMetalHosts (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/baremetalhosts` | List all BMHs |
| `GET` | `/api/v1/namespaces/{ns}/baremetalhosts` | List BMHs in namespace |
| `GET` | `/api/v1/namespaces/{ns}/baremetalhosts/{name}` | Get BMH |
| `POST` | `/api/v1/namespaces/{ns}/baremetalhosts` | Create BMH |
| `PUT` | `/api/v1/namespaces/{ns}/baremetalhosts/{name}` | Update BMH |
| `PATCH` | `/api/v1/namespaces/{ns}/baremetalhosts/{name}` | Patch BMH (power on/off via `spec.online`) |
| `DELETE` | `/api/v1/namespaces/{ns}/baremetalhosts/{name}` | Delete BMH |
| `POST` | `/api/v1/namespaces/{ns}/baremetalhosts/{name}/refresh` | Refresh single BMH |
| `POST` | `/api/v1/baremetalhosts/refresh` | Refresh all BMHs |

### DNS/DHCP Proxy Resources (namespace = network name, proxied to microdns)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dnsrecords[/{name}]` | DNS records (dr) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dhcppools[/{name}]` | DHCP pools (dp) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dhcpreservations[/{name}]` | DHCP reservations (dhcpr) |
| `GET` | `/api/v1/namespaces/{net}/dhcpleases` | DHCP leases (dl) |
| `GET/POST/DELETE` | `/api/v1/namespaces/{net}/dnsforwarders[/{name}]` | DNS forwarders (df) |

### StoragePools (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/storagepools[/{name}]` | Storage pools (sp) |
| `POST` | `/api/v1/storagepools/{name}/migrate` | Migrate PVC/disk to pool |

### Job Scheduling

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{ns}/hostreservations[/{name}]` | Host reservations |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/jobrunners[/{name}]` | Job runners (cluster-scoped) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{ns}/jobs[/{name}]` | Jobs |
| `POST` | `/api/v1/namespaces/{ns}/jobs/{name}/cancel` | Cancel job |
| `GET` | `/api/v1/namespaces/{ns}/jobs/{name}/logs` | Get job logs |
| `GET` | `/api/v1/jobqueue` | Computed job queue view |

### Agent Endpoints (source-IP authenticated)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/agent/work` | Pull work assignment |
| `POST` | `/api/v1/agent/heartbeat` | Agent heartbeat |
| `POST` | `/api/v1/agent/logs` | Stream job logs |
| `POST` | `/api/v1/agent/complete` | Signal job completion |

### Cluster & Operations

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/nodes` | List cluster nodes |
| `GET` | `/api/v1/nodes/{name}` | Get node details |
| `GET` | `/api/v1/events` | List events (cluster or namespaced) |
| `GET` | `/api/v1/export` | Export all state |
| `POST` | `/api/v1/import` | Import state |
| `GET` | `/api/v1/consistency` | Run consistency check |
| `POST` | `/api/v1/consistency/repair` | Repair consistency issues |
| `GET` | `/api/v1/dns/validate` | Validate DNS records |
| `POST` | `/api/v1/registry/push-notify` | Registry push webhook |
| `GET` | `/api/v1/images` | List cached images |
| `POST` | `/api/v1/images/redeploy` | Force image redeploy |
| `GET` | `/api/v1/lifecycle/stats` | Lifecycle phase timing |
| `GET` | `/healthz` | Health check (includes version + commit) |
| `GET` | `/api`, `/api/v1`, `/apis`, `/version` | API discovery (kubectl compat) |

### `mk` CLI Shorthand

All resources are accessible via `mk` (alias for `KUBECONFIG=~/.kube/mkube.config oc`):

```bash
mk get networks                    # List networks with type column
mk get networks g10 -o yaml        # Full network spec
mk get pods -A                     # All pods, all namespaces
mk get bmh -A                      # All BareMetalHosts
mk get dr -n g10                   # DNS records for g10
mk get dp -n g10                   # DHCP pools for g10
mk get dhcpr -n g10                # DHCP reservations for g10
mk get dl -n g10                   # DHCP leases for g10
mk get df -n g10                   # DNS forwarders for g10
mk get pvc -A                      # All PVCs
mk get deployments -A              # All deployments
mk get bootconfigs                 # Boot configs
mk get iscsi-cdroms                # iSCSI CDROMs
mk get iscsi-disks                 # iSCSI Disks (idisk)
mk get jobrunners                  # Job runners
mk get jobs -A                     # All jobs
mk get hostreservations -A         # All host reservations
```

## Work Plan

### Resolved Issues
- Consistency check lock contention: GET /api/v1/consistency held middleware RLock for 57 seconds. Any pending writer (scheduler, CRUD) blocked ALL new readers (Go RWMutex fairness) → entire API unresponsive. Fix: exempt from middleware, cached background report with per-check brief RLock/RUnlock, 120s auto-refresh timer.
- Console UI performance: SPA navigation, cacheable static assets, batch DOM writes, debounced inputs, API data prefetch cache (15s TTL), storage PVC pool column + move button, pool disk/PVC names.
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
- iSCSI CDROM CRD (cluster-scoped, NATS-backed, CRUD API, watch, table format, ISO upload via streaming, subscribe/unsubscribe, ROSE /disk API for iSCSI export, consistency checks, export/import). In production — routine bare-metal boots via iSCSI ISO.
- mkube-update tarball format fix (crane.Save produced OCI format with compressed layers; RouterOS needs docker-save v1 with uncompressed layers, repositories file, VERSION/json per layer). Verified: mkube-update successfully replaces mkube via registry push.
- mkube-update scratch container fix (/tmp doesn't exist, bootstrap retry on API unreachable)
- mkube-update GHCR fallback (tries local registry first, falls back to ghcr.io/glennswest/ if local pull fails)
- Registry TLS cert mismatch (installer regenerated CA+server certs but registry wasn't restarted — stale cert served)
- gw DNS address fix (192.168.1.199 → 192.168.1.52, CT 117 on pvex)
- BootConfig CRD (cluster-scoped, NATS-backed, CRUD API, watch, table format, source IP lookup, BMH bootConfigRef linkage with assignedTo sync, delete protection, consistency checks). Serves ignition/cloud-init/kickstart configs to booting servers via source IP → BMH → BootConfig lookup.
- Registry HTTP/2 GOAWAY crash fix (Go h2 sends GOAWAY during large blob uploads, killing registry. Disabled h2 via TLSNextProto, forced HTTP/1.1)
- Registry panic recovery middleware (recover() on all handlers, log stack trace, return 500 instead of crashing)
- Infrastructure health check (mkube checks registry :5001/healthz every reconcile, restarts after 3 failures. Catches zombie state where RouterOS says RUNNING but process is dead)
- mkube-update poll interval 60s → 15s (code default + installer template + live config)
- Zero-downtime blue-green container updates: UpdatePod pre-extracts new image in staging container while old serves traffic. Fast cutover (~5-8s) uses pre-extracted root-dir (RouterOS skips extraction). Alternating root-dir pattern. Staging health check with fallback to destructive update.
- Stale root-dir on container update: RemoveFile failed silently on non-empty rootfs directories → RouterOS skipped tarball extraction and used old content. Fixed with RemoveDirectory (recursive removal on all backends). Root-dir path normalization mismatch also fixed (leading `/` vs no leading `/` broke staging alternation). Multi-pod stale detection fixed (RefreshImage called once per unique image, result propagated to all pods).
- Proxmox VE LXC backend: Third backend provider. REST API client with PVE API token auth, async task polling (UPID), VMID range allocator, OCI→rootfs template converter, ContainerRuntime adapter with mount accumulator, NetworkDriver for pre-existing bridges, discovery module. Config: `backend: proxmox`. Deploy config: `deploy/proxmox-config.yaml`. 17 tests.
- ISCSICdrom `version` field: Tracks ISO version in spec, shown in table output, inherited on derive.
- Multi-node cluster architecture: Embedded NATS, peer sync, node-scoped reconciliation, multi-node deployment scheduling. Packages: `pkg/nats/`, `pkg/cluster/`. rose1↔pvex cluster with independent NATS and HTTP sync.
- Node tracking and pod migration: All pods auto-stamped with `vkube.io/node` annotation. Pod table shows NODE column. Node list shows all cluster nodes with architecture. Pod migration API (`POST .../migrate`) with architecture mismatch validation. Stale container cleanup for migrated pods.
- DNS ConfigMap reconcile fix: `generateDefaultConfigMaps` from static config (`rose1-config.yaml`) was overwriting Network CRD-derived ConfigMaps every 10s reconcile cycle. Fixed by overriding static ConfigMaps with Network CRD-generated TOML for migrated networks. Also added boot-time `ReconcileNetworkConfigMaps` to fix stale ConfigMaps on startup.
- server30 → server9 migration: Updated `deploy/rose1-config.yaml` (g10: .30→.18 with server9 hostname, removed server30b, g11: hostname change). Cleaned stale DNS A records from microdns redb database.
- Direct microdns REST API for DHCP/forwarders: mkube now calls microdns REST API directly for DHCP pool creation, reservation upsert/delete, and DNS forward zone management. Replaces the old TOML pipeline. TOML reduced to minimal structural config. Network CRD keeps reservations as NATS desired-state backup. `seedDNSConfig()` re-seeds on empty DB. BMH sync pushes changes via REST API immediately.
- microdns liveness checks: `checkMicroDNSServices()` in consistency report verifies REST API health, DHCP pool/reservation counts, and DNS forwarder topology. `repairDNSLiveness()` triggers `seedDNSConfig()` after restart for auto-recovery. `checkInfraHealth()` polling fallback detects zombie microdns containers.
- pxemanager code removed from mkube: All pxemanager client code (pxeHost type, pxeHTTPClient, pxeRegisterHost, pxeSetImage, pxeIPMIPower, pxeIPMIStatus, pxeConfigureIPMI, pxeGetHost), reconcileBMHChanges, enrichBMHStatus, enrichBMHListConcurrent — all removed. bmh-operator handles IPMI directly. Eliminates stale error messages from decommissioned pxemanager.
- Network provisioning flow: Creating a Network CRD auto-provisions infrastructure on RouterOS (bridge, gateway IP, DHCP relay). Deprovisioning on delete tears down in reverse. RouterOS client extended with bridge/IP/DHCP-relay CRUD. Network driver implements CreateBridge/DeleteBridge. `network_provision.go` handles the lifecycle.
- Network CRD pairNetwork: Links companion data/IPMI networks (e.g. g12↔g13). Config `NetworkDef.Type` carries type classification through migration.
- NATS URL for microdns: Embedded NATS sets URL to 0.0.0.0 (unreachable from other containers). Fixed to use NATS container IP.
- DNS data persistence: Managed DNS pods now get PVC-backed data volume for redb persistence.
- NATS messaging in microdns TOML: Generated TOML includes `[messaging]` section enabling microdns → NATS DHCP event pipeline.
- dhcpIndex rebuild from CRDs: `rebuildDHCPIndex()` builds from both static config and Network CRDs. DHCP watcher polls CRD-based networks. Wired into CRD handlers and SetStore boot path.
- Dynamic IPAM registration for Network CRDs: Networks created via API were never registered with IPAM allocator, causing DNS pod creation to fail. Added `Manager.RegisterNetwork()` method. Wired into `handleCreateNetwork` and `LoadNetworksFromStore`.
- PVC key consistency for managed DNS: PVCs created by `deployManagedDNS` used wrong key format (no namespace), causing consistency check failures.
- Bridge rename on rose1: `bridge` → `bridge-g10`, `bridge-boot` → `bridge-g11`, deleted unused `containers` bridge. All references (IPs, relays, DHCP server) auto-updated by RouterOS internal ID refs.
- g8/g9 full network setup: Created managed Network CRDs with DNS, DHCP, forward zones. Replaced standalone zero-zone DNS containers with mkube-managed pods. All 6 networks operational.
- DNS/DHCP proxy resources via kube API: All microdns resources accessible via `mk get/apply/delete`. 5 resource types: DNSRecord (dr), DHCPPool (dp), DHCPReservation (dhcpr), DHCPLease (dl), DNSForwarder (df). Namespace = network name. microdns is source of truth (no NATS). 24 handler functions, 5 table formatters. DNS client extended with full record support and lease listing.
- iSCSI root_path for DHCP: Pool-level default root_path (baremetalservices iSCSI target) auto-set for data networks. Per-reservation root_path from BMH spec overrides pool default. Flows through dns_seed.go, dns_proxy.go, Network CRD, and BMH sync.
- Config sync in `make deploy`: `make deploy` previously only pushed the binary (container image) — config files on the device were never updated. Added `deploy-config` target that SCPs `rose1-config.yaml` + `boot-order.yaml` to the device. `make deploy` now depends on `deploy-config`. This was the root cause of g10/g11 DNS outage after bridge rename: device had stale config with old bridge names (`bridge` instead of `bridge-g10`).
- Deploy version verification: healthz now reports version and commit hash. `make deploy` waits up to 90s after push for mkube-update to swap in the new binary, then verifies the running commit matches the build. Prevents silently running stale code after deploy.
- Auto-recovery for stopped/faulted containers: Reconciler detects stopped containers with start-on-boot=yes, attempts restart first (cheapest fix), falls back to full destroy+recreate if restart fails. RouterOS `comment` field propagated through the stack for diagnostics. Events: ContainerStopped, Restarted, RecoveryRecreate. Lifecycle phase timing stats via `GET /api/v1/lifecycle/stats`.
- Live integration test CLI: `cmd/mkube-test` runs against live mkube API with dedicated test network. 4 suites: container start/stop, DHCP reservation CRUD, DNS record CRUD, DHCP pool CRUD. Reports timing stats.
- Job scheduling system: 3 new CRDs (HostReservation namespaced, JobRunner cluster-scoped, Job namespaced) + JobQueue computed view + scheduler goroutine (10s tick) + mkube-agent binary. Full CRUD/PATCH/Watch/table/consistency/export-import. Agent endpoints: source-IP authenticated work pull, heartbeat, log streaming, completion. Scheduler: priority scheduling, provisioning/running/heartbeat timeouts, idle runner power-off. NATS JOBLOGS bucket with 7-day TTL. In-memory log ring buffer (10k lines).
- PVC volume data loss prevention: boot-order.yaml DNS pods had volumeMounts for "data" but no PVC Volume definition → ephemeral volume on recreation → redb database wiped. Fixed: (1) boot-order.yaml now includes PVC volume defs for all DNS pods, (2) resolvePVCVolume auto-creates missing PVCs when pod spec references them, (3) loadFromStore migration fixes orphaned volumeMounts in stored pods.
- microdns REST/recursor race fix: Records added via REST API were invisible to DNS resolver until restart. Two-part fix: (1) Resolver checks local authoritative zones FIRST before forward table. (2) REST API mutations clear recursor in-memory DnsCache via shared Arc. Verified: REST record creation → immediate DNS resolution.
- microdns DHCP pool allocator DB-only fix: `Dhcpv4Server::new()` built pools from TOML config (always empty — mkube pushes pools via REST to DB). Fixed: new() loads pools from DB via `list_dhcp_pools()`. sync_pool() rebuilds full pool list from DB every 60s. Removed `from_db()` constructor, TOML reservations, config fallback. `/dhcp/status` endpoint also reads from DB. Verified: server7 gets DHCP lease on g10.
- Job CRDs lost on restart: LoadHostReservationsFromStore, LoadJobRunnersFromStore, LoadJobsFromStore were missing from the immediate NATS boot path in main.go (lines 507-518). Only SetStore (deferred path) had them, but NATS usually connects immediately so deferred path never runs. Data persisted to NATS correctly but was never loaded into memory on boot.
- Idempotent veth/bridge-port creation: CreateVeth checks for existing veth (no-op if matching, update-in-place if different). AddBridgePort checks for existing port (no-op if correct bridge, remove+re-add if wrong). Prevents CreateFailed loop from orphaned veths.
- IPAM static allocation idempotency: AllocateStatic returns nil when the same key already holds the requested IP. Fixes g10 DNS pod stuck in CreateFailed loop — orphaned veth kept IPAM allocation alive, blocking pod recreation with its own IP.
- MicroDNS smoke test: Automated end-to-end validation after every seedDNSConfig. Checks DHCP pools/reservations via REST, creates canary DNS A record (_smoketest), verifies resolution on port 53 with UDP answer parsing, cleans up. Results in sync.Map, exposed in consistency report. On-demand API: POST /api/v1/networks/{name}/smoketest.
- Aggressive DNS failure detection: checkInfraHealth tracks consecutive failures per network. After 3 failures (30s), forces pod recreation. Immediate repairDNSLiveness on first failure instead of waiting for consistency check cycle.
- Auto-generated liveness probes from container ports: extractProbes() auto-generates TCP liveness (30s period, 3 failures) and readiness (10s period) probes from declared containerPorts when no explicit probes exist. Catches dead processes inside "running" containers via lifecycle watchdog. Consistency checker `podLiveness` section probes all ports. `checkInfraHealth` now monitors ALL pods with TCP ports (not just DNS), auto-restarts after 3 consecutive failures.
- PVC survivability and disk health verification: EnsureDirectory/FileExists/ListDirectory added to ContainerRuntime interface (RouterOS via /file API, Proxmox/StormBase via os.*). PVC directories auto-created on resolve and explicit creation. Consistency checker verifies PVC directory existence on disk, auto-creates missing (self-healing), warns on empty directories for active pods. Prevents silent data loss from broken PVC mounts.
- DHCP reservation self-containment: Reservations were missing gateway/DNS/domain fields. Servers with reserved IPs outside pool range had no default route, causing boot loops (server8 looped 7.5h). Root cause: `NetworkDHCPReservation` and `dns.DHCPReservation` structs had no gateway/dns_servers/domain fields. `upsertNetworkReservation` never populated them. Fixed: added Gateway/DNSServers/Domain to both structs, `upsertNetworkReservation` now populates from Network CRD defaults (gateway, DNS server, zone). All BMH reservations now carry full network context.
- Staging veth/IPAM leak in blue-green updates: `stagingExtractAndVerify` leaked staging veth when late-stage steps failed (extraction timeout, start/verify failure). RecoveryRecreate flow didn't release production veths or clean root-dirs, causing infinite crash loops ("IP already allocated to __stg", "root-dir overlap"). Fixed: (1) defer cleanup in staging, (2) RecoveryRecreate releases specific container's veth + staging leftovers + root-dir, (3) CreatePod detects "__stg" in allocation errors and cleans leaked staging veths.
- ISCSIDisk CRD (IMPLEMENTED): Cluster-scoped CRD for per-instance read/write iSCSI block devices. Creates sparse disk files under `/raid1/disks/`, clones from ISCSICdrom (ISO→disk) or existing ISCSIDisk sources. Background cloning with phase tracking (Pending→Cloning→Ready). RouterOS iSCSI target auto-registration with `disk-` prefix. Full CRUD + clone + resize + capacity API. BMH `spec.disk` field for iSCSI root disk boot (takes precedence over `spec.image`). Watch, table format, consistency checks, export/import. Reconciler recreates missing targets after RouterOS reboot.
- Pure Go disk image library (`pkg/diskimg`): Replaces `qemu-img`, `gzip`, and `cp --sparse=auto` external tool dependencies with pure Go. VMDK reader (streamOptimized with grain markers + deflate, monolithicSparse with GD/GT tables, monolithicFlat descriptors), QCOW2 reader (v2/v3, L1/L2 tables, compressed clusters with deflate/zlib), VHD reader (fixed, dynamic with BAT). Sparse-aware file copy. Gzip decompression via `compress/gzip`. 8 unit tests. Required because mkube runs in a scratch container where no external tools exist.
- Stale binary after image push: Three root causes. (1) `RefreshImage` first-check-after-restart always returned `changed=false` — session memory was empty, `wasChanged = storedDigest != "" && ...` was always false. Fixed with `RefreshImageWithHint()` + `vkube.io/image-digest` pod annotation persisted to NATS. Boot reconcile passes stored digest as hint. (2) No deployed digest tracking — added `stampImageDigest` on CreatePod/UpdatePod. (3) mkube-update used simple `/file/remove` on root-dir — fails silently on non-empty dirs, RouterOS skips extraction, reuses old binary. Replaced with recursive `rosRemoveDirectory()`.
- Event-driven architecture: Replaced fixed-interval polling with event-driven reactions across the entire stack. Kick channels for reconciler/scheduler triggered by CRUD handlers. NATS resource watchers for BMH/Deployment/Network/Job/JobRunner. Registry SSE push events for mkube-update (instant reaction instead of 15s poll). Lifecycle `OnStateChanged` callbacks fire on every container state transition, pushing immediate pod status updates and kicking reconcile. NotifyPods fallback polling relaxed from 5s to 30s. Container state polling tightened (waitForStopped 2s→1s, waitForRunning 1s→500ms).
- mkube-update post-replace health verification + rollback: After replacing a container, monitors for 15s to detect startup crashes. On failure: 3 restart retries, then automatic rollback to the previous image tarball (preserved via `-prev.tar` rename). Both new and rollback images get health verification with retry. Digest reverted on rollback so next poll retries.
- Build container job model: mkube-agent rewritten from inline-script-in-agent to build-container model. Jobs specify `repo` (git URL), `buildScript` (script name in repo), `buildImage` (container image — `fedoradev:latest` or `rawhidedev:latest`). Agent uses podman to pull image, runs disposable container that clones repo and executes build script, streams logs, disposes container. Build images: `registry.gt.lo:5000/fedoradev:latest` (stable), `registry.gt.lo:5000/rawhidedev:latest` (rawhide). Legacy inline script mode preserved.
- Container veth binding crash loop fix: `stopAndRemoveContainer` only tried `RemoveContainer` once. If RouterOS rejected removal (container still stopping), the old container kept holding the veth. `CreateContainer` with the same veth failed with 400 error, causing crash loops (g10/dns: 19 crashes). Fixed: retry removal up to 7 times with progressive backoff, re-issue stop on "running" errors. `cutoverContainer` and auto-recovery now check removal result and force-release veth if container can't be removed.
- RWMutex deadlock fix: `RunJobScheduler` held `p.mu.Lock()` for entire `schedulerTick()` (NATS writes + BMH updates). API handlers held `p.mu.RLock()` during `enrichPod` → `GetPodStatus` → RouterOS HTTP calls. Go's RWMutex blocks new readers when writer pending → `/healthz` blocked → stormd kills mkube. Two fixes: (1) `/healthz`, `/version`, `/apis` exempted from global mutex (they read immutable startup data). (2) Scheduler deferred-write pattern — in-memory mutations under lock, NATS writes flushed outside lock via `schedulerDeferred`.
- DHCP domain_search all zones: `seedDHCPPool` was setting `domain_search` to only the local zone. Now builds from all Network CRDs — local zone first, then peers sorted alphabetically. Auto-reseed detects stale domain_search count and corrects existing pools.
- Forward zones baked into microdns TOML: `generateMinimalTOML` now includes forward zones for all peer networks, eliminating the REST API seeding gap where cross-zone DNS queries returned NXDOMAIN (cached by systemd-resolved for 300s).
- TOML dotted key fix: Zone names containing dots (e.g. `g8.lo`) must be quoted in TOML (`"g8.lo"`) or they're parsed as dotted key paths (table `g8`, key `lo`). Caused all DNS pods to crash with TOML parse error.
- DNS pod endless restart loop: `generateMinimalTOML` iterated `p.networks` (Go map, random order) for forward zones. Output changed every call, causing `syncConfigMapsToDisk` to detect "changes" every 10s reconcile cycle, endlessly restarting DNS pods. Fixed by sorting peer zones alphabetically.
- Job scheduler PXE reboot of online hosts: Scheduler overwrote BMH template/bootConfigRef/image even when server was already online. BMH operator saw config change → triggered PXE reboot. Fixed: when host is already online, skip all boot config changes — agent picks up new job via work poll.
- Built-in web console UI: Integrated standalone `console` Rust project into mkube as Go `pkg/console`. 13 pages (Dashboard, Nodes, Pods, Deployments, Networks, BMH, BootConfigs, Registries, Storage, Jobs, CloudID, Logs). Served at `/ui/` on mkube's existing port 8082. stormd extension via `[process.ui]` config. CORS headers for iframe proxy. CloudID template management (CRUD, assignments, oneshot, backup/restore). Enhanced job runner management (create/delete runners and reservations, queue view, job logs).
- Parallel job execution: Agent rewritten with worker pool — semaphore-capped concurrency (MKUBE_MAX_CONCURRENT, default 4). All agent→server calls carry job identity (heartbeat body `{"job":"ns/name"}`, logs `?job=ns/name`, complete body `jobName`/`jobNamespace`). Server handlers accept job identity with backward-compatible first-match fallback. Scheduler `findAvailableHost` counts active jobs per BMH, allows up to maxConcurrent per host. `releaseJobHost` only clears ActiveJob when no active jobs remain. HostReservation `ActiveJob` is informational (last assigned), scheduling decisions based on actual job counts.
- Agent container storage on data disk: `agent-storage-setup.sh` in bootconfig auto-detects largest non-boot block device, formats XFS if needed, mounts at `/var/data`. Podman container storage bind-mounted from `/var/data/agent-storage:/var/lib/containers`. Prevents OS disk exhaustion during large image pulls (rawhidedev is multi-GB).
- StoragePool CRD (IMPLEMENTED): Cluster-scoped CRD in `pkg/provider/storage_pool_crd.go`. Auto-discovers RouterOS hardware/RAID disks at boot via `/disk` REST API. Each pool has a mount-point name (e.g. "raid1", "sata1"), capacity tracking, and resource counts. PVCs select pools via `storageClassName` or `vkube.io/storage-pool` annotation. iSCSI disks via `spec.storagePool`. Default pool provides backward compat. Migration API (`POST /api/v1/storagepools/{name}/migrate`) moves PVCs/disks between pools. Console Pools tab with per-pool capacity bars.
- Agent podman socket API library: `pkg/podman/client.go` — pure Go Podman REST API client via Unix socket (`/run/podman/podman.sock`). No external CLI deps. Agent rewritten to use socket API for all container operations (Info, Images, Pull, Run, Prune). Agent version/commit in heartbeat env, displayed in runner detail UI. Podman `/wait` returns plain integer (not JSON object) — handled with integer-first parse.
- Job cancellation and timeout: Agent detects server-side cancellation via heartbeat response (`{"cancel": true}`) and 404 fallback. On cancel/timeout, stops build container via `StopContainer` (podman API `POST /stop`), reports exit code 137. Heartbeat interval 10s for fast detection. Build timeout from `spec.timeout` (default 2h). Verified end-to-end: cancel → agent detects within 10s → container stopped → cleanup → worker released.

### TODO (priority order)
1. **BareMetalHost Operator (BMO)**: Owns ALL host state and state machines. Architecture:
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
3. ~~**UI Dashboard**~~: Done — built-in web console at `/ui/` with 13 pages, stormd extension, CloudID template management, job runner management.
4. **Registry push notifications to mkube-update**: Webhook/watch instead of polling.
5. **Track external microdns instances**: gw DNS on pvex needs proper sync.
6. **Fix storage test failures**: `TestEnsureImageCacheHit` and `TestProvisionVolume` in `pkg/storage/manager_test.go`.
7. **PXEManager tftpboot PV**: Move tftpboot to persistent volume (PVC support now available).
8. **TLS cert rotation**: Add API/mechanism to update registry CA+server certs and trigger all consumers (mkube, mkube-update, other containers) to reload the new CA. Currently requires manual registry restart + mkube redeploy when certs are regenerated by installer.
9. **microdns resilience**: DNS containers must survive mkube failures without going down. DNS is critical infrastructure. Currently mkube going down (update, crash) can cascade — DNS pods get deleted and recreation fails if API is slow. Need: DNS containers with `start-on-boot=yes` and independent of mkube lifecycle.
10. **gw.lo DNS forward zone**: Re-enable forwarding to gw.lo DNS at 192.168.1.52 once routing from gt/g10/g11 networks to gw network is verified working.
11. **Registry HTTP/2 proper fix**: Currently h2 is disabled as workaround. Need high-speed registry — either find root cause in Go's h2 server (GOAWAY under blob upload load), switch to a production registry lib (e.g. `distribution/distribution`), or use a reverse proxy (caddy/nginx) in front that handles h2 correctly.
12. **microdns health-checked DNS load balancing**: Add SSE-style health check to microdns round-robin — probe backends, remove dead ones from DNS responses until recovered. General-purpose pattern for any service.
13. ~~**mkube HA (2-node)**~~: Done — multi-node cluster with embedded NATS, peer sync, node-scoped reconciliation. No leader election needed — each node only reconciles its own pods.
14. **configstate.gt.lo — Git-backed config state backup**: New service hosting a git server. Each component (mkube, bmh-operator, microdns, etc.) gets its own git repo. On config changes, data is exported as JSON and pushed to the appropriate repo. Write-only — intended for change tracking and disaster recovery. Covers: server static IPs, container YAML/specs, network CRDs, BMH objects, DHCP reservations, DNS zones, boot configs. NOT for ISOs or large binaries. Deploy before disruptive changes (like BMH hardware inventory rollout) to capture pre-change state.
14. **Proxmox integration test**: Deploy mkube with `backend: proxmox` against pvex.gw.lo. Smoke test: create pod via API, verify LXC container appears, test start/stop/restart, verify IPAM + DNS, verify OCI→template conversion + upload.
15. **Proxmox PVE 9.1+ native OCI**: Detect PVE version and pass OCI image ref directly to `pct create` (skip rootfs conversion). Requires PVE 9.1+ with native OCI support.

### In Progress
<!-- - [ ] (started YYYY-MM-DD) Task description -->

## Testing

```bash
# All tests
go test ./...

# Provider tests only
go test ./pkg/provider/...

# Proxmox client + VMID allocator tests
go test ./pkg/proxmox/...

# With verbose output
go test -v ./pkg/provider/...
```

Known test failures (pre-existing):
- `pkg/storage/manager_test.go`: `TestEnsureImageCacheHit` (read-only /cache), `TestProvisionVolume` (path mismatch)
