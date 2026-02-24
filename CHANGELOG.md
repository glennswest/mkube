# Changelog

## [Unreleased]

### 2026-02-23
- **fix:** Remove gw/dns pod from boot-order — gw microdns runs on pvex.gw.lo, not rose1. The conflicting rose1 container caused IP conflict on bridge-lan and "no route to host" errors.
- **fix:** Remove legacy dnsx.gw.lo references from all network static records — PowerDNS migration is complete, dnsx is no longer used.
- **fix:** Consistency checker DNS false positives — `checkDNS` now uses all desired pods (tracked + NATS + boot-order) instead of just boot-order manifest, and includes static records, DHCP reservations, and infrastructure records (rose1, dns) in the expected set. Eliminates false "stale" warnings for legitimate records.
- **feat:** Auto-cleanup stale DNS records — `cleanStaleDNSRecords` in async consistency checker deletes A records for unknown hostnames and removes old IPs for known hostnames (e.g., accumulated records from pod IP changes).
- **fix:** Register container-level DNS records (`container.pod` format) during reconcile — pods tracked via "already exists" path never called `AllocateInterface`, so `microdns.dns` records were missing. `reregisterPodDNS` now registers both container-level and pod-level DNS records.
- **fix:** NATS-sourced pods no longer flagged as "tracked but not in manifest" — these are deployed via `oc apply` and persisted in NATS, which is normal. Changed from warn to pass.
- **feat:** `externalDNS` flag on NetworkDef — marks networks where the DNS server is not managed by mkube (e.g., gw DNS on pvex.gw.lo). Consistency checker treats unreachable external DNS zones as pass instead of fail.
- **fix:** IPAM collision on g10/g11 — container IPAM started allocating at .2 in every subnet, colliding with static server IPs (e.g., ipmiserial getting 192.168.11.15 which is server6's IPMI). Added configurable `ipamStart`/`ipamEnd` per network in config. g10 and g11 now allocate container IPs from .200-.250, well above server reservations (.10-.30) and DHCP ranges.
- **fix:** Standalone reconciler missing digest cache clear on push events — the standalone reconciler received registry push events but did NOT call `ClearImageDigestByRepo` before reconciling, so `RefreshImage` compared stale digest vs stale digest and never detected changes. Root cause of ipmiserial (and all auto-update pods) not updating on image push.
- **feat:** `GET /api/v1/images` endpoint — exposes image cache state (refs, digests, tarball paths, pull times) for debugging auto-update issues.
- **feat:** Container network health repair — automatically detects and recreates pods with broken networking (missing veth, no IP, static IP mismatch). Tracks consecutive failures with threshold of 3 before triggering recreate to avoid flapping.
- **feat:** Static IP validation in reconcile — pods tracked via "already exists" path now have their veth IP checked against `vkube.io/static-ip` annotation. Mismatches trigger immediate delete+recreate.
- **feat:** Lifecycle failure recovery — containers that exceed max restarts are now automatically recreated with fresh veth allocation via new `OnFailed` callback from lifecycle manager.
- **feat:** Network health category in consistency report — `/api/v1/consistency` now includes a `network` section showing veth presence, IP assignment, and static IP match status for all pods.

### 2026-02-24
- **fix:** DHCP reservation hostname — `FC:4C:EA:F9:4F:2F` was mapped to `server30` but the device identifies as `gb10`. Changed `server30`→`gb10` and `server30b`→`gb10b` in g10 DHCP reservations and DNS.
- **perf:** DNS record cache (batch mode) — `BeginBatch`/`EndBatch` on DNS client caches `ListRecords` results per zone and blacklists failed endpoints for the remainder of the batch, avoiding O(pods × containers × 2) HTTP GETs and repeated 10s timeouts during reconcile. Applied to `reregisterPodDNS`, `InitDNSZones`, and `cleanStaleDNSRecords`.
- **perf:** Reduce DNS HTTP timeout from 10s to 3s — DNS containers are local, 10s was far too generous and caused 60s reconcile cycles when namespace DNS endpoints were unreachable.
- **feat:** Boot timing instrumentation — all startup phases (`BOOT:` prefix) and reconcile steps (`RECONCILE:` prefix) now log elapsed milliseconds. Identifies bottlenecks: discovery (8.4s), DZO bootstrap (6.2s), DNS init (3.2s) = ~18s boot total. First reconcile ~188s (pod creation + DNS registration).
- **fix:** Stale DNS cleanup — when a pod gets a new IP, old A records for the same hostname are automatically removed before registering the new one. Prevents accumulation of stale DNS entries across pod recreations.

### 2026-02-23
- **fix:** Consistency checker crash-looping containers — orphan detection only checked `p.pods` (tracked pods), not NATS store or boot-order manifest. NATS-sourced pods like ipmiserial were incorrectly flagged as orphaned and killed. Now checks all desired sources (tracked + NATS + boot-order) and skips cleanup entirely when NATS isn't connected yet.
- **fix:** Pods missing IPs after restart — IPAM not re-synced for pods tracked via "already exists" path during reconcile. Added ResyncAllocations call in reconcile and consistency checker to ensure all veths have IPAM entries
- **fix:** Auto-cleanup stale containers in CreatePod — detects and removes orphaned RouterOS containers before recreation, preventing "in use by container" veth errors
- **fix:** Force-release veths held by orphaned containers — when veth allocation fails, finds the container holding the veth, stops/removes it, and retries
- **feat:** Orphaned container detection in consistency checker — async cleanup removes RouterOS containers that follow mkube naming but aren't tracked by any pod
- **fix:** Always delete stale tarball cache and rebuild from registry — registry is the source of truth for container images
- **fix:** Clear image digest cache on registry push events — ensures immediate detection of new image pushes
- **fix:** Add persistent mount for registry blob store (`/raid1/volumes/kube.gt.lo/registry`) — data survives mkube redeploy
- **fix:** Make deploy.sh idempotent for mount creation — prevents duplicate mount entries on re-deploy
- **fix:** Paginate DNS ListRecords to find all duplicates — was only fetching first 100 records
- **feat:** Async consistency checker — runs after CreatePod, DeletePod, and reconcile to clean up orphaned veths and stale IPAM entries
- **feat:** DNS backup system — JSON exports of all microdns zones saved to `dns-backup/` for disaster recovery
- **feat:** PowerDNS migration — imported all gw.lo, g10.lo, g11.lo records + reverse zones from legacy PowerDNS (dnsx)
- **fix:** Store data volumes under `/raid1/volumes/` instead of `/raid1/images/` — prevents tarball extraction from wiping persistent data (DNS databases, etc.) on container recreation
- **fix:** Reinitialize NATS KV buckets on reconnect — prevents stale stream handles after NATS container restart
- **feat:** Add static DNS records for all zones — rose1, dns, dnsx, nats, mkube in gt/g10/g11/gw networks
- **fix:** Enable NATS monitoring port (`-m 8222`) so liveness probe works — was causing max restart failures and JetStream stream-not-found errors
- **fix:** Prevent reconciler race with image redeploy goroutine — reconciler skips pods being redeployed
- **feat:** Image auto-update: proper digest headers, stale image detection for tracked pods
- **fix:** Image update pipeline: push-triggered reconcile, robust DeletePod, orphan detection
- **feat:** Add DHCP relay support with server_ip, user=0:0, and serverNetwork routing
- **fix:** PXE boot chain: point nextServer to pxe pod and add static DNS record
- **fix:** Orphaned static IP preventing DNS container recreation
