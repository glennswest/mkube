# Multi-stage build for mkube-update
# Produces a minimal container image for the update sidecar.

# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /mkube-update \
    ./cmd/mkube-update/

# ── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tini

COPY --from=builder /mkube-update /usr/local/bin/mkube-update

EXPOSE 8081

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["mkube-update"]
