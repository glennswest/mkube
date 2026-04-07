# Enhancement: stormd SSH via CloudID

## Problem

stormd's SSH server only supports password authentication (`password = "stormd"` in config). Every container using stormd as PID 1 requires typing a password to SSH in. There is no SSH key-based auth.

## Desired Behavior

stormd should fetch authorized SSH public keys from a CloudID instance, identified by a **tag**. This eliminates password prompts and centralizes key management across all stormd-supervised containers.

## Proposed Config Change

```toml
[ssh]
enabled = true
bind = "0.0.0.0:22"
host_key = "/var/stormd/host_key"
cloudid_url = "http://192.168.200.20:8090"
tag = "mkube"
```

- `cloudid_url` — base URL of the CloudID API
- `tag` — identifier for this container/host; stormd uses it to fetch the correct SSH public keys from CloudID

When both `cloudid_url` and `tag` are set, stormd should:

1. On startup (and periodically), fetch authorized keys from CloudID using the tag
2. Accept SSH connections authenticated by those keys (public key auth)
3. Fall back to password auth only if CloudID is unreachable and `password` is also set
4. Refresh keys periodically (e.g. every 60s) so key changes propagate without restart

## CloudID API Contract

stormd needs a CloudID endpoint that returns SSH public keys for a given tag. Suggested:

```
GET /api/v1/ssh-keys/{tag}
```

Response:
```json
{
  "tag": "mkube",
  "keys": [
    "ssh-ed25519 AAAA... user@host",
    "ssh-rsa AAAA... other@host"
  ]
}
```

If the tag has no keys, return `{"tag": "mkube", "keys": []}`.

## Example Configs After Change

### mkube container (deploy/stormd-config.toml)
```toml
[ssh]
enabled = true
bind = "0.0.0.0:22"
host_key = "/var/stormd/host_key"
cloudid_url = "http://192.168.200.20:8090"
tag = "mkube"
```

### pvc-test container (cmd/pvc-test/stormd-config.toml)
```toml
[ssh]
enabled = true
bind = "0.0.0.0:22"
host_key = "/var/stormd/host_key"
cloudid_url = "http://192.168.200.20:8090"
tag = "pvc-test"
```

## Scope

- **stormd**: Add CloudID SSH key fetching, public key auth, periodic refresh
- **CloudID**: Add `/api/v1/ssh-keys/{tag}` endpoint (or reuse templates with image_type=ssh)
- **mkube**: Update stormd-config.toml files to use `cloudid_url` + `tag` instead of `password`
