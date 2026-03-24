# Enhancement: B Port DHCP Filtering (No PXE, No Gateway)

## Problem

Bare metal servers have two data NICs on the same network (g10):
- **A port** (bootMACAddress): primary NIC, should get PXE boot options + gateway
- **B port**: secondary NIC, should get an IP only — no PXE, no default gateway

Currently mkube only creates a DHCP reservation for the A port. B ports get their IP from the DHCP pool, which includes pool-level `gateway` and `next_server`/`boot_file` defaults. This causes:
1. B port may try to PXE boot if BIOS boot order includes it
2. B port gets a default gateway, creating a competing route with the A port

## Known B Port MACs

| Server | A port MAC | A IP | B port MAC | B IP |
|--------|-----------|------|-----------|------|
| server1 | ac:1f:6b:8a:a7:9c | .10 | ? | .20 |
| server2 | ac:1f:6b:8b:11:5d | .11 | f4:52:14:84:bc:c0 | .21 |
| server3 | ac:1f:6b:8a:a4:5c | .12 | f4:52:14:84:86:f0 | .22 |
| server4 | ac:1f:6b:8a:9b:e2 | .13 | f4:52:14:84:5f:a0 | .23 |
| server5 | ac:1f:6b:8a:a3:1a | .14 | f4:52:14:84:6f:f0 | .24 |
| server6 | ac:1f:6b:8d:90:13 | .15 | f4:52:14:84:b7:d0 | .25 |
| server9 | 50:6b:4b:b1:9e:26 | .18 | 50:6b:4b:b1:9e:27 | .28 |

## Approach

### Add `spec.nics[]` to BMH

```go
type BMHNICSpec struct {
    MAC     string `json:"mac"`
    IP      string `json:"ip,omitempty"`
    Role    string `json:"role"` // "data", "mgmt", "storage"
    Network string `json:"network,omitempty"` // defaults to spec.network
}
```

Example BMH spec:
```json
{
  "spec": {
    "bootMACAddress": "ac:1f:6b:8b:11:5d",
    "network": "g10",
    "ip": "192.168.10.11",
    "nics": [
      {"mac": "f4:52:14:84:bc:c0", "ip": "192.168.10.21", "role": "data"}
    ]
  }
}
```

### Modify `syncBMHToNetwork()`

When syncing DHCP reservations:

**A port** (bootMACAddress) — existing behavior:
- Gateway: yes (from network config)
- NextServer: yes (PXE TFTP server)
- BootFile: yes (PXE boot file)

**B port** (spec.nics[]) — new:
- Gateway: **empty string** (microdns per-reservation override strips it)
- NextServer: **empty string** (no PXE)
- BootFile: **empty string** (no PXE)
- Hostname: `{hostname}-b` or `{hostname}-{role}`

### microdns Requirement

microdns already supports per-reservation `gateway`, `next_server`, `boot_file` overrides. Need to verify that setting `gateway: ""` in a reservation actually suppresses the pool default. If not, add a `no_gateway: true` field to the reservation.

## Files to Modify

- `pkg/provider/baremetalhost.go` — add NICSpec, extend syncBMHToNetwork()
- `pkg/provider/dns_seed.go` — networkReservationToDNS() may need updates
- `pkg/dns/client.go` — DHCPReservation struct (if new fields needed)

## Verification

1. Create BMH with `spec.nics` containing B port
2. Verify two DHCP reservations appear in network CRD
3. DHCP request from A port MAC → gets gateway + PXE options
4. DHCP request from B port MAC → gets IP only, no gateway, no PXE
5. `ip route` on booted server shows single default route via A port
