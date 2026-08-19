#!/usr/bin/env bash
# pull-and-deploy.sh — Deploy mkube-update to RouterOS from GHCR.
# No local Go compilation required. Only needs: crane, jq, ssh, scp, sftp.
#
# Usage:
#   hack/pull-and-deploy.sh [device] [image]
#   hack/pull-and-deploy.sh rose1.gw.lo ghcr.io/glennswest/mkube-update:edge

set -euo pipefail

DEVICE="${1:-rose1.gw.lo}"
IMAGE="${2:-ghcr.io/glennswest/mkube-update:edge}"
SSH_USER="${SSH_USER:-admin}"
SSH_OPTS="-o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
SSH="ssh ${SSH_OPTS} ${SSH_USER}@${DEVICE}"
# Extra flags for crane. A local registry (registry.gt.lo:5000) serves a
# self-signed cert, so deploying a locally-built image needs --insecure;
# GHCR does not. Set CRANE_OPTS="--insecure" for the former.
CRANE_OPTS="${CRANE_OPTS:-}"
PLATFORM="linux/arm64"

# ── Configuration ────────────────────────────────────────────────────────────
BRIDGE_NAME="bridge-gt"
CONTAINER_NAME="mkube-update-updater"
VETH_NAME="veth-mkube-update"
VETH_ADDR="192.168.200.3/24"
VETH_GW="192.168.200.1"
ROOT_DIR="/raid1/images/${CONTAINER_NAME}"
TARBALL_DIR="/raid1/tarballs"
VOLUME_DIR="/raid1/volumes"
REMOTE_TARBALL="${TARBALL_DIR}/mkube-update.tar"
DNS_SERVER="192.168.200.199"

# ── Preflight checks ────────────────────────────────────────────────────────
for cmd in crane jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not found in PATH" >&2
        exit 1
    fi
done

ros() {
    ${SSH} "$1" | tr -d '\r'
}

# Container state flags, from `/container/print detail`:
#   S stopped   N starting   R running   T stopping
#   E downloading/extracting   D deleting   F download/extract failed
#
# A freshly added container goes E -> R. It never passes through S, so waiting
# for S after `/container/add` waits forever — every such wait here ran to its
# full timeout, and the deploy then carried on regardless because the "done"
# markers below were printed unconditionally. That combination reported a
# successful deploy while the container had not been replaced at all.
wait_state() {
    local name="$1" target="$2" max="${3:-30}" i=0
    printf "  Waiting for %s -> %s " "$name" "$target"
    while [ $i -lt $max ]; do
        # TERSE, deliberately: plain `/container/print` puts the state flag and
        # the name on DIFFERENT lines, so matching "index + flag" against the
        # name line never succeeded and every wait here ran to timeout — the
        # stop was reported as failed while the container was still running,
        # and the remove/add that followed then failed for real.
        local line
        line=$(ros "/container/print terse" 2>/dev/null | grep "name=${name}" || true)
        if [ "$target" = "missing" ]; then
            if [ -z "$line" ]; then
                printf "done\n"; return 0
            fi
        elif echo "$line" | grep -qE "^[[:space:]]*[0-9]+[[:space:]]+${target}[[:space:]]"; then
            printf "done\n"; return 0
        fi
        printf "."; i=$((i + 1)); sleep 2
    done
    printf " timeout!\n"; return 1
}

echo "═══════════════════════════════════════════════════════════"
echo "  Deploying mkube-update to ${DEVICE}"
echo "  Image: ${IMAGE}"
echo "═══════════════════════════════════════════════════════════"
echo ""

# ── Step 1: Verify connectivity ──────────────────────────────────────────────
echo "▸ Checking device connectivity..."
if ! ${SSH} '/system/resource/print' &>/dev/null; then
    echo "  ✗ Cannot connect to ${SSH_USER}@${DEVICE}"
    exit 1
fi
echo "  ✓ Connected to ${DEVICE}"

# ── Step 2: Pull image and create docker-save tarball ────────────────────────
echo ""
echo "▸ Pulling image and creating docker-save tarball..."

WORK=$(mktemp -d)
trap "rm -rf ${WORK}" EXIT

# Export flat rootfs
echo "  Exporting rootfs from ${IMAGE}..."
crane export ${CRANE_OPTS} --platform "${PLATFORM}" "${IMAGE}" "${WORK}/rootfs.tar"

# Get image config for entrypoint/cmd
IMG_CONFIG=$(crane config ${CRANE_OPTS} --platform "${PLATFORM}" "${IMAGE}")
ENTRYPOINT=$(echo "${IMG_CONFIG}" | jq -r '(.config.Entrypoint // []) | join(" ")' 2>/dev/null || true)
CMD_VAL=$(echo "${IMG_CONFIG}" | jq -r '(.config.Cmd // []) | join(" ")' 2>/dev/null || true)
WORKDIR=$(echo "${IMG_CONFIG}" | jq -r '.config.WorkingDir // "/"' 2>/dev/null || true)
ENV_JSON=$(echo "${IMG_CONFIG}" | jq -c '.config.Env // []' 2>/dev/null || echo '[]')

echo "  Entrypoint: ${ENTRYPOINT:-<none>}"
echo "  Cmd: ${CMD_VAL:-<none>}"

# Compute layer SHA256
LAYER_SHA=$(shasum -a 256 "${WORK}/rootfs.tar" | awk '{print $1}')
LAYER_ID="${LAYER_SHA}"

# Create docker-save structure
LAYER_SAVE_DIR="${WORK}/${LAYER_ID}"
mkdir -p "${LAYER_SAVE_DIR}"
mv "${WORK}/rootfs.tar" "${LAYER_SAVE_DIR}/layer.tar"
echo "1.0" > "${LAYER_SAVE_DIR}/VERSION"

# Derive repo:tag
REPO_TAG=$(echo "${IMAGE}" | sed 's|.*/||' | sed 's/:/:/' )
REPO_NAME=$(echo "${REPO_TAG}" | cut -d: -f1)
TAG_NAME=$(echo "${REPO_TAG}" | cut -d: -f2)

# Build container config for JSON
ENTRYPOINT_JSON=$(echo "${IMG_CONFIG}" | jq -c '.config.Entrypoint // empty' 2>/dev/null || echo 'null')
CMD_JSON=$(echo "${IMG_CONFIG}" | jq -c '.config.Cmd // empty' 2>/dev/null || echo 'null')

# Build config object
CONFIG_CONTENT=$(jq -n \
    --arg arch "arm64" \
    --arg layer_sha "sha256:${LAYER_SHA}" \
    --argjson entrypoint "${ENTRYPOINT_JSON:-null}" \
    --argjson cmd "${CMD_JSON:-null}" \
    --arg workdir "${WORKDIR}" \
    --argjson env "${ENV_JSON}" \
    '{
        architecture: $arch,
        os: "linux",
        rootfs: { type: "layers", diff_ids: [$layer_sha] },
        config: (
            {}
            + (if $entrypoint then {Entrypoint: $entrypoint} else {} end)
            + (if $cmd then {Cmd: $cmd} else {} end)
            + (if $workdir != "/" and $workdir != "" then {WorkingDir: $workdir} else {} end)
            + (if ($env | length) > 0 then {Env: $env} else {} end)
        )
    }')

CONFIG_SHA=$(echo -n "${CONFIG_CONTENT}" | shasum -a 256 | awk '{print $1}')
echo "${CONFIG_CONTENT}" > "${WORK}/${CONFIG_SHA}.json"

# Layer json
cat > "${LAYER_SAVE_DIR}/json" <<EOF
{"id":"${LAYER_ID}","created":"1970-01-01T00:00:00Z"}
EOF

# manifest.json
cat > "${WORK}/manifest.json" <<EOF
[{"Config":"${CONFIG_SHA}.json","RepoTags":["${REPO_NAME}:${TAG_NAME}"],"Layers":["${LAYER_ID}/layer.tar"]}]
EOF

# repositories
cat > "${WORK}/repositories" <<EOF
{"${REPO_NAME}":{"${TAG_NAME}":"${LAYER_ID}"}}
EOF

# Build final docker-save tar
LOCAL_TARBALL="${WORK}/mkube-update.tar"
tar -C "${WORK}" -cf "${LOCAL_TARBALL}" \
    manifest.json \
    repositories \
    "${CONFIG_SHA}.json" \
    "${LAYER_ID}/layer.tar" \
    "${LAYER_ID}/VERSION" \
    "${LAYER_ID}/json"

TARBALL_SIZE=$(du -h "${LOCAL_TARBALL}" | cut -f1)
echo "  ✓ Tarball created (${TARBALL_SIZE})"

# ── Step 3: Verify bridge ───────────────────────────────────────────────────
echo ""
echo "▸ Verifying network bridge '${BRIDGE_NAME}'..."
BRIDGE_EXISTS=$(ros "/interface/bridge/print count-only where name=${BRIDGE_NAME}")
if [ "${BRIDGE_EXISTS}" = "0" ]; then
    echo "  ✗ Bridge '${BRIDGE_NAME}' does not exist."
    exit 1
fi
echo "  ✓ Bridge '${BRIDGE_NAME}' exists"

# ── Step 4: Create veth ─────────────────────────────────────────────────────
echo ""
echo "▸ Configuring veth '${VETH_NAME}'..."
ros "/interface/veth/add name=${VETH_NAME} address=${VETH_ADDR} gateway=${VETH_GW}" >/dev/null 2>&1 && echo "  ✓ Veth created" || echo "  ✓ Veth already exists"
ros "/interface/bridge/port/add bridge=${BRIDGE_NAME} interface=${VETH_NAME}" >/dev/null 2>&1 && echo "  ✓ Bridge port added" || echo "  ✓ Bridge port already configured"

# ── Step 5: Create volume directories ────────────────────────────────────────
echo ""
echo "▸ Creating volume directories..."
sftp ${SSH_OPTS} "${SSH_USER}@${DEVICE}" <<SFTP_EOF 2>/dev/null || true
-mkdir ${VOLUME_DIR}/${CONTAINER_NAME}
-mkdir ${VOLUME_DIR}/${CONTAINER_NAME}/config
-mkdir ${VOLUME_DIR}/${CONTAINER_NAME}/data
SFTP_EOF
echo "  ✓ Volume directories ready"

# ── Step 6: Upload config ───────────────────────────────────────────────────
echo ""
echo "▸ Uploading configuration..."
CONFIG_FILE="deploy/mkube-update-config.yaml"
if [ ! -f "${CONFIG_FILE}" ]; then
    echo "  ✗ Config file not found: ${CONFIG_FILE}"
    exit 1
fi
scp ${SSH_OPTS} "${CONFIG_FILE}" "${SSH_USER}@${DEVICE}:${VOLUME_DIR}/${CONTAINER_NAME}/config/config.yaml"
echo "  ✓ Config uploaded"

# ── Step 7: Create mount points ─────────────────────────────────────────────
echo ""
echo "▸ Creating container mounts..."
ros "/container/mounts/add list=${CONTAINER_NAME}.config src=/${VOLUME_DIR}/${CONTAINER_NAME}/config dst=/etc/mkube-update" 2>/dev/null || echo "  (config mount already exists)"
ros "/container/mounts/add list=${CONTAINER_NAME}.config src=/${VOLUME_DIR}/${CONTAINER_NAME}/data dst=/data" 2>/dev/null || echo "  (data mount already exists)"
echo "  ✓ Mounts configured"

# ── Step 8: Upload tarball ───────────────────────────────────────────────────
echo ""
echo "▸ Uploading tarball..."
echo "  Destination: ${REMOTE_TARBALL}"
scp ${SSH_OPTS} "${LOCAL_TARBALL}" "${SSH_USER}@${DEVICE}:${REMOTE_TARBALL}"
echo "  ✓ Upload complete"

# ── Step 9: Stop and remove existing container ───────────────────────────────
echo ""
echo "▸ Checking for existing container..."
EXISTING=$(ros "/container/print count-only where name=${CONTAINER_NAME}")
if [ -n "${EXISTING}" ] && [ "${EXISTING}" != "0" ]; then
    echo "  Stopping existing container..."
    ros "/container/stop [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    if ! wait_state "${CONTAINER_NAME}" "S"; then
        echo "  ✗ Container did not stop — refusing to continue." >&2
        echo "    Removing a running container fails, and the add that follows" >&2
        echo "    then fails on root-dir overlap, leaving nothing deployed." >&2
        exit 1
    fi
    echo "  Removing existing container..."
    ros "/container/remove [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    if ! wait_state "${CONTAINER_NAME}" "missing"; then
        echo "  ✗ Container still present after remove — refusing to continue." >&2
        exit 1
    fi
    echo "  ✓ Removed old container"
else
    echo "  No existing container found"
fi

# ── Step 10: Create and start container ──────────────────────────────────────
echo ""
echo "▸ Creating container '${CONTAINER_NAME}'..."
# RouterOS refuses to create a container whose root-dir still exists, and a
# removed container leaves its rootfs behind. Move it aside first.
ros "/file/remove [find name~\"${ROOT_DIR#/}\"]" 2>/dev/null || true

ADD_OUT=$(ros "/container/add file=${REMOTE_TARBALL} interface=${VETH_NAME} root-dir=${ROOT_DIR} name=${CONTAINER_NAME} start-on-boot=yes logging=yes dns=${DNS_SERVER} hostname=${CONTAINER_NAME} mountlists=${CONTAINER_NAME}.config" 2>&1 || true)
if echo "${ADD_OUT}" | grep -qi "failure\|error"; then
    echo "  ✗ Container add failed: ${ADD_OUT}" >&2
    exit 1
fi
if [ "$(ros "/container/print count-only where name=${CONTAINER_NAME}")" = "0" ]; then
    echo "  ✗ Container add reported nothing but the container does not exist" >&2
    exit 1
fi
echo "  ✓ Container created"

# E -> R: RouterOS extracts and, with start-on-boot, starts it itself. Waiting
# for S here is what hung this script for twenty minutes.
echo ""
echo "▸ Waiting for container to extract and run..."
if ! wait_state "${CONTAINER_NAME}" "R" 90; then
    echo "  Not running yet — starting it explicitly..."
    ros "/container/start [find name=${CONTAINER_NAME}]" 2>/dev/null || true
    if ! wait_state "${CONTAINER_NAME}" "R" 30; then
        echo "  ✗ Container never reached RUNNING" >&2
        exit 1
    fi
fi
echo "  ✓ Container running"

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
echo "  Network:   ${VETH_ADDR} on bridge ${BRIDGE_NAME}"
echo ""
echo "  mkube-update will now bootstrap mkube automatically."
echo ""
echo "  Monitor logs:"
echo "    ssh ${SSH_USER}@${DEVICE} '/log/print where topics~\"container\"'"
echo ""
echo "  Check status:"
echo "    ssh ${SSH_USER}@${DEVICE} '/container/print'"
echo "═══════════════════════════════════════════════════════════"
