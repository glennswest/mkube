# mkube

A single-binary [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider for MikroTik RouterOS containers. Manages pods, networking (IPAM), storage, DNS, DHCP, deployments, and PXE boot — all from a single Go binary running as a RouterOS container.

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│  mkube (single Go binary, runs as RouterOS container on rose1)      │
│                                                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────────┐ │
│  │ Virtual       │  │ Network      │  │ Storage Manager            │ │
│  │ Kubelet +     │  │ Manager      │  │ • OCI→tarball convert      │ │
│  │ RouterOS      │  │ • Multi-net  │  │ • Volume provisioning      │ │
│  │ Provider      │  │   IPAM       │  │ • Image digest tracking    │ │
│  │ • Pods        │  │ • veth/bridge│  │ • Registry integration     │ │
│  │ • Deployments │  │ • Static IPs │  └────────────┬───────────────┘ │
│  │ • BMH         │  └──────┬───────┘               │                 │
│  └──────┬───────┘          │                        │                 │
│         │                  │                        │                 │
│  ┌──────┴──────────────────┴────────────────────────┴───────────────┐ │
│  │                  RouterOS REST API Client                        │ │
│  └──────────────────────────┬───────────────────────────────────────┘ │
│                             │                                        │
│  ┌──────────────────────────┴───────────────────────────────────────┐ │
│  │  Lifecycle Manager (boot ordering, health checks, watchdog)      │ │
│  └──────────────────────────────────────────────────────────────────┘ │
│                                                                      │
│  ┌────────────────┐  ┌──────────────────┐  ┌──────────────────────┐ │
│  │ DNS Client     │  │ NATS JetStream   │  │ Consistency Checker  │ │
│  │ (microdns API) │  │ (persistent KV)  │  │ • DNS liveness       │ │
│  └────────────────┘  └──────────────────┘  │ • Orphan detection   │ │
│                                             │ • Network health     │ │
│                                             └──────────────────────┘ │
└──────────────────────────────────────────────────────────────────────┘
         │
         ▼  RouterOS REST API (/rest/container/*)
┌──────────────────────────────────────────────────────────────────────┐
│  MikroTik RouterOS Container Runtime                                 │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐                      │
│  │ NATS │ │ DNS  │ │ DNS  │ │ DNS  │ │ apps │  ...                  │
│  └──────┘ └──────┘ └──────┘ └──────┘ └──────┘                      │
└──────────────────────────────────────────────────────────────────────┘
```

### Component Binaries

| Binary | Description | Runs on |
|--------|-------------|---------|
| `mkube` | Main controller — pods, networking, storage, DNS, reconciler | RouterOS container (ARM64) |
| `mkube-update` | Watches registry for new images, updates mkube and other containers | RouterOS container (ARM64) |
| `mkube-registry` | Standalone OCI registry with TLS, push webhooks to mkube | RouterOS container (ARM64) |
| `mkube-installer` | One-shot bootstrap CLI — creates registry, seeds images, starts mkube-update | Local Mac/Linux |

### Container IPs (gt network)

| Container | IP | Port |
|-----------|-----|------|
| rose1 (gateway) | 192.168.200.1 | — |
| mkube | 192.168.200.2 | 8082 (API) |
| registry.gt.lo | 192.168.200.3 | 5000 (HTTPS) |
| mkube-update | 192.168.200.5 | — |
| NATS | 192.168.200.10 | 4222 |

## Features

### Pod Management
- Full `PodLifecycleHandler` for RouterOS containers
- Translates K8s Pod specs to RouterOS container API calls
- Persists desired state in NATS JetStream KV store
- Reconcile loop (10s) ensures desired state matches actual
- Staggered restarts — pods sharing the same image are restarted one at a time with liveness verification

### Deployment Controller
- Lightweight custom Deployment type (no ReplicaSets)
- Auto-recreates pods on deletion (within 10s)
- Scale up/down via replica count
- Rolling image updates with liveness checks between replicas
- DNS round-robin load balancing via shared aliases
- CRUD API at `/api/v1/namespaces/{ns}/deployments`

### Network Manager (IPAM)
- Multi-network support (gt, g10, g11, gw — each with own bridge and CIDR)
- Configurable IPAM ranges per network (`ipamStart`/`ipamEnd`)
- Static IP assignment via `vkube.io/static-ip` annotation
- Automatic veth creation and bridge port assignment
- Re-syncs allocations on restart

### DNS Management
- Integrated microdns client for automatic DNS registration
- Pod aliases via `vkube.io/aliases` annotation
- Per-namespace DNS zones with cross-zone forwarding
- Port 53 liveness probe (raw UDP DNS query, not just REST API check)
- Auto-restart dead DNS pods with staggered rollout
- Stale DNS record cleanup on pod IP changes
- External DNS support (`externalDNS: true` for DNS servers not managed by mkube)

### Storage Manager
- OCI image to RouterOS tarball conversion
- Disk-cached image digests (survives restarts)
- Registry address rewriting (aliases → primary address)
- Volume provisioning with per-container isolation
- Persistent mounts for data that survives container recreation

### Lifecycle Manager
- Boot ordering with priority levels
- Health checks: HTTP probes, TCP probes, container status polling
- Watchdog with configurable restart policies and backoff
- Max restart limits with auto-recreation on failure

### Standalone Registry
- OCI Distribution Spec v2 compatible
- TLS with auto-generated CA + server certificates
- Push webhook notifications to mkube for image updates
- Per-resource locking (no global mutex)

### Consistency Checker
- Async checks after pod create/delete/reconcile
- DNS record verification (all zones, all sources)
- DNS port 53 liveness with auto-repair
- Orphan container detection and cleanup
- Network health validation (veth, IP, static IP match)
- Deployment replica count verification
- IPAM allocation re-sync

### Additional Features
- DHCP relay support with microdns integration
- PXE/UEFI boot support (`bootFileEfi` for iPXE)
- BareMetalHost custom resource for hardware management
- ConfigMap support (auto-generated from network config)
- Export/import of all resources as YAML manifests
- Event recording (ring buffer, max 256)

## Quick Start

### Fresh Device Bootstrap

```bash
# Build all binaries
make build-all

# Bootstrap a fresh MikroTik device
make deploy-installer
# Or manually:
./installer --device 192.168.1.88
```

The installer will:
1. Generate TLS CA + server certificates
2. Create the registry container on the device
3. Seed required images from GHCR to local registry
4. Start mkube-update (which then starts mkube)

### Deploy to Existing Device

```bash
# Build and deploy mkube to rose1
make deploy
```

### Build Only

```bash
# All binaries (mkube, mkube-update, mkube-registry, installer)
make build-all

# Individual binaries
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube-update/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/registry/
CGO_ENABLED=0 go build ./cmd/installer/
```

## API Reference

Base URL: `http://192.168.200.2:8082`

### Pods
```
GET    /api/v1/pods                                    # List all pods
GET    /api/v1/namespaces/{ns}/pods                    # List pods in namespace
GET    /api/v1/namespaces/{ns}/pods/{name}             # Get pod
POST   /api/v1/namespaces/{ns}/pods                    # Create pod
PUT    /api/v1/namespaces/{ns}/pods/{name}             # Update pod
DELETE /api/v1/namespaces/{ns}/pods/{name}             # Delete pod
```

### Deployments
```
GET    /api/v1/deployments                             # List all deployments
GET    /api/v1/namespaces/{ns}/deployments             # List in namespace
GET    /api/v1/namespaces/{ns}/deployments/{name}      # Get deployment
POST   /api/v1/namespaces/{ns}/deployments             # Create deployment
PUT    /api/v1/namespaces/{ns}/deployments/{name}      # Update deployment
DELETE /api/v1/namespaces/{ns}/deployments/{name}      # Delete deployment
```

### Operations
```
GET    /api/v1/consistency                             # Consistency report
GET    /api/v1/images                                  # Image cache state
POST   /api/v1/images/redeploy                         # Force redeploy by image
POST   /api/v1/registry/push-notify                    # Registry push webhook
GET    /api/v1/dns/validate                            # DNS validation
GET    /healthz                                        # Health check
```

### Manifests (oc apply compatible)
```
POST   /api/v1/apply                                   # Apply YAML manifest
GET    /api/v1/export                                  # Export all as YAML
```

## Custom Annotations

| Annotation | Description |
|-----------|-------------|
| `vkube.io/network` | Target network name (gt, g10, g11, gw) |
| `vkube.io/namespace` | DZO namespace for DNS registration |
| `vkube.io/aliases` | DNS aliases: `alias=container,alias2` |
| `vkube.io/static-ip` | Request specific IP address |
| `vkube.io/boot-priority` | Integer boot order (lower = first) |
| `vkube.io/depends-on` | Comma-separated dependencies |
| `vkube.io/image-policy` | `auto` for automatic image updates |
| `vkube.io/file` | Local tarball path (skip OCI pull) |
| `vkube.io/owner-deployment` | Set by deployment controller (do not set manually) |

## Network Layout

mkube manages multiple networks, each with its own bridge, CIDR, DNS, and DHCP:

| Network | Bridge | CIDR | DNS Server | IPAM Range |
|---------|--------|------|------------|------------|
| gt | containers | 192.168.200.0/24 | 192.168.200.199 | .2-.198 |
| g10 | bridge | 192.168.10.0/24 | 192.168.10.252 | .200-.250 |
| g11 | bridge-boot | 192.168.11.0/24 | 192.168.11.252 | .200-.250 |
| gw | bridge-lan | 192.168.1.0/24 | 192.168.1.52 (external) | — |

## Configuration

See `deploy/config.yaml` for all options. Key settings:

| Setting | Default | Description |
|---------|---------|-------------|
| `routeros.restUrl` | `https://192.168.200.1/rest` | RouterOS REST API endpoint |
| `networks[].podCIDR` | varies | IP range for containers per network |
| `networks[].ipamStart/End` | varies | IPAM allocation range |
| `networks[].dns.server` | varies | microdns server IP |
| `networks[].externalDNS` | false | DNS managed externally |
| `storage.basePath` | `/raid1/images` | Container root dirs |
| `storage.tarballCache` | `/raid1/cache` | Image tarball cache |
| `registry.address` | `192.168.200.3:5000` | Registry address |
| `registry.localAddresses` | `[...]` | Registry address aliases |
| `nats.url` | `nats://192.168.200.10:4222` | NATS JetStream URL |
| `persistentMounts` | `{...}` | Host paths for persistent data |

## Project Structure

```
cmd/
  mkube/            Main controller binary
  mkube-update/     Image update watcher
  registry/         Standalone OCI registry
  installer/        Bootstrap CLI tool (runs on Mac)
pkg/
  config/           YAML configuration with CLI overrides
  dns/              microdns REST API client
  dzo/              DNS Zone Orchestrator (cross-zone management)
  lifecycle/        Boot ordering, health checks, watchdog
  namespace/        Namespace management
  network/          Multi-network IPAM, veth/bridge management
  provider/         Virtual Kubelet provider (pods, deployments, BMH, consistency)
  registry/         OCI registry implementation
  routeros/         RouterOS REST API client
  runtime/          Container runtime abstraction
  storage/          OCI→tarball, volume provisioning, image cache
  store/            NATS JetStream KV persistence + YAML import/export
  stormbase/        StormBase runtime backend
deploy/
  config.yaml       Default configuration
  rose1-config.yaml rose1-specific config
  boot-order.yaml   Bootstrap pod manifest (NATS + DNS)
  site-pods/        Site-specific pod manifests
hack/
  build.sh          Build and export rootfs tarball
  deploy.sh         Full deployment to RouterOS device
  deploy-installer.sh  Bootstrap a fresh device
```

## Monitoring

```bash
# mkube API health
curl -s http://192.168.200.2:8082/healthz

# Consistency report
curl -s http://192.168.200.2:8082/api/v1/consistency | python3 -m json.tool

# List all pods
curl -s http://192.168.200.2:8082/api/v1/pods | python3 -m json.tool

# Image cache state
curl -s http://192.168.200.2:8082/api/v1/images | python3 -m json.tool

# RouterOS container logs
ssh admin@rose1.gw.lo '/log/print where topics~"container"'

# Container status
ssh admin@rose1.gw.lo '/container/print'
```

## License

MIT
