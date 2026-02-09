# Multi-stage build for mikrotik-kube
# Produces a minimal static binary suitable for RouterOS containers.
#
# RouterOS container constraints:
#   - Expects a tar archive of a rootfs
#   - Limited resources (ARM64 or x86_64 depending on device)
#   - No systemd, no init system

# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build static binary (CGO_ENABLED=0 for scratch compatibility)
ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG VERSION=dev
ARG COMMIT=none

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /mikrotik-kube \
    ./cmd/mikrotik-kube/

# ── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache \
    ca-certificates \
    curl \
    tini

# Create non-root user
RUN addgroup -S mikrotik-kube && adduser -S -G mikrotik-kube mikrotik-kube

# Create data directories
RUN mkdir -p /etc/mikrotik-kube /data/registry /data/cache /data/volumes \
    && chown -R mikrotik-kube:mikrotik-kube /data

COPY --from=builder /mikrotik-kube /usr/local/bin/mikrotik-kube

# Default config
COPY deploy/config.yaml /etc/mikrotik-kube/config.yaml

EXPOSE 5000 8080

# Use tini as PID 1 (proper signal handling in containers)
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["mikrotik-kube", "--config", "/etc/mikrotik-kube/config.yaml"]
