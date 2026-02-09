# mikrotik-kube

A single-binary [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider for MikroTik RouterOS containers, with integrated network management (IPAM), storage management, systemd-like boot ordering, and an embedded OCI registry.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  mikrotik-kube (single Go binary, runs as RouterOS container)    │
│                                                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────┐ │
│  │ Virtual       │  │ Network      │  │ Storage Manager        │ │
│  │ Kubelet +     │  │ Manager      │  │ • OCI→tarball convert  │ │
│  │ RouterOS      │  │ • IPAM pool  │  │ • Volume provisioning  │ │
│  │ Provider      │  │ • veth/bridge│  │ • Garbage collection   │ │
│  └──────┬───────┘  └──────┬───────┘  └────────────┬───────────┘ │
│         │                 │                        │             │
│  ┌──────┴─────────────────┴────────────────────────┴───────────┐ │
│  │                  RouterOS REST API Client                    │ │
│  └─────────────────────────┬───────────────────────────────────┘ │
│                            │                                     │
│  ┌─────────────────────────┴───────────────────────────────────┐ │
│  │  Systemd Manager (boot ordering, health checks, watchdog)   │ │
│  └─────────────────────────────────────────────────────────────┘ │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │  Embedded OCI Registry (:5000, pull-through cache)          │ │
│  └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
         │
         ▼  RouterOS REST API (/rest/container/*)
┌──────────────────────────────────────────────────────────────────┐
│  MikroTik RouterOS Container Runtime                             │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐                               │
│  │ C1  │ │ C2  │ │ C3  │ │ C4  │  ...                          │
│  └─────┘ └─────┘ └─────┘ └─────┘                               │
└──────────────────────────────────────────────────────────────────┘
```

## Features

### Virtual Kubelet Provider
- Full `PodLifecycleHandler` implementation for RouterOS
- Translates K8s Pod specs to RouterOS container API calls
- Runs standalone (local reconciler) or connected to a K8s API server
- Custom annotations for MikroTik-specific config

### Network Manager (IPAM)
- Sequential IP allocation from a configurable CIDR pool (default `192.168.200.0/24`)
- Automatic veth interface creation and bridge port assignment
- Syncs with existing allocations on startup (survives restarts)
- Per-container network isolation via RouterOS bridge

### Storage Manager
- OCI image to RouterOS tarball conversion via `go-containerregistry`
- Local tarball cache with LRU garbage collection
- Volume provisioning with per-container directory isolation
- Orphaned volume cleanup

### Systemd Manager
- Boot ordering with dependency resolution (topological sort)
- Health checks: HTTP probes, TCP probes, status polling
- Watchdog with configurable restart policies and backoff
- Max restart limits to prevent crash loops

### Embedded Registry
- OCI Distribution Spec v2 compatible
- Pull-through cache for Docker Hub, GHCR, etc.
- Local image storage for air-gapped deployments

## Quick Start

### Deploy to rose1

The fastest path from checkout to running:

```bash
# Build for ARM64 and deploy to rose1.gw.lo
make deploy
```

This single command will:
1. Build the Go binary (cross-compiled for ARM64)
2. Package it as a RouterOS-compatible rootfs tarball
3. SSH to `admin@rose1.gw.lo` and:
   - Create the `containers` bridge (`192.168.200.1/24`)
   - Add NAT masquerade for `192.168.200.0/24`
   - Create the management veth (`192.168.200.2`)
   - Upload the tarball to `/raid1/tarballs/`
   - Create and start the `mikrotik-kube` container

### Deploy to a different device

```bash
make deploy DEVICE=192.168.1.88 ARCH=arm64
```

### Build only

```bash
# For ARM64 MikroTik devices (hAP ax3, RB5009, ROSE, etc.)
make tarball ARCH=arm64

# For x86 MikroTik CHR
make tarball ARCH=amd64

# For local development
make build-local
```

## Network Layout

mikrotik-kube creates a dedicated bridge for managed containers:

```
                    ┌─────────────────────────────────┐
                    │  RouterOS                        │
                    │                                  │
 192.168.200.1/24 ──┤  bridge: "containers"            │
                    │  ├── veth-mkube  (192.168.200.2) │  ← mikrotik-kube itself
                    │  ├── veth-pod1   (192.168.200.3) │  ← managed container
                    │  ├── veth-pod2   (192.168.200.4) │  ← managed container
                    │  └── ...                         │
                    │                                  │
                    │  NAT masquerade: 192.168.200.0/24│
                    └─────────────────────────────────┘
```

Managed containers get IPs sequentially from the pool (`.3` onward; `.1` is the gateway, `.2` is mikrotik-kube).

## Configuration

### Default config

See `deploy/config.yaml` for all options. For rose1-specific settings, see `deploy/rose1-config.yaml`.

| Setting | Default | Description |
|---------|---------|-------------|
| `routeros.restUrl` | `https://192.168.200.1/rest` | RouterOS REST API endpoint |
| `network.podCIDR` | `192.168.200.0/24` | IP range for containers |
| `network.bridgeName` | `containers` | RouterOS bridge name |
| `storage.basePath` | `/raid1/images` | Container root dirs |
| `storage.tarballCache` | `/raid1/cache` | Image tarball cache |
| `storage.gcIntervalMinutes` | `30` | How often to run GC |
| `systemd.maxRestarts` | `5` | Max restarts before marking failed |
| `registry.enabled` | `true` | Enable embedded OCI registry |
| `registry.storePath` | `/raid1/registry` | Registry blob storage |

### rose1 storage layout

```
/raid1/
  images/         Container root directories
  tarballs/       Uploaded container tarballs
  cache/          Image pull cache (managed by mikrotik-kube)
  volumes/        Persistent volume mounts
  registry/       OCI registry blob storage
```

### Environment variables

| Variable | Description |
|----------|-------------|
| `MIKROTIK_KUBE_PASSWORD` | RouterOS API password |

## Custom Annotations

| Annotation | Description |
|-----------|-------------|
| `mikrotik.io/boot-priority` | Integer boot order (lower = first) |
| `mikrotik.io/depends-on` | Comma-separated container dependencies |
| `mikrotik.io/health-check` | Health check endpoint |

## Operating Modes

### Standalone Mode (default)
Reads pod manifests from a local YAML file and reconciles against RouterOS. No Kubernetes cluster required.

```bash
mikrotik-kube --standalone --config /etc/mikrotik-kube/config.yaml
```

### Virtual Kubelet Mode
Registers as a node in an existing Kubernetes cluster. Pods scheduled to this node are created on RouterOS.

```bash
mikrotik-kube --kubeconfig /path/to/kubeconfig --node-name rose1
```

Then from kubectl:
```bash
kubectl apply -f pod.yaml  # with toleration for virtual-kubelet.io/provider=mikrotik
```

## RouterOS Manual Setup

If you prefer to set up the router manually instead of using `make deploy`:

```routeros
# Enable container mode (requires reboot)
/system/device-mode/update container=yes

# Create bridge for managed containers
/interface/bridge add name=containers comment="Managed by mikrotik-kube"
/ip/address add address=192.168.200.1/24 interface=containers

# Create management veth for mikrotik-kube
/interface/veth add name=veth-mkube address=192.168.200.2/24 gateway=192.168.200.1
/interface/bridge/port add bridge=containers interface=veth-mkube

# NAT for container internet access
/ip/firewall/nat add chain=srcnat src-address=192.168.200.0/24 action=masquerade

# Create and start mikrotik-kube
/container add \
    file=raid1/tarballs/mikrotik-kube.tar \
    name=mikrotik-kube \
    interface=veth-mkube \
    root-dir=/raid1/images/mikrotik-kube \
    logging=yes \
    start-on-boot=yes \
    hostname=mikrotik-kube \
    dns=8.8.8.8

/container start [find name=mikrotik-kube]
```

## Project Structure

```
cmd/mikrotik-kube/       Entry point, CLI flags, manager initialization
pkg/
  config/              YAML configuration with CLI overrides
  routeros/            RouterOS REST API client (containers, veth, files)
  provider/            Virtual Kubelet PodLifecycleHandler implementation
  network/             IPAM allocator, veth/bridge management
  storage/             OCI-to-tarball conversion, volume provisioning, GC
  systemd/             Boot ordering, health checks, watchdog
  registry/            Embedded OCI registry with pull-through cache
deploy/
  config.yaml          Default configuration template
  rose1-config.yaml    Configuration for rose1.gw.lo
  boot-order.yaml      Boot ordering manifest examples
hack/
  build.sh             Build and export rootfs tarball
  deploy.sh            Full deployment to RouterOS device via SSH
```

## Development

```bash
make build-local    # Build for host platform
make test           # Run tests
make lint           # Lint (requires golangci-lint)
make clean          # Clean build artifacts
```

## Monitoring

```bash
# View container logs
ssh admin@rose1.gw.lo '/log/print where topics~"container"'

# Check container status
ssh admin@rose1.gw.lo '/container/print where name=mikrotik-kube'

# Check bridge and network
ssh admin@rose1.gw.lo '/interface/bridge/port/print where bridge=containers'
ssh admin@rose1.gw.lo '/ip/address/print where interface=containers'
```

## License

MIT
