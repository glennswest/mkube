# Boot Architecture: ISO + Config

## Overview

All servers boot via iSCSI sanboot from ISO images. Each ISO has a list of compatible boot configs (ignition, kickstart, etc.). The workflow is:

1. **Choose an ISO** — which iSCSI CDROM to boot (CoreOS live, Fedora netinstall, etc.)
2. **Choose a config** — filtered to configs compatible with that ISO

No hardcoded boot images in pxemanager. Everything comes from mkube CRDs.

## How It Works

1. Admin uploads ISOs as `ISCSICdrom` objects (already exists)
2. Admin creates `BootConfig` objects with config files (ignition, kickstart, etc.)
3. Admin links configs to ISOs: `iscsiCdrom.spec.bootConfigs = ["coreos-builder", "builder-target"]`
4. Admin assigns ISO + config to a BMH:
   - `bmh.spec.image = "coreos-live"` (which ISO to sanboot)
   - `bmh.spec.bootConfigRef = "coreos-builder"` (which config to serve)
5. Server PXE boots → pxemanager reads BMH → looks up ISCSICdrom IQN → generates `sanboot iscsi:...`
6. ISO boots, fetches config from `GET /api/v1/bootconfig` (source IP resolution)

## CRD Changes

### ISCSICdrom — add `spec.bootConfigs`

```go
type ISCSICdromSpec struct {
    ISOFile     string   `json:"isoFile"`               // existing
    Description string   `json:"description,omitempty"`  // existing
    ReadOnly    bool     `json:"readOnly"`               // existing
    BootConfigs []string `json:"bootConfigs,omitempty"`  // NEW: compatible BootConfig names
}
```

```yaml
apiVersion: v1
kind: ISCSICdrom
metadata:
  name: coreos-live
spec:
  isoFile: coreos-live.iso
  readOnly: true
  bootConfigs:          # configs that work with this ISO
    - coreos-builder    # live installer ignition
    - builder-target    # post-install ignition
---
apiVersion: v1
kind: ISCSICdrom
metadata:
  name: fedora-netinst
spec:
  isoFile: fedora-netinst.iso
  readOnly: true
  bootConfigs:
    - fedora-server     # kickstart for server install
    - fedora-builder    # kickstart for builder setup
---
apiVersion: v1
kind: ISCSICdrom
metadata:
  name: baremetalservices
spec:
  isoFile: baremetalservices.iso
  readOnly: true
  # no bootConfigs — standalone live OS, no config needed
```

### BareMetalHost — use existing fields

`spec.image` already exists — now references an ISCSICdrom name (not a pxemanager image).
`spec.bootConfigRef` already exists — references a BootConfig name.

```yaml
apiVersion: v1
kind: BareMetalHost
metadata:
  name: server1
  namespace: default
spec:
  bootMACAddress: "AC:1F:6B:8A:A7:9C"
  ip: "192.168.10.10"
  image: "coreos-live"              # which ISO to sanboot
  bootConfigRef: "coreos-builder"   # which config to serve (must be in ISO's bootConfigs list)
  bmc:
    address: "192.168.11.10"
    username: "ADMIN"
    password: "ADMIN"
```

### BootConfig — no changes needed

Already implemented. Stores config files as `spec.data` (map of filename→content).
Served via `GET /api/v1/bootconfig` using source IP resolution.

```yaml
apiVersion: v1
kind: BootConfig
metadata:
  name: coreos-builder
spec:
  format: ignition
  data:
    config.ign: '{"ignition":{"version":"3.4.0"}, ...}'
```

## Validation

When setting `bmh.spec.bootConfigRef`, mkube should validate:

1. The referenced BootConfig exists
2. The referenced BootConfig is in the ISO's `spec.bootConfigs` list
3. If `bmh.spec.image` has no `bootConfigs` (like baremetalservices), `bootConfigRef` should be empty

This prevents assigning a kickstart to a CoreOS ISO or vice versa.

## pxemanager iPXE Script Generation

When a server PXE boots, pxemanager:

1. Client requests `/ipxe?mac=<mac>`
2. Find BMH by MAC address (from mkube watch)
3. Read `bmh.spec.image` → look up ISCSICdrom → get `status.targetIQN`
4. Generate iPXE script:

```
#!ipxe
echo Booting coreos-live for server1 (AC:1F:6B:8A:A7:9C)
sanboot iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:file4
```

5. If `bmh.spec.image` is empty or "localboot" → return `exit` (boot from disk)

The iSCSI portal IP is always `192.168.10.1` (rose.g10.lo) for g10 clients.

## Config Serving (already implemented)

ISO kernel args point to mkube:
- CoreOS: `ignition.config.url=http://192.168.200.2:8082/api/v1/bootconfig`
- Fedora: `inst.ks=http://192.168.200.2:8082/api/v1/bootconfig`

mkube resolves: source IP → BMH → `bootConfigRef` → BootConfig → return `spec.data` content.

## pxemanager UI

### Hosts Table

Each host row has two dropdowns:

| Host | MAC | ISO | Config | Power | Actions |
|------|-----|-----|--------|-------|---------|
| server1 | AC:1F:6B:... | [coreos-live ▾] | [coreos-builder ▾] | ON | Restart / Off |

- **ISO dropdown**: lists all ISCSICdrom names + "localboot"
- **Config dropdown**: filtered to selected ISO's `spec.bootConfigs` list. Empty if ISO has no configs.
- Selecting either dropdown PATCHes the BMH via mkube API

### Data Flow

```
pxemanager watches:
  - GET /api/v1/baremetalhosts?watch=true  (existing)
  - GET /api/v1/iscsi-cdroms?watch=true    (NEW — for ISO list + IQN lookup)

pxemanager UI actions:
  - Change ISO:    PATCH /api/v1/namespaces/{ns}/baremetalhosts/{name}  {"spec":{"image":"coreos-live"}}
  - Change config: PATCH /api/v1/namespaces/{ns}/baremetalhosts/{name}  {"spec":{"bootConfigRef":"coreos-builder"}}
```

## Example Configs

### coreos-builder (live installer ignition)

Boots CoreOS live ISO, installs to disk with target config, reboots.

```json
{
  "ignition": { "version": "3.4.0" },
  "systemd": {
    "units": [{
      "name": "coreos-installer.service",
      "enabled": true,
      "contents": "[Unit]\nDescription=Install CoreOS to disk\nAfter=network-online.target\n\n[Service]\nType=oneshot\nExecStart=/usr/bin/coreos-installer install /dev/sda --ignition-url http://192.168.200.2:8082/api/v1/bootconfig --insecure-ignition\nExecStartPost=/usr/bin/systemctl reboot\n\n[Install]\nWantedBy=multi-user.target\n"
    }]
  }
}
```

After install+reboot, switch BMH to `image: localboot` + `bootConfigRef: builder-target` so the installed system gets its config on first disk boot.

### builder-target (post-install ignition)

SSH keys, podman, cockpit, buildah/skopeo, serial consoles, firewall.

### fedora-server (kickstart)

```
url --url=https://download.fedoraproject.org/pub/fedora/linux/releases/43/Everything/x86_64/os/
keyboard us
lang en_US.UTF-8
timezone UTC
rootpw --plaintext admin
bootloader --append="console=tty0 console=ttyS0,115200n8 console=ttyS1,115200n8"
clearpart --all --initlabel
autopart
reboot
%packages
@^server-product-environment
%end
```

## Summary of CRD Changes

| CRD | Field | Change |
|-----|-------|--------|
| ISCSICdrom | `spec.bootConfigs` | NEW — list of compatible BootConfig names |
| BareMetalHost | `spec.image` | Existing — now references ISCSICdrom name |
| BareMetalHost | `spec.bootConfigRef` | Existing — references BootConfig name |
| BootConfig | — | No changes needed |
