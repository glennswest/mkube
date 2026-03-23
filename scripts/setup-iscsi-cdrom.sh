#!/bin/bash
# setup-iscsi-cdrom.sh — Create an iSCSI CDROM and upload an ISO file
# Usage: ./setup-iscsi-cdrom.sh <name> <iso-file> [description]
#
# Cleans up any existing CDROM with the same name first.

set -euo pipefail

MKUBE_API="${MKUBE_API:-http://192.168.200.2:8082}"
NAME="${1:?Usage: $0 <name> <iso-file> [description]}"
ISO_FILE="${2:?Usage: $0 <name> <iso-file> [description]}"
DESCRIPTION="${3:-}"

if [ ! -f "$ISO_FILE" ]; then
    echo "ERROR: ISO file not found: $ISO_FILE"
    exit 1
fi

ISO_SIZE=$(stat -f%z "$ISO_FILE" 2>/dev/null || stat -c%s "$ISO_FILE" 2>/dev/null)
echo "==> Setting up iSCSI CDROM: $NAME"
echo "    ISO: $ISO_FILE ($(echo "$ISO_SIZE" | awk '{printf "%.1f MiB", $1/1048576}'))"

# --- Cleanup existing CDROM with same name ---
echo "==> Checking for existing CDROM '$NAME'..."
EXISTING=$(curl -sf "${MKUBE_API}/api/v1/iscsi-cdroms/${NAME}" 2>/dev/null || true)

if [ -n "$EXISTING" ]; then
    echo "    Found existing CDROM, cleaning up..."

    # Unsubscribe all subscribers first
    SUBSCRIBERS=$(echo "$EXISTING" | python3 -c "
import json,sys
d=json.load(sys.stdin)
subs=d.get('status',{}).get('subscribers',[])
for s in (subs or []):
    print(s['name'])
" 2>/dev/null || true)

    for SUB in $SUBSCRIBERS; do
        echo "    Unsubscribing: $SUB"
        curl -sf -X POST "${MKUBE_API}/api/v1/iscsi-cdroms/${NAME}/unsubscribe" \
            -H 'Content-Type: application/json' \
            -d "{\"name\":\"${SUB}\"}" > /dev/null
    done

    # Delete CDROM and ISO file (default behavior)
    echo "    Deleting CDROM and ISO file..."
    curl -sf -X DELETE "${MKUBE_API}/api/v1/iscsi-cdroms/${NAME}" > /dev/null
    echo "    Cleanup complete."
    sleep 1
fi

# --- Create new CDROM ---
echo "==> Creating iSCSI CDROM '$NAME'..."
ISO_BASENAME=$(basename "$ISO_FILE")

CREATE_BODY=$(cat <<EOF
{
    "metadata": {"name": "$NAME"},
    "spec": {
        "isoFile": "$ISO_BASENAME",
        "description": "${DESCRIPTION:-$ISO_BASENAME}",
        "readOnly": true
    }
}
EOF
)

CREATED=$(curl -sf -X POST "${MKUBE_API}/api/v1/iscsi-cdroms" \
    -H 'Content-Type: application/json' \
    -d "$CREATE_BODY")

PHASE=$(echo "$CREATED" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',{}).get('phase','unknown'))" 2>/dev/null || echo "unknown")
echo "    Created. Phase: $PHASE"

# --- Upload ISO ---
echo "==> Uploading ISO file (this may take a while)..."
UPLOAD_RESULT=$(curl -sf -X POST "${MKUBE_API}/api/v1/iscsi-cdroms/${NAME}/upload" \
    -F "iso=@${ISO_FILE}")

PHASE=$(echo "$UPLOAD_RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status',{}).get('phase','unknown'))" 2>/dev/null || echo "unknown")
TARGET_IQN=$(echo "$UPLOAD_RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status',{}).get('targetIQN',''))" 2>/dev/null || echo "")
PORTAL_IP=$(echo "$UPLOAD_RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status',{}).get('portalIP',''))" 2>/dev/null || echo "")
PORTAL_PORT=$(echo "$UPLOAD_RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status',{}).get('portalPort',3260))" 2>/dev/null || echo "3260")

echo ""
echo "==> iSCSI CDROM '$NAME' is ${PHASE}"
echo "    Target IQN:  $TARGET_IQN"
echo "    Portal:      ${PORTAL_IP}:${PORTAL_PORT}"
echo ""
echo "To subscribe a host:"
echo "  curl -X POST ${MKUBE_API}/api/v1/iscsi-cdroms/${NAME}/subscribe \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"name\":\"server1\"}'"
