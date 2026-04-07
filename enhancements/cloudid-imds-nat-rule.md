# CloudID: Self-manage IMDS DST-NAT Rule

## Problem

The `fcos-cloudid` ISO fetches ignition config from the standard AWS IMDS endpoint at `169.254.169.254:80`. CloudID serves this at `192.168.200.20:8090` on the gt network, but bare metal servers on g10 (`192.168.10.x`) can't reach cloudid without a DST-NAT rule on the router.

This rule was manually added and got lost (router reboot, config reset, etc.), causing server1 to boot-loop for ~20 minutes trying to reach IMDS with `i/o timeout`.

## Required Behavior

On startup and periodically (every 60s), cloudid should:

1. **Check** if a RouterOS DST-NAT rule exists:
   - chain: `dstnat`
   - dst-address: `169.254.169.254`
   - protocol: `tcp`
   - dst-port: `80`
   - action: `dst-nat`
   - to-addresses: `<cloudid's own IP>` (e.g. `192.168.200.20`)
   - to-ports: `<cloudid's IMDS listen port>` (e.g. `8090`)

2. **Create** the rule if missing, with comment `IMDS redirect to cloudid (managed)`.

3. **Update** the rule if `to-addresses` or `to-ports` don't match (e.g. cloudid IP changed).

4. **Log** when the rule is created or updated. Silent on routine checks where rule already exists.

## Configuration

CloudID config should accept RouterOS connection info:

```toml
[routeros]
rest_url = "http://192.168.200.1/rest"
user = "mkube"
password = "mkube-rest"
```

If `[routeros]` is not configured, skip NAT management silently (for dev/test environments).

## RouterOS REST API

```bash
# List NAT rules
GET /rest/ip/firewall/nat

# Create rule
PUT /rest/ip/firewall/nat
Content-Type: application/json
{
  "chain": "dstnat",
  "dst-address": "169.254.169.254",
  "protocol": "tcp",
  "dst-port": "80",
  "action": "dst-nat",
  "to-addresses": "192.168.200.20",
  "to-ports": "8090",
  "comment": "IMDS redirect to cloudid (managed)"
}

# Update existing rule
PATCH /rest/ip/firewall/nat/<rule-id>
```

## Impact

Without this rule, any bare metal server using `fcos-cloudid` will boot-loop indefinitely waiting for IMDS. This is a critical dependency for the job runner infrastructure.
