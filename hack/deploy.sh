#!/usr/bin/env bash
# deploy.sh — Build, upload, and configure mikrotik-kube on a RouterOS device.
#
# Usage:
#   ./hack/deploy.sh <device> <tarball>
#   ./hack/deploy.sh rose1.gw.lo dist/mikrotik-kube-dev-arm64.tar
#
# Or via Makefile:
#   make deploy                           # defaults to rose1.gw.lo
#   make deploy DEVICE=192.168.1.88       # custom device

set -euo pipefail

DEVICE="${1:?Usage: deploy.sh <device> <tarball>}"
TARBALL="${2:?Usage: deploy.sh <device> <tarball>}"
SSH_USER="${SSH_USER:-admin}"
SSH="ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new ${SSH_USER}@${DEVICE}"

# ── Configuration ────────────────────────────────────────────────────────────
BRIDGE_NAME="containers"
BRIDGE_ADDR="192.168.200.1/24"
SUBNET="192.168.200.0/24"
MGMT_VETH="veth-mkube"
MGMT_IP="192.168.200.2/24"
MGMT_GW="192.168.200.1"
CONTAINER_NAME="mikrotik-kube"
ROOT_DIR="/raid1/images/${CONTAINER_NAME}"
TARBALL_DIR="/raid1/tarballs"
CACHE_DIR="/raid1/cache"
VOLUME_DIR="/raid1/volumes"
REMOTE_TARBALL="${TARBALL_DIR}/${CONTAINER_NAME}.tar"

echo "═══════════════════════════════════════════════════════════"
echo "  Deploying mikrotik-kube to ${DEVICE}"
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

# ── Step 3: Create bridge if needed ──────────────────────────────────────────
echo ""
echo "▸ Configuring network bridge '${BRIDGE_NAME}'..."

BRIDGE_EXISTS=$(${SSH} "/interface/bridge/print count-only where name=${BRIDGE_NAME}" 2>/dev/null)
if [ "${BRIDGE_EXISTS}" = "0" ]; then
    echo "  Creating bridge ${BRIDGE_NAME}..."
    ${SSH} "/interface/bridge/add name=${BRIDGE_NAME} comment=\"Managed by mikrotik-kube (${SUBNET})\""
    echo "  ✓ Bridge created"
else
    echo "  ✓ Bridge already exists"
fi

# Add IP to bridge if not present
ADDR_EXISTS=$(${SSH} "/ip/address/print count-only where interface=${BRIDGE_NAME} address=${BRIDGE_ADDR}" 2>/dev/null)
if [ "${ADDR_EXISTS}" = "0" ]; then
    echo "  Adding ${BRIDGE_ADDR} to ${BRIDGE_NAME}..."
    ${SSH} "/ip/address/add address=${BRIDGE_ADDR} interface=${BRIDGE_NAME} comment=\"mikrotik-kube gateway\""
    echo "  ✓ Address added"
else
    echo "  ✓ Address already configured"
fi

# ── Step 4: NAT masquerade for container subnet ─────────────────────────────
echo ""
echo "▸ Configuring NAT..."

NAT_EXISTS=$(${SSH} "/ip/firewall/nat/print count-only where chain=srcnat src-address=${SUBNET} action=masquerade" 2>/dev/null)
if [ "${NAT_EXISTS}" = "0" ]; then
    echo "  Adding masquerade rule for ${SUBNET}..."
    ${SSH} "/ip/firewall/nat/add chain=srcnat src-address=${SUBNET} action=masquerade comment=\"mikrotik-kube containers\""
    echo "  ✓ NAT rule added"
else
    echo "  ✓ NAT rule already exists"
fi

# ── Step 5: Create management veth ───────────────────────────────────────────
echo ""
echo "▸ Configuring management veth '${MGMT_VETH}'..."

VETH_EXISTS=$(${SSH} "/interface/veth/print count-only where name=${MGMT_VETH}" 2>/dev/null)
if [ "${VETH_EXISTS}" = "0" ]; then
    echo "  Creating veth ${MGMT_VETH} (${MGMT_IP})..."
    ${SSH} "/interface/veth/add name=${MGMT_VETH} address=${MGMT_IP} gateway=${MGMT_GW}"
    echo "  ✓ Veth created"
else
    echo "  ✓ Veth already exists"
fi

# Add veth to bridge if not already a port
PORT_EXISTS=$(${SSH} "/interface/bridge/port/print count-only where bridge=${BRIDGE_NAME} interface=${MGMT_VETH}" 2>/dev/null)
if [ "${PORT_EXISTS}" = "0" ]; then
    echo "  Adding ${MGMT_VETH} to bridge ${BRIDGE_NAME}..."
    ${SSH} "/interface/bridge/port/add bridge=${BRIDGE_NAME} interface=${MGMT_VETH}"
    echo "  ✓ Bridge port added"
else
    echo "  ✓ Bridge port already configured"
fi

# ── Step 6: Upload tarball ───────────────────────────────────────────────────
echo ""
echo "▸ Uploading tarball..."

TARBALL_SIZE=$(du -h "${TARBALL}" | cut -f1)
echo "  Source: ${TARBALL} (${TARBALL_SIZE})"
echo "  Destination: ${REMOTE_TARBALL}"

scp -o ConnectTimeout=10 "${TARBALL}" "${SSH_USER}@${DEVICE}:${REMOTE_TARBALL}"
echo "  ✓ Upload complete"

# ── Step 7: Upload config ────────────────────────────────────────────────────
echo ""
echo "▸ Uploading configuration..."

# Check for device-specific config, fall back to default
CONFIG_FILE="deploy/rose1-config.yaml"
if [ ! -f "${CONFIG_FILE}" ]; then
    CONFIG_FILE="deploy/config.yaml"
fi

scp -o ConnectTimeout=10 "${CONFIG_FILE}" "${SSH_USER}@${DEVICE}:/raid1/volumes/${CONTAINER_NAME}/config.yaml" 2>/dev/null || {
    # Directory may not exist yet — that's OK, container will create it
    echo "  Config will be mounted after first container start"
}
echo "  ✓ Config uploaded"

# ── Step 8: Stop and remove existing container if present ────────────────────
echo ""
echo "▸ Checking for existing container..."

EXISTING=$(${SSH} "/container/print count-only where name=${CONTAINER_NAME}" 2>/dev/null)
if [ "${EXISTING}" != "0" ]; then
    echo "  Stopping existing container..."
    ${SSH} "/container/stop [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    sleep 3
    echo "  Removing existing container..."
    ${SSH} "/container/remove [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    sleep 2
    echo "  ✓ Removed old container"
else
    echo "  No existing container found"
fi

# ── Step 9: Create and start the container ───────────────────────────────────
echo ""
echo "▸ Creating container '${CONTAINER_NAME}'..."

${SSH} "/container/add \
    file=${REMOTE_TARBALL} \
    name=${CONTAINER_NAME} \
    interface=${MGMT_VETH} \
    root-dir=${ROOT_DIR} \
    mounts=raid1/volumes/${CONTAINER_NAME}/config.yaml:/etc/mikrotik-kube/config.yaml \
    logging=yes \
    start-on-boot=yes \
    hostname=${CONTAINER_NAME} \
    dns=8.8.8.8"

echo "  ✓ Container created"

echo ""
echo "▸ Waiting for container to extract..."
sleep 5

echo "▸ Starting container..."
${SSH} "/container/start [find name=${CONTAINER_NAME}]"
echo "  ✓ Container started"

# ── Step 10: Verify ──────────────────────────────────────────────────────────
echo ""
echo "▸ Verifying deployment..."
sleep 3

${SSH} "/container/print where name=${CONTAINER_NAME}"

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  Deployment complete!"
echo ""
echo "  Device:    ${DEVICE}"
echo "  Container: ${CONTAINER_NAME}"
echo "  Network:   ${MGMT_IP} on bridge ${BRIDGE_NAME}"
echo "  REST API:  https://${MGMT_GW}/rest"
echo ""
echo "  Monitor logs:"
echo "    ssh ${SSH_USER}@${DEVICE} '/log/print where topics~\"container\"'"
echo ""
echo "  Check status:"
echo "    ssh ${SSH_USER}@${DEVICE} '/container/print where name=${CONTAINER_NAME}'"
echo "═══════════════════════════════════════════════════════════"
