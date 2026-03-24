# Enhancement: Agent BootConfig & Service Fixes

## Problem

The `coreos-agent` bootconfig produces a systemd service that is missing critical mounts, causing:

1. **No podman socket** — agent cannot manage build containers, jobs fail with `dial unix /run/podman/podman.sock: connect: no such file or directory`
2. **No persistent image storage** — container images stored on OS disk (`/var/home/core/.local/share/containers/storage`) instead of data disk, wiped on every reboot
3. **Stale service files on existing hosts** — ignition only applies on first boot, so hosts provisioned before bootconfig updates keep the old broken service

## Root Cause

The `coreos-agent` bootconfig's `mkube-agent.service` ExecStart is missing:
- `-v /run/podman/podman.sock:/run/podman/podman.sock` (socket mount)
- `-e CONTAINER_HOST=unix:///run/podman/podman.sock` (socket env)

These were present in the manually-modified service on server1 but never added to the bootconfig.

Additionally, server1 had manually-added `ExecStartPre` lines that pre-pulled ~14 GB of build images (`rawhidedev:latest`, `fedoradev:latest`) before the agent could start, blocking heartbeats for 10+ minutes and causing provisioning timeouts.

## Required Changes

### 1. Patch `coreos-agent` bootconfig

Update the `mkube-agent.service` unit in the ignition JSON to add podman socket mount:

```
ExecStart=/usr/bin/podman run --name mkube-agent --rm --network host --privileged \
  -v /var/data:/data \
  -v /var/data/agent-storage:/var/lib/containers \
  -v /var/data/tmp:/var/tmp \
  -v /run/podman/podman.sock:/run/podman/podman.sock \
  -e TMPDIR=/data/tmp \
  -e MKUBE_API=http://192.168.200.2:8082 \
  -e CONTAINER_HOST=unix:///run/podman/podman.sock \
  --pull=always registry.gt.lo:5000/mkube-agent:edge
```

### 2. Job retry button in console UI

When a job fails (phase: Failed, TimedOut, or Completed), add a "Retry" button that:
- Deletes the failed job
- Creates a new job with the same spec
- Saves the manual delete+resubmit dance

### 3. Consider service update mechanism

Since ignition is first-boot only, hosts provisioned with older bootconfigs keep stale service files forever. Options:
- Agent self-update: agent fetches current bootconfig from mkube API and updates its own service file on startup
- Re-provision: wipe and re-PXE the host (destructive, loses image cache)
- Accept drift: manually SSH and fix (current approach, doesn't scale)

## Verification

- After bootconfig patch: provision a new host and verify agent starts with socket mount, images persist across reboot
- After retry button: fail a job, click retry in UI, verify new job starts
