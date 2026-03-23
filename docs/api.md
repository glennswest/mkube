# mkube REST API Reference

Base URL: `http://192.168.200.2:8082`

## Pods (namespaced)

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

## Deployments (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/deployments` | List all deployments |
| `GET` | `/api/v1/namespaces/{ns}/deployments` | List deployments in namespace |
| `GET` | `/api/v1/namespaces/{ns}/deployments/{name}` | Get deployment |
| `POST` | `/api/v1/namespaces/{ns}/deployments` | Create deployment |
| `PUT` | `/api/v1/namespaces/{ns}/deployments/{name}` | Update deployment |
| `DELETE` | `/api/v1/namespaces/{ns}/deployments/{name}` | Delete deployment |

## ConfigMaps (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/namespaces/{ns}/configmaps` | List configmaps |
| `GET` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Get configmap |
| `POST` | `/api/v1/namespaces/{ns}/configmaps` | Create configmap |
| `PUT` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Update configmap |
| `PATCH` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Patch configmap |
| `DELETE` | `/api/v1/namespaces/{ns}/configmaps/{name}` | Delete configmap |

## PersistentVolumeClaims (namespaced)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/persistentvolumeclaims` | List all PVCs |
| `GET` | `/api/v1/namespaces/{ns}/persistentvolumeclaims` | List PVCs in namespace |
| `GET` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Get PVC |
| `POST` | `/api/v1/namespaces/{ns}/persistentvolumeclaims` | Create PVC |
| `PUT` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Update PVC |
| `DELETE` | `/api/v1/namespaces/{ns}/persistentvolumeclaims/{name}` | Delete PVC |

## Networks (cluster-scoped)

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

**NetworkSpec fields**: `type`, `pairNetwork` (companion data/IPMI link), `bridge`, `cidr`, `gateway`, `vlan`, `router`, `dns`, `dhcp`, `ipam`, `externalDNS`, `managed`, `provisioned`, `staticRecords`

## Registries (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/registries` | List all registries |
| `GET` | `/api/v1/registries/{name}` | Get registry |
| `POST` | `/api/v1/registries` | Create registry |
| `PUT` | `/api/v1/registries/{name}` | Update registry |
| `PATCH` | `/api/v1/registries/{name}` | Patch registry |
| `DELETE` | `/api/v1/registries/{name}` | Delete registry |
| `GET` | `/api/v1/registries/{name}/config` | Generated registry config |

## iSCSI CDROMs (cluster-scoped)

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

## iSCSI Disks (cluster-scoped)

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

## BootConfigs (cluster-scoped)

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
| `GET` | `/api/v1/bootconfig` | Lookup boot config by source IP -> BMH -> bootConfigRef |
| `POST` | `/api/v1/boot-complete` | Signal boot completion (source IP auth) |

## BareMetalHosts (namespaced)

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

## DNS/DHCP Proxy Resources (namespace = network name, proxied to microdns)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dnsrecords[/{name}]` | DNS records (dr) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dhcppools[/{name}]` | DHCP pools (dp) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{net}/dhcpreservations[/{name}]` | DHCP reservations (dhcpr) |
| `GET` | `/api/v1/namespaces/{net}/dhcpleases` | DHCP leases (dl) |
| `GET/POST/DELETE` | `/api/v1/namespaces/{net}/dnsforwarders[/{name}]` | DNS forwarders (df) |

## StoragePools (cluster-scoped)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/storagepools[/{name}]` | Storage pools (sp) |
| `POST` | `/api/v1/storagepools/{name}/migrate` | Migrate PVC/disk to pool |

## Job Scheduling

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{ns}/hostreservations[/{name}]` | Host reservations |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/jobrunners[/{name}]` | Job runners (cluster-scoped) |
| `GET/POST/PUT/PATCH/DELETE` | `/api/v1/namespaces/{ns}/jobs[/{name}]` | Jobs |
| `POST` | `/api/v1/namespaces/{ns}/jobs/{name}/cancel` | Cancel job |
| `GET` | `/api/v1/namespaces/{ns}/jobs/{name}/logs` | Get job logs |
| `GET` | `/api/v1/jobqueue` | Computed job queue view |

## Agent Endpoints (source-IP authenticated)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/api/v1/agent/work` | Pull work assignment |
| `POST` | `/api/v1/agent/heartbeat` | Agent heartbeat |
| `POST` | `/api/v1/agent/logs` | Stream job logs |
| `POST` | `/api/v1/agent/complete` | Signal job completion |

## Cluster & Operations

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

## `mk` CLI Shorthand

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
mk get sp                          # Storage pools
```
