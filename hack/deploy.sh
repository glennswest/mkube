#!/usr/bin/env bash
# deploy.sh — Build, upload, and configure microkube on a RouterOS device.
#
# Usage:
#   ./hack/deploy.sh <device> <tarball>
#   ./hack/deploy.sh rose1.gw.lo dist/microkube-arm64.tar.gz
#
# Or via Makefile:
#   make deploy                           # defaults to rose1.gw.lo
#   make deploy DEVICE=192.168.1.88       # custom device

set -euo pipefail

DEVICE="${1:?Usage: deploy.sh <device> <tarball>}"
TARBALL="${2:?Usage: deploy.sh <device> <tarball>}"
SSH_USER="${SSH_USER:-admin}"
SSH_OPTS="-o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
SSH="ssh ${SSH_OPTS} ${SSH_USER}@${DEVICE}"

# ── Configuration ────────────────────────────────────────────────────────────
BRIDGE_NAME="bridge-gt"
BRIDGE_ADDR="192.168.200.1/24"
MGMT_VETH="veth-mkube"
MGMT_IP="192.168.200.2/24"
MGMT_GW="192.168.200.1"
CONTAINER_NAME="kube.gt.lo"
ROOT_DIR="/raid1/images/${CONTAINER_NAME}"
TARBALL_DIR="/raid1/tarballs"
VOLUME_DIR="/raid1/volumes"
REMOTE_TARBALL="${TARBALL_DIR}/${CONTAINER_NAME}.tar"
DNS_SERVER="192.168.200.199"

ros() {
    ${SSH} "$1" | tr -d '\r'
}

wait_state() {
    local name="$1" target="$2" max=30 i=0
    printf "  Waiting for %s -> %s " "$name" "$target"
    while [ $i -lt $max ]; do
        local output
        output=$(ros "/container/print" 2>/dev/null || true)
        if [ "$target" = "missing" ]; then
            if ! echo "$output" | grep -q "$name"; then
                printf "done\n"; return 0
            fi
        elif echo "$output" | grep "$name" | grep -qE "^\s*[0-9]+\s+${target}\s"; then
            printf "done\n"; return 0
        fi
        printf "."; i=$((i + 1)); sleep 2
    done
    printf " timeout!\n"; return 1
}

echo "═══════════════════════════════════════════════════════════"
echo "  Deploying microkube to ${DEVICE}"
echo "  Tarball: ${TARBALL}"
echo "═══════════════════════════════════════════════════════════"
echo ""

# ── Step 1: Verify device connectivity ───────────────────────────────────────
echo "▸ Checking device connectivity..."
if ! ${SSH} '/system/resource/print' &>/dev/null; then
    echo "  ✗ Cannot connect to ${SSH_USER}@${DEVICE}"
    exit 1
fi
echo "  ✓ Connected to ${DEVICE}"

ARCH=$(${SSH} '/system/resource/print' 2>/dev/null | grep architecture-name | awk '{print $2}')
echo "  Architecture: ${ARCH}"

# ── Step 2: Verify container mode ────────────────────────────────────────────
echo ""
echo "▸ Checking container mode..."
CONTAINER_MODE=$(${SSH} '/system/device-mode/print' 2>/dev/null | grep 'container:' | awk '{print $2}')
if [ "${CONTAINER_MODE}" != "yes" ]; then
    echo "  ✗ Container mode is not enabled. Enable with:"
    echo "    /system/device-mode/update container=yes"
    exit 1
fi
echo "  ✓ Container mode enabled"

# ── Step 3: Verify bridge exists ─────────────────────────────────────────────
echo ""
echo "▸ Verifying network bridge '${BRIDGE_NAME}'..."

# bridge-gt and its .1 address are managed by RouterOS — do not create or modify.
BRIDGE_EXISTS=$(ros "/interface/bridge/print count-only where name=${BRIDGE_NAME}")
if [ "${BRIDGE_EXISTS}" = "0" ]; then
    echo "  ✗ Bridge '${BRIDGE_NAME}' does not exist. Create it in RouterOS first."
    exit 1
fi
echo "  ✓ Bridge '${BRIDGE_NAME}' exists"

# ── Step 4: Create management veth ───────────────────────────────────────────
echo ""
echo "▸ Configuring management veth '${MGMT_VETH}'..."

ros "/interface/veth/add name=${MGMT_VETH} address=${MGMT_IP} gateway=${MGMT_GW}" >/dev/null 2>&1 && echo "  ✓ Veth created" || echo "  ✓ Veth already exists"

# Add veth to bridge
ros "/interface/bridge/port/add bridge=${BRIDGE_NAME} interface=${MGMT_VETH}" >/dev/null 2>&1 && echo "  ✓ Bridge port added" || echo "  ✓ Bridge port already configured"

# ── Step 5: Create volume directories ─────────────────────────────────────────
echo ""
echo "▸ Creating volume directories..."

# RouterOS SCP can't create intermediate dirs — use sftp batch mode
sftp ${SSH_OPTS} "${SSH_USER}@${DEVICE}" <<SFTP_EOF 2>/dev/null || true
-mkdir ${VOLUME_DIR}/${CONTAINER_NAME}
-mkdir ${VOLUME_DIR}/${CONTAINER_NAME}/config
SFTP_EOF

echo "  ✓ Volume directories ready"

# ── Step 6: Upload config ────────────────────────────────────────────────────
echo ""
echo "▸ Uploading configuration..."

CONFIG_FILE="deploy/rose1-config.yaml"
if [ ! -f "${CONFIG_FILE}" ]; then
    CONFIG_FILE="deploy/config.yaml"
fi

scp ${SSH_OPTS} "${CONFIG_FILE}" "${SSH_USER}@${DEVICE}:${VOLUME_DIR}/${CONTAINER_NAME}/config/config.yaml"

# Upload boot-order manifest if it exists
BOOT_ORDER="deploy/boot-order.yaml"
if [ -f "${BOOT_ORDER}" ]; then
    scp ${SSH_OPTS} "${BOOT_ORDER}" "${SSH_USER}@${DEVICE}:${VOLUME_DIR}/${CONTAINER_NAME}/config/boot-order.yaml"
    echo "  ✓ Config + boot-order uploaded"
else
    echo "  ✓ Config uploaded (no boot-order.yaml)"
fi

# ── Step 7: Create mount points ──────────────────────────────────────────────
echo ""
echo "▸ Creating container mounts..."

ros "/container/mounts/add list=${CONTAINER_NAME}.config src=/${VOLUME_DIR}/${CONTAINER_NAME}/config dst=/etc/microkube" 2>/dev/null || echo "  (config mount already exists)"

echo "  ✓ Mounts configured"

# ── Step 8: Upload tarball ───────────────────────────────────────────────────
echo ""
echo "▸ Uploading tarball..."

TARBALL_SIZE=$(du -h "${TARBALL}" | cut -f1)
echo "  Source: ${TARBALL} (${TARBALL_SIZE})"
echo "  Destination: ${REMOTE_TARBALL}"

scp ${SSH_OPTS} "${TARBALL}" "${SSH_USER}@${DEVICE}:${REMOTE_TARBALL}"
echo "  ✓ Upload complete"

# ── Step 9: Stop and remove existing container if present ────────────────────
echo ""
echo "▸ Checking for existing container..."

EXISTING=$(ros "/container/print count-only where name=${CONTAINER_NAME}")
if [ -n "${EXISTING}" ] && [ "${EXISTING}" != "0" ]; then
    echo "  Stopping existing container..."
    ros "/container/stop [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    wait_state "${CONTAINER_NAME}" "S" 2>/dev/null || true
    echo "  Removing existing container..."
    ros "/container/remove [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    wait_state "${CONTAINER_NAME}" "missing" 2>/dev/null || true
    echo "  ✓ Removed old container"
else
    echo "  No existing container found"
fi

# ── Step 10: Create and start the container ──────────────────────────────────
echo ""
echo "▸ Creating container '${CONTAINER_NAME}'..."

ros "/container/add file=${REMOTE_TARBALL} interface=${MGMT_VETH} root-dir=${ROOT_DIR} name=${CONTAINER_NAME} start-on-boot=yes logging=yes dns=${DNS_SERVER} hostname=${CONTAINER_NAME} mountlists=${CONTAINER_NAME}.config"

echo "  ✓ Container created"

echo ""
echo "▸ Waiting for container to extract..."
wait_state "${CONTAINER_NAME}" "S"

echo "▸ Starting container..."
ros "/container/start [find name=${CONTAINER_NAME}]"
echo "  ✓ Container started"

# ── Step 11: Verify ──────────────────────────────────────────────────────────
echo ""
echo "▸ Verifying deployment..."
sleep 3

ros "/container/print where name=${CONTAINER_NAME}"

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  Deployment complete!"
echo ""
echo "  Device:    ${DEVICE}"
echo "  Container: ${CONTAINER_NAME}"
echo "  Network:   ${MGMT_IP} on bridge ${BRIDGE_NAME}"
echo "  DNS:       ${DNS_SERVER} (MicroDNS gt.lo)"
echo "  REST API:  https://${MGMT_GW}/rest"
echo ""
echo "  Monitor logs:"
echo "    ssh ${SSH_USER}@${DEVICE} '/log/print where topics~\"container\"'"
echo ""
echo "  Check status:"
echo "    ssh ${SSH_USER}@${DEVICE} '/container/print where name=${CONTAINER_NAME}'"
echo "═══════════════════════════════════════════════════════════"
