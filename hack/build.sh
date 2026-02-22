#!/usr/bin/env bash
# build.sh — Build mkube and export as a RouterOS-compatible tarball.
#
# Usage:
#   ./hack/build.sh                    # Build for arm64 (most MikroTik devices)
#   ./hack/build.sh amd64              # Build for x86_64 (CHR / x86 devices)
#
# This script is called by the Makefile `tarball` target. For deployment,
# use `make deploy` instead which calls hack/deploy.sh.

set -euo pipefail

ARCH="${1:-arm64}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
IMAGE_NAME="mkube"
IMAGE_TAG="${VERSION}-${ARCH}"
TARBALL_NAME="${IMAGE_NAME}-${IMAGE_TAG}.tar"

echo "═══════════════════════════════════════════════════════════"
echo "  Building mkube"
echo "  Version: ${VERSION}  Arch: ${ARCH}  Commit: ${COMMIT}"
echo "═══════════════════════════════════════════════════════════"

# ── Step 1: Build the container image ────────────────────────────────────────
echo ""
echo "▸ Building container image..."
docker buildx build \
    --platform "linux/${ARCH}" \
    --build-arg VERSION="${VERSION}" \
    --build-arg COMMIT="${COMMIT}" \
    --build-arg TARGETARCH="${ARCH}" \
    -t "${IMAGE_NAME}:${IMAGE_TAG}" \
    --load \
    .

# ── Step 2: Export as a flat rootfs tarball ──────────────────────────────────
# RouterOS expects a tar of the root filesystem, NOT an OCI image tar.
# We create a container, export its filesystem, and clean up.
echo ""
echo "▸ Exporting rootfs tarball..."

mkdir -p dist
CONTAINER_ID=$(docker create "${IMAGE_NAME}:${IMAGE_TAG}")
docker export "${CONTAINER_ID}" -o "dist/${TARBALL_NAME}"
docker rm "${CONTAINER_ID}" > /dev/null

echo "  → dist/${TARBALL_NAME} ($(du -h "dist/${TARBALL_NAME}" | cut -f1))"

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  Build complete: dist/${TARBALL_NAME}"
echo "═══════════════════════════════════════════════════════════"
