# Changelog

## [Unreleased]

### 2026-02-23
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
