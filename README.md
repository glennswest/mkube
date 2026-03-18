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
| `mkube-agent` | Job execution agent — pulls work, runs scripts, streams logs, reports completion | CoreOS bare metal (x86_64) |

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

### Job Scheduling
- Run transient workloads on bare metal hosts managed via BMH
- **HostReservation** — claims a BMH for a named pool (prevents double-booking)
- **JobRunner** — defines pool behavior: boot config, idle timeout, reclaim policy, max concurrency, overflow
- **Job** — unit of work: bash script, env vars, priority, timeout, artifact collection
- **JobQueue** — computed read-only view of pending jobs sorted by priority
- Scheduler goroutine (10s tick) matches jobs to available hosts, powers on via BMH, monitors timeouts
- **mkube-agent** — static Go binary runs on the booted host, polls for work, executes script, streams logs, sends heartbeats, reports exit code
- Lifecycle: Pending → Scheduling → Provisioning → Running → Completed | Failed | TimedOut | Cancelled
- Idle hosts auto-powered-off after configurable timeout (reclaim policy)

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
# All binaries (mkube, mkube-update, mkube-registry, installer, agent)
make build-all

# Individual binaries
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/mkube-update/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/registry/
CGO_ENABLED=0 go build ./cmd/installer/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/mkube-agent/
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

### Networks (cluster-scoped)
```
GET    /api/v1/networks                                # List all networks
GET    /api/v1/networks/{name}                         # Get network (enriched with status)
POST   /api/v1/networks                                # Create network
PUT    /api/v1/networks/{name}                         # Update network
PATCH  /api/v1/networks/{name}                         # Patch network (merge)
DELETE /api/v1/networks/{name}                         # Delete (409 if pods reference it)
GET    /api/v1/networks/{name}/config                  # Generate microdns TOML config
```

### PersistentVolumeClaims
```
GET    /api/v1/persistentvolumeclaims                  # List all PVCs
GET    /api/v1/namespaces/{ns}/persistentvolumeclaims  # List in namespace
GET    /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}  # Get PVC
POST   /api/v1/namespaces/{ns}/persistentvolumeclaims  # Create PVC
PUT    /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}  # Update PVC
DELETE /api/v1/namespaces/{ns}/persistentvolumeclaims/{name}  # Delete PVC
```

### BareMetalHosts
```
GET    /api/v1/baremetalhosts                          # List all BMH
GET    /api/v1/namespaces/{ns}/baremetalhosts          # List in namespace
GET    /api/v1/namespaces/{ns}/baremetalhosts/{name}   # Get BMH
POST   /api/v1/namespaces/{ns}/baremetalhosts          # Create BMH
PUT    /api/v1/namespaces/{ns}/baremetalhosts/{name}   # Update BMH
PATCH  /api/v1/namespaces/{ns}/baremetalhosts/{name}   # Patch BMH (merge)
DELETE /api/v1/namespaces/{ns}/baremetalhosts/{name}    # Delete BMH
```

### iSCSI CDROMs (cluster-scoped)
```
GET    /api/v1/iscsi-cdroms                            # List all CDROMs
GET    /api/v1/iscsi-cdroms/{name}                     # Get CDROM
POST   /api/v1/iscsi-cdroms                            # Create CDROM (phase=Pending)
PUT    /api/v1/iscsi-cdroms/{name}                     # Update CDROM
PATCH  /api/v1/iscsi-cdroms/{name}                     # Patch CDROM
DELETE /api/v1/iscsi-cdroms/{name}                     # Delete (409 if subscribers exist)
POST   /api/v1/iscsi-cdroms/{name}/upload              # Upload ISO (multipart, field: "iso")
POST   /api/v1/iscsi-cdroms/{name}/subscribe           # Subscribe host
POST   /api/v1/iscsi-cdroms/{name}/unsubscribe         # Unsubscribe host
```

**iSCSI CDROM workflow:**
1. Create the CDROM object (returns phase=Pending)
2. Upload the ISO file via multipart POST (streams to `/raid1/iso/`, configures RouterOS iSCSI target)
3. Subscribe hosts to get iSCSI connection details (targetIQN, portalIP, portalPort)
4. Unsubscribe when done; delete CDROM with `?deleteISO=true` to remove the ISO file

Use the helper script for create+upload+cleanup in one step:
```bash
scripts/setup-iscsi-cdrom.sh <name> <iso-file> [description]
```

### HostReservations (namespaced)
```
GET    /api/v1/hostreservations                                   # List all
GET    /api/v1/namespaces/{ns}/hostreservations                   # List in namespace
GET    /api/v1/namespaces/{ns}/hostreservations/{name}            # Get
POST   /api/v1/namespaces/{ns}/hostreservations                   # Create
PUT    /api/v1/namespaces/{ns}/hostreservations/{name}            # Update
PATCH  /api/v1/namespaces/{ns}/hostreservations/{name}            # Patch (merge)
DELETE /api/v1/namespaces/{ns}/hostreservations/{name}            # Delete (409 if active job)
```

### JobRunners (cluster-scoped)
```
GET    /api/v1/jobrunners                              # List all
GET    /api/v1/jobrunners/{name}                       # Get (enriched with live status)
POST   /api/v1/jobrunners                              # Create
PUT    /api/v1/jobrunners/{name}                       # Update
PATCH  /api/v1/jobrunners/{name}                       # Patch (merge)
DELETE /api/v1/jobrunners/{name}                       # Delete (409 if active jobs)
```

### Jobs (namespaced)
```
GET    /api/v1/jobs                                    # List all
GET    /api/v1/namespaces/{ns}/jobs                    # List in namespace
GET    /api/v1/namespaces/{ns}/jobs/{name}             # Get
POST   /api/v1/namespaces/{ns}/jobs                    # Create (starts as Pending)
PUT    /api/v1/namespaces/{ns}/jobs/{name}             # Update
PATCH  /api/v1/namespaces/{ns}/jobs/{name}             # Patch (merge)
DELETE /api/v1/namespaces/{ns}/jobs/{name}             # Delete (409 if running)
POST   /api/v1/namespaces/{ns}/jobs/{name}/cancel      # Cancel a running job
GET    /api/v1/namespaces/{ns}/jobs/{name}/logs        # Get job output logs
```

### JobQueue (computed view)
```
GET    /api/v1/jobqueue                                # Pending jobs sorted by priority
```

### Agent Endpoints (source-IP authenticated)
```
GET    /api/v1/agent/work                              # Agent polls for assigned job
POST   /api/v1/agent/heartbeat                         # Agent heartbeat (30s interval)
POST   /api/v1/agent/logs                              # Agent streams log lines
POST   /api/v1/agent/complete                          # Agent reports exit code
```

### ConfigMaps
```
GET    /api/v1/namespaces/{ns}/configmaps              # List in namespace
GET    /api/v1/namespaces/{ns}/configmaps/{name}       # Get ConfigMap
POST   /api/v1/namespaces/{ns}/configmaps              # Create ConfigMap
PUT    /api/v1/namespaces/{ns}/configmaps/{name}       # Update ConfigMap
DELETE /api/v1/namespaces/{ns}/configmaps/{name}        # Delete ConfigMap
```

### Registries (cluster-scoped)
```
GET    /api/v1/registries                              # List all registries
GET    /api/v1/registries/{name}                       # Get registry
POST   /api/v1/registries                              # Create registry
PUT    /api/v1/registries/{name}                       # Update registry
DELETE /api/v1/registries/{name}                       # Delete registry
```

### Events
```
GET    /api/v1/events                                  # List all events
GET    /api/v1/namespaces/{ns}/events                  # List events in namespace
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

## oc / kubectl Commands

All resources support `oc` (or `kubectl`) with `--server=http://192.168.200.2:8082`. Table format output is supported for all resource types.

### Resource Types

| Resource | Short Name | Namespaced | Kind |
|----------|-----------|------------|------|
| baremetalhosts | bmh | yes | BareMetalHost |
| configmaps | | yes | ConfigMap |
| deployments | deploy | yes | Deployment |
| events | | yes | Event |
| hostreservations | hres | yes | HostReservation |
| iscsi-cdroms | icd | no | ISCSICdrom |
| jobrunners | jr | no | JobRunner |
| jobs | job | yes | Job |
| jobqueue | jq | no | Job (computed) |
| namespaces | | no | Namespace |
| networks | net | no | Network |
| nodes | | no | Node |
| persistentvolumeclaims | pvc | yes | PersistentVolumeClaim |
| pods | | yes | Pod |
| registries | reg | no | Registry |
| services | | yes | Service |

### Common Commands

```bash
# Set server for convenience
export KUBECONFIG=/dev/null
alias mk='oc --server=http://192.168.200.2:8082'

# List resources
mk get pods -A                    # All pods across namespaces
mk get deploy -A                  # All deployments
mk get networks                   # All networks (cluster-scoped)
mk get pvc -A                     # All PVCs
mk get bmh -A                     # All bare metal hosts
mk get icd                        # All iSCSI CDROMs (short name)
mk get registries                 # All registries
mk get events -A                  # All events
mk get configmaps -n g10          # ConfigMaps in g10 namespace

# Get specific resource
mk get pod pxe -n g10             # Specific pod
mk get network gt                 # Specific network
mk get bmh server1 -n default     # Specific BMH

# Apply manifests
mk apply -f pod.yaml              # Create/update from YAML
mk apply -f deployment.yaml

# Delete resources
mk delete pod mypod -n default
mk delete bmh server1 -n default

# API discovery
mk api-resources                  # List all available resource types
```

## Job Scheduling

Run transient workloads (builds, tests, provisioning scripts) on bare metal hosts. The scheduler automatically powers on hosts, boots CoreOS via PXE/ignition, runs your script, streams logs, and powers off when idle.

### Concepts

| Resource | Scope | Purpose |
|----------|-------|---------|
| **HostReservation** | Namespaced | Claims a BMH for a pool — prevents double-booking |
| **JobRunner** | Cluster | Pool template — boot config, idle timeout, concurrency limit, reclaim policy |
| **Job** | Namespaced | Unit of work — bash script + env + priority + timeout |
| **JobQueue** | Computed | Read-only view of pending jobs sorted by priority |

### Setup

One-time setup to enable job scheduling on a host. Apply the provided manifest or create resources individually:

```bash
# Option A: Apply the bundled setup manifest (creates all 3 resources)
mk apply -f deploy/job-scheduling-setup.yaml

# Option B: Create resources individually
```

**1. Create a BootConfig with mkube-agent ignition:**

```yaml
apiVersion: v1
kind: BootConfig
metadata:
  name: coreos-agent
spec:
  format: ignition
  description: CoreOS with mkube-agent for job execution
  data:
    config.ign: |
      {
        "ignition": {"version": "3.4.0"},
        "storage": {
          "directories": [{"path": "/data", "mode": 493}],
          "files": [
            {
              "path": "/usr/local/bin/mkube-agent",
              "mode": 493,
              "contents": {
                "source": "http://192.168.200.2:8082/api/v1/bootconfigs/coreos-agent/files/mkube-agent"
              }
            },
            {
              "path": "/etc/mkube-agent.env",
              "mode": 420,
              "contents": {"inline": "MKUBE_API=http://192.168.200.2:8082"}
            }
          ]
        },
        "systemd": {
          "units": [{
            "name": "mkube-agent.service",
            "enabled": true,
            "contents": "[Unit]\nDescription=mkube job agent\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nEnvironmentFile=/etc/mkube-agent.env\nExecStart=/usr/local/bin/mkube-agent\nWorkingDirectory=/data\n\n[Install]\nWantedBy=multi-user.target\n"
          }]
        }
      }
```

**2. Create a JobRunner (defines the pool):**

```yaml
apiVersion: v1
kind: JobRunner
metadata:
  name: build-runner
spec:
  pool: build                  # pool name — jobs target this
  bootConfigRef: coreos-agent  # ignition config for booting hosts
  idleTimeout: 300             # power off after 5 min idle
  reclaimPolicy: PowerOff      # PowerOff (default) or Retain
  maxConcurrent: 1             # max simultaneous jobs (0 = unlimited)
```

**3. Reserve a host for the pool:**

```yaml
apiVersion: v1
kind: HostReservation
metadata:
  name: server1
  namespace: default
spec:
  bmhRef: server1    # must match an existing BareMetalHost name
  pool: build        # must match a JobRunner pool
  owner: ci
  purpose: Build and test jobs
```

### Submitting Jobs

```yaml
apiVersion: v1
kind: Job
metadata:
  name: my-build
  namespace: default
spec:
  pool: build           # target pool (must have a JobRunner)
  priority: 10          # higher = scheduled first (default 0)
  script: |
    #!/bin/bash
    set -euo pipefail
    echo "Building on $(hostname) at $(date)"
    uname -a
    # your build commands here
  env:
    REPO: https://github.com/example/project
    BRANCH: main
  timeout: 3600         # max 1 hour (0 = no limit)
```

```bash
mk apply -f job.yaml
```

### Monitoring

```bash
# Runner status (hosts, active/completed/failed counts)
mk get jr
# NAME           POOL    BOOT-CONFIG    HOSTS   ACTIVE   COMPLETED   FAILED   IDLE-TIMEOUT   AGE
# build-runner   build   coreos-agent   1       1        5           0        300s           2d

# Host reservations (which hosts, active jobs)
mk get hres
# NAME      POOL    BMH       OWNER   STATUS   ACTIVE-JOB            AGE
# server1   build   server1   ci      Active   default/my-build      2d

# All jobs (status, host, duration, exit code)
mk get job
# NAME       NAMESPACE   POOL    PRIORITY   STATUS      HOST      DURATION   EXIT   AGE
# my-build   default     build   10         Running     server1   3m22s      -      5m
# old-build  default     build   0          Completed   server1   12m4s      0      1d

# Pending job queue (priority sorted)
mk get jq
# #   NAME        NAMESPACE   POOL    PRIORITY   AGE
# 1   urgent-job  default     build   100        10s
# 2   next-job    default     build   5          2m
# 3   low-job     default     build   0          5m

# Job logs
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build/logs
```

### Job Lifecycle

```
                    ┌─────────┐
                    │ Pending │  (queued, waiting for host)
                    └────┬────┘
                         │  scheduler assigns host + powers on BMH
                    ┌────▼──────┐
                    │ Scheduling│
                    └────┬──────┘
                         │  BMH bootConfigRef set, online=true
                    ┌────▼────────┐
                    │ Provisioning│  (host PXE booting, agent starting)
                    └────┬────────┘
                         │  agent calls GET /api/v1/agent/work
                    ┌────▼───┐
                    │ Running │  (script executing, logs streaming)
                    └────┬───┘
                         │
              ┌──────────┼──────────┐
         ┌────▼─────┐ ┌──▼───┐ ┌───▼─────┐
         │ Completed│ │Failed│ │ TimedOut │
         │ (exit 0) │ │      │ │         │
         └──────────┘ └──────┘ └─────────┘
```

**Timeout enforcement:**
- **Provisioning timeout** — 10 minutes for the host to PXE boot and agent to connect
- **Running timeout** — configurable per job via `spec.timeout` (seconds)
- **Heartbeat timeout** — 90 seconds since last agent heartbeat (detects agent crashes)

### Cancelling Jobs

```bash
# Cancel a running/pending job
curl -s -X POST http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build/cancel
```

### Building mkube-agent

```bash
# Build the agent binary (static linux/amd64)
make build-agent    # → dist/mkube-agent (5.7 MB)

# Build + push agent container image (stormdbase with SSH, logging, liveness)
make deploy-agent   # → registry.gt.lo:5000/mkube-agent:edge

# Or build everything including the agent
make build-all
```

The agent is available in two deployment modes:
- **Binary via Ignition** — BootConfig downloads `mkube-agent` from mkube during PXE boot (systemd oneshot service)
- **Container via stormdbase** — `registry.gt.lo:5000/mkube-agent:edge` includes SSH (port 22), management API (port 9080), log capture, and auto-restart on exit

### Job Guide: Build, Submit, and Get Results

End-to-end walkthrough for running a job on bare metal.

#### Prerequisites

You need three resources already in place (one-time setup — see [Setup](#setup) above):

1. **BareMetalHost** — a registered server with IPMI credentials
2. **HostReservation** — claims that BMH for a pool (e.g. `build`)
3. **JobRunner** — defines the pool's boot config, idle timeout, and concurrency

Verify the pool is ready:

```bash
# Check runner exists and has reserved hosts
mk get jr
# NAME           POOL    BOOT-CONFIG    HOSTS   ACTIVE   COMPLETED   FAILED   IDLE-TIMEOUT   AGE
# build-runner   build   coreos-agent   1       0        0           0        300s           2d

# Check host reservation is active
mk get hres -A
# NAME      POOL    BMH       OWNER   STATUS   ACTIVE-JOB   AGE
# server1   build   server1   ci      Active                2d
```

#### Step 1: Write the Job Manifest

Create a YAML file (e.g. `my-job.yaml`):

```yaml
apiVersion: v1
kind: Job
metadata:
  name: my-build-001
  namespace: default
spec:
  pool: build
  priority: 10
  timeout: 3600
  script: |
    #!/bin/bash
    set -euo pipefail
    echo "=== Build started on $(hostname) at $(date) ==="
    echo "OS: $(cat /etc/os-release | grep PRETTY_NAME)"
    echo "CPUs: $(nproc)"
    echo "RAM: $(free -h | awk '/Mem:/{print $2}')"

    # Clone and build
    git clone --depth 1 -b "${BRANCH}" "${REPO}" /data/src
    cd /data/src
    make build
    make test

    echo "=== Build finished at $(date) ==="
  env:
    REPO: https://github.com/example/project
    BRANCH: main
```

#### Step 2: Submit the Job

```bash
# Submit via mk apply
mk apply -f my-job.yaml

# Or submit via curl
curl -s -X POST http://192.168.200.2:8082/api/v1/namespaces/default/jobs \
  -H 'Content-Type: application/json' \
  -d @my-job.yaml
```

The job starts in **Pending** phase. The scheduler picks it up within 10 seconds.

#### Step 3: Watch Progress

```bash
# Quick status check
mk get jobs -n default
# NAME            POOL    PRIORITY   STATUS        HOST      DURATION   EXIT   AGE
# my-build-001    build   10         Running       server1   2m15s      -      3m

# Detailed status (shows phase, host IP, heartbeat, timing)
mk get job my-build-001 -n default -o yaml

# Or via curl for JSON
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build-001 | python3 -m json.tool
```

Phase progression to watch for:
- **Pending** → waiting for a free host in the pool
- **Provisioning** → host powered on, PXE booting (up to 10 min)
- **Running** → agent connected, script executing
- **Completed** / **Failed** → done

#### Step 4: Get the Results

**Check exit code and status:**

```bash
mk get job my-build-001 -n default -o yaml
```

Key status fields:
```yaml
status:
  phase: Completed          # or Failed, TimedOut, Cancelled
  exitCode: 0               # 0 = success, non-zero = failure
  startedAt: "2026-03-18T20:01:00Z"
  completedAt: "2026-03-18T20:13:04Z"
  logLines: 247
  bmhRef: server1
  hostIP: 192.168.10.10
```

**Get the full log output:**

```bash
# Via curl (returns plain text)
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build-001/logs

# Save to file
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build-001/logs > build.log

# Tail the last 20 lines
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build-001/logs | tail -20
```

Logs are streamed in real-time while the job runs — you can poll repeatedly to follow progress. Logs persist in NATS for 7 days after completion.

#### Step 5: Cleanup

```bash
# Delete completed job
mk delete job my-build-001 -n default

# Or cancel a running job
curl -s -X POST http://192.168.200.2:8082/api/v1/namespaces/default/jobs/my-build-001/cancel
```

The host is automatically released back to the pool when the job finishes. After the runner's `idleTimeout` (default 300s) with no pending jobs, the host powers off.

#### Quick Reference: One-Liner Submit and Wait

```bash
# Submit, poll until done, print exit code and logs
cat <<'EOF' | mk apply -f -
apiVersion: v1
kind: Job
metadata:
  name: quick-test
  namespace: default
spec:
  pool: build
  script: |
    echo "Hello from $(hostname)"
    uname -a
  timeout: 60
EOF

# Poll until completed (check every 10s)
while true; do
  STATUS=$(curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/quick-test \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['status']['phase'])")
  echo "Status: $STATUS"
  case "$STATUS" in Completed|Failed|TimedOut|Cancelled) break;; esac
  sleep 10
done

# Print results
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/quick-test \
  | python3 -c "import json,sys; s=json.load(sys.stdin)['status']; print(f'Exit: {s.get(\"exitCode\",\"?\")}  Duration: {s.get(\"startedAt\",\"\")} → {s.get(\"completedAt\",\"\")}')"
curl -s http://192.168.200.2:8082/api/v1/namespaces/default/jobs/quick-test/logs
```

### Reclaim Policy

| Policy | Behavior |
|--------|----------|
| `PowerOff` (default) | After the last job completes and `idleTimeout` expires, all reserved hosts in the pool are powered off |
| `Retain` | Hosts stay powered on — useful for pools with frequent jobs |

### Overflow

Set `allowOverflow: true` on the JobRunner to let the scheduler use any unreserved BMH when reserved hosts are busy. Overflow hosts are used only when all pool-reserved hosts have active jobs.

## BMH Operations

### Reboot a Server

```bash
# Reboot immediately (bmh-operator powers off then on)
mk annotate bmh/server1 bmh.mkube.io/reboot="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite

# Power off
mk patch bmh server1 --type=merge -p '{"spec":{"online":false}}'

# Power on
mk patch bmh server1 --type=merge -p '{"spec":{"online":true}}'
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
  mkube-agent/      Job execution agent (runs on CoreOS bare metal)
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
  job-scheduling-setup.yaml  Job scheduling setup for server1
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
