# BootConfig CRD Specification

## Overview

Store boot config files (ignition, kickstart, etc.) in mkube and serve them over HTTP by name. Booting servers fetch their config via `GET /api/v1/bootconfigs/{name}/config`.

## CRD

```yaml
apiVersion: v1
kind: BootConfig
metadata:
  name: coreos-builder       # fetch via /api/v1/bootconfigs/coreos-builder/config
spec:
  config: |                   # the raw file content (ignition JSON, butane YAML, kickstart, etc.)
    {
      "ignition": { "version": "3.4.0" },
      ...
    }
status:
  phase: Ready                # Ready or Error
  size: 4096                  # bytes
```

## Types

```go
type BootConfig struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec              BootConfigSpec   `json:"spec"`
    Status            BootConfigStatus `json:"status,omitempty"`
}

type BootConfigSpec struct {
    Config string `json:"config"` // raw config file content
}

type BootConfigStatus struct {
    Phase string `json:"phase"`          // Ready, Error
    Size  int64  `json:"size,omitempty"` // bytes
}

type BootConfigList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata"`
    Items           []BootConfig `json:"items"`
}
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/bootconfigs` | List all configs |
| GET | `/api/v1/bootconfigs/{name}` | Get config object (JSON with metadata) |
| GET | `/api/v1/bootconfigs/{name}/config` | Serve raw config file content |
| POST | `/api/v1/bootconfigs` | Create a config |
| PUT | `/api/v1/bootconfigs/{name}` | Update a config |
| DELETE | `/api/v1/bootconfigs/{name}` | Delete a config |

The `/config` sub-resource returns the raw file content with `Content-Type: text/plain`. No JSON wrapping, no metadata — just the file. This is what booting servers fetch.

## Storage

- **Bucket:** `BOOTCONFIGS` in NATS KV store
- **Key:** `{name}`
- **Value:** JSON-serialized `BootConfig`

Cluster-scoped (no namespace). Configs are global — same config can be referenced by any server on any network.

## Usage

### Create a config

```bash
curl -X POST http://192.168.200.2:8082/api/v1/bootconfigs \
  -H 'Content-Type: application/json' \
  -d '{
    "metadata": { "name": "coreos-builder" },
    "spec": { "config": "{\"ignition\":{\"version\":\"3.4.0\"}, ...}" }
  }'
```

### Fetch raw config (what booting servers do)

```bash
curl http://192.168.200.2:8082/api/v1/bootconfigs/coreos-builder/config
```

### ISO kernel args

CoreOS live ISO GRUB config:
```
ignition.config.url=http://192.168.200.2:8082/api/v1/bootconfigs/coreos-builder/config
```

Fedora netinstall ISO GRUB config:
```
inst.ks=http://192.168.200.2:8082/api/v1/bootconfigs/fedora-server/config
```

### iPXE sanboot

PXE manager generates:
```
#!ipxe
sanboot iscsi:192.168.10.1::::iqn.2000-02.com.mikrotik:cdrom-coreos-live
```

The ISO boots, fetches its config by name from the URL baked into its GRUB config. Different ISOs point to different config names.

## Example Configs

### coreos-builder (live ignition — installs to disk then reboots)

```json
{
  "ignition": { "version": "3.4.0" },
  "systemd": {
    "units": [{
      "name": "coreos-installer.service",
      "enabled": true,
      "contents": "[Unit]\nDescription=Install CoreOS to disk\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=oneshot\nExecStart=/usr/bin/coreos-installer install /dev/sda --ignition-url http://192.168.200.2:8082/api/v1/bootconfigs/builder-target/config --insecure-ignition --append-karg console=tty0 --append-karg console=ttyS0,115200n8 --append-karg console=ttyS1,115200n8\nExecStartPost=/usr/bin/systemctl reboot\nStandardOutput=journal+console\nStandardError=journal+console\n\n[Install]\nWantedBy=multi-user.target\n"
    }]
  }
}
```

### builder-target (applied to installed system)

The full `builder.bu` content (SSH keys, podman, cockpit, buildah/skopeo, serial consoles, firewall).

### fedora-server (kickstart)

```
install
url --url=https://download.fedoraproject.org/pub/fedora/linux/releases/43/Everything/x86_64/os/
keyboard us
lang en_US.UTF-8
timezone UTC
rootpw --plaintext admin
sshkey --username=root "ssh-rsa AAAA..."
bootloader --location=mbr --append="console=tty0 console=ttyS0,115200n8 console=ttyS1,115200n8"
clearpart --all --initlabel
autopart
reboot
%packages
@^server-product-environment
%end
```

## No Changes to Other CRDs

ISCSICdrom and BareMetalHost are unchanged. This CRD is standalone — it just stores and serves files.
