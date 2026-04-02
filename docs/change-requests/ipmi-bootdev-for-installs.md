# Change Request: IPMI Boot Device Control for ISO Installs

## Status: IMPLEMENTED (bb74473, 2026-04-02)

## Problem

When a BMH is set to a CDROM image (e.g., `fedora43`) for an OS install, the server PXE boots, iPXE sanboots the iSCSI CDROM, kickstart runs, and the server reboots. On reboot, the server PXE boots again and **reinstalls from the CDROM** in an infinite loop, because the BIOS boot order still has PXE first.

Previous attempts to solve this via iPXE boot-once scripting (dynamic `ipxe_boot_url` endpoint that switches the BMH image to `localboot` after serving the sanboot command) failed because iPXE does not reliably chain-load HTTP URLs from the DHCP boot file field.

## Implementation

### Package: `pkg/bmc/`

Pure-Go IPMI client using `bougou/go-ipmi` v0.8.2. Works from scratch containers — no `ipmitool` binary needed.

#### Files

| File | Purpose |
|------|---------|
| `pkg/bmc/client.go` | `BMC` interface, `BootDevice`/`PowerAction` types, `Credentials` struct, `IsInstallImage()` helper |
| `pkg/bmc/ipmi.go` | IPMI v2.0 LANPLUS implementation — `SetBootDevice`, `Power`, `GetPowerStatus`, `Close` |
| `pkg/bmc/mock.go` | Test mock with call recording, `MockClientFactory` for injection |
| `pkg/bmc/controller.go` | Event-driven power controller goroutine, DHCP lease watcher |
| `pkg/bmc/controller_test.go` | 8 unit tests |
| `pkg/provider/bmc_integration.go` | Provider wiring — callbacks, `enqueueBMCPowerEvent`, `RunBMCController` |

#### Architecture

The `bmc.Controller` runs as a goroutine, processing `PowerEvent` structs from a buffered channel (capacity 32). Events are enqueued non-blocking from API handlers and the job scheduler.

Three power paths:

1. **Install boot** (`IsInstallImage(image) == true`):
   - `SetBootDevice(PXE)` — one-time BIOS override, non-persistent
   - `GetPowerStatus()` → `Power(On)` or `Power(Reset)` if already on
   - Spawn lease watcher goroutine (see below)

2. **Normal power on** (image is `""`, `localboot`, or `baremetalservices`):
   - `GetPowerStatus()` → `Power(On)` or `Power(Reset)` if already on
   - No boot device override

3. **Power off**:
   - `Power(Off)`

All paths update `status.PoweredOn` via the `OnPowerChanged` callback.

#### DHCP Lease Watcher (Boot Device → Disk Timing)

After an install boot, the controller subscribes to `mkube.dhcp.*.lease` NATS events and filters by the BMH's boot MAC address. When a `LeaseCreated` event matches, it proves the server has completed BIOS POST and consumed the PXE boot flag — safe to set `bootdev disk`.

On lease detection (or 3-minute timeout fallback):
1. `SetBootDevice(Disk)` — next reboot goes to hard drive
2. `OnInstallBooted` callback → sets `spec.image = "localboot"` + re-syncs DHCP reservations

Duplicate watchers for the same BMH are cancelled — only the latest one runs.

#### Integration Points

**`pkg/provider/provider.go`**:
- `bmcController *bmc.Controller` field on `MicroKubeProvider`
- Initialized in `NewMicroKubeProvider()` (nil-safe if Logger is nil)
- Store reference updated in `SetStore()` for deferred NATS connection

**`pkg/provider/baremetalhost.go`**:
- `handleUpdateBMH` (PUT) — enqueues BMC event when `spec.online` changes
- `handlePatchBMH` (PATCH) — same

**`pkg/provider/job_scheduler.go`**:
- `schedulePendingJobs` — enqueues BMC power-on when scheduler sets `spec.online = true`
- `checkIdleRunners` — enqueues BMC power-off when idle timeout expires
- `ensureScheduledHostsOnline` — enqueues BMC power-on during scheduled hours

**`cmd/mkube/main.go`**:
- `go p.RunBMCController(ctx)` alongside other goroutines

### `IsInstallImage()` Logic

Returns `true` for any image that represents an OS install (server should PXE boot to installer). Returns `false` for:
- `""` (empty — localboot default)
- `"localboot"` (explicit disk boot)
- `"baremetalservices"` (agent-only boot, no OS install)

Case-insensitive comparison.

## BMH Lifecycle for Installs

```
User sets:  spec.image = "fedora43", spec.online = true

mkube BMC controller:
  1. SetBootDevice(PXE)              — one-time BIOS override
  2. Power(On) or Power(Reset)       — start the server
  3. Subscribe mkube.dhcp.*.lease    — watch for boot MAC
  4. DHCP lease detected (or 3m timeout)
  5. SetBootDevice(Disk)             — next boot goes to disk
  6. Set spec.image = "localboot"    — clean up DHCP reservation
  7. Re-sync DHCP to network CRD

Server:
  1. BIOS POST (reads & consumes PXE boot flag)
  2. PXE boots → iPXE sanboots iSCSI CDROM
  3. Kickstart installs OS, reboots
  4. BIOS POST (reads & consumes disk boot flag)
  5. Boots from disk → OS starts normally
  6. No reinstall loop
```

## Test Plan

1. `go test ./pkg/bmc/...` — 8 unit tests pass ✅
2. `go test ./...` — no regressions ✅
3. `go build ./cmd/mkube/` — compiles ✅
4. Deployed to rose1 (bb74473) ✅
5. **Pending end-to-end test**: Power on a BMH with an install image (e.g. server2 with `fedora43`) and verify:
   - Logs show: `SetBootDevice(PXE)` → `Power(On)` → DHCP lease detected → `SetBootDevice(Disk)` → image switched to `localboot`
   - Server installs OS once, reboots to disk, no reinstall loop

## Current BMH Fleet (as of deploy)

| Server | BMC IP | Image | Online |
|--------|--------|-------|--------|
| server1 | 192.168.11.10 | localboot | true |
| server2 | 192.168.11.11 | fedora43 | false |
| server3 | 192.168.11.12 | localboot | false |
| server4 | 192.168.11.13 | baremetalservices | false |
| server5 | 192.168.11.14 | baremetalservices | false |
| server6 | 192.168.11.15 | baremetalservices | false |
| server7 | 192.168.11.16 | baremetalservices | false |
| server8 | 192.168.11.17 | fcos-cloudid | false |
| server9 | 192.168.11.18 | baremetalservices | false |

**server2** (`fedora43`) and **server8** (`fcos-cloudid`) will exercise the install boot path when powered on. All others use `localboot` or `baremetalservices` and will use the normal power path.
