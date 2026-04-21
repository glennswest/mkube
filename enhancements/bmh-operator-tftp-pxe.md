# Enhancement: TFTP/PXE Boot Server in bmh-operator

## Problem

PXE boot for bare metal hosts is broken. Legacy Intel PXE clients need TFTP to
chain-load `undionly.kpxe` (iPXE firmware) before they can use mkube's HTTP-based
iPXE boot endpoint (`/api/v1/ipxe/boot`).

The TFTP server was previously provided by `pxemanager`, which was decommissioned
in Mar 2026 when BMH management moved to bmh-operator. The iPXE HTTP layer was
added to mkube, but the TFTP layer was never replaced.

### Current State

- **DHCP pool** has `nextServer: 192.168.10.200`, `bootFile: undionly.kpxe`
- **NAT rules** forward UDP 69 from 192.168.10.200 to mkube (192.168.200.2:69)
- **Boot files** exist on disk at `raid1/volumes/pxe.g10.lo.tftpboot/`:
  - `undionly.kpxe` (69KB) — iPXE chain-loader for BIOS PXE
  - `boot.ipxe` (63B) — iPXE boot script
  - `ipxe.efi` — iPXE chain-loader for UEFI (if present)
  - `pxelinux.0`, `ldlinux.c32` — legacy PXELINUX (fallback)
- **No process** serves TFTP on port 69

### Boot Flow (Expected)

```
Intel PXE client
  → DHCP (gets next-server=192.168.10.200, boot-file=undionly.kpxe)
  → TFTP download undionly.kpxe from 192.168.10.200
  → iPXE starts
  → DHCP again (detected as iPXE client)
  → gets ipxeBootUrl=http://mkube:8082/api/v1/ipxe/boot
  → HTTP fetch iPXE script from mkube
  → sanboot to iSCSI CDROM target
```

## Proposed Solution

bmh-operator should include a lightweight TFTP server that:

1. Serves `undionly.kpxe` and `ipxe.efi` for PXE chain-loading
2. Runs on the BMC/data network (port 69 UDP)
3. Minimal scope — only serves the iPXE firmware files needed for chain-loading
4. Files can be embedded in the binary or mounted from a volume

### Requirements

- Pure Go TFTP server (e.g. `pin/tftp` library)
- Serve from a configurable directory or embedded assets
- Listen on `:69` (standard TFTP port)
- Support read-only TFTP (no writes needed)
- Log requests for debugging PXE boot issues

### Network Topology

The TFTP server needs to be reachable from the bare metal servers' data network
(g10: 192.168.10.0/24). Options:

A. **bmh-operator pod gets a g10 IP** and the DHCP `nextServer` points directly to it
B. **NAT stays as-is** — NAT rule forwards 192.168.10.200:69 to bmh-operator's IP

### Files to Serve

At minimum:
- `undionly.kpxe` — BIOS iPXE chain-loader (~69KB)
- `ipxe.efi` — UEFI iPXE chain-loader (~1MB)
- `boot.ipxe` — optional iPXE script (can redirect to mkube's HTTP endpoint)

These are static binaries that rarely change. Embedding them in the bmh-operator
binary is acceptable.

## Impact

- Restores PXE boot for all bare metal hosts with legacy Intel PXE NICs
- Required for `baremetalservices`, `fcos-cloudid`, and all install images
- Servers with Mellanox ConnectX (built-in iPXE) don't need TFTP but benefit
  from the fallback

## Workaround (Interim)

Until bmh-operator adds TFTP:
- Create a temporary TFTP pod using the existing `pxe.g10.lo.tftpboot` mount list
- Or embed a minimal TFTP server in mkube (since NAT already points port 69 there)
