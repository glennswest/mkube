BINARY    := mkube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
DEVICE    ?= rose1.g10.lo
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local tarball deploy test lint clean mocks \
        build-registry build-installer build-update build-all \
        deploy-update deploy-installer

## Build the Go binary for the target architecture
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/$(BINARY)-$(ARCH) ./cmd/mkube/

## Build for the host platform (development)
build-local:
	go build $(GOFLAGS) -o dist/$(BINARY) ./cmd/mkube/

## Build standalone registry for target architecture
build-registry:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/mkube-registry-$(ARCH) ./cmd/registry/

## Build installer (runs locally on the workstation, not on the device)
build-installer:
	go build $(GOFLAGS) -o dist/mkube-installer ./cmd/installer/

## Build mkube-update for the target architecture
build-update:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/mkube-update-$(ARCH) ./cmd/mkube-update/

## Build all binaries for the target architecture
build-all: build build-registry build-installer build-update

## Create RouterOS-compatible docker-save tarball (no Docker needed)
tarball: build
	@mkdir -p dist
	@bash hack/make-tarball.sh dist/$(BINARY)-$(ARCH) deploy/config.yaml dist/$(BINARY)-$(ARCH).tar

## Deploy to a MikroTik device (build + upload + configure)
deploy: tarball
	bash hack/deploy.sh $(DEVICE) dist/$(BINARY)-$(ARCH).tar

## Run tests
test:
	go test -v -race ./...

## Lint
lint:
	golangci-lint run ./...

## Clean build artifacts
clean:
	rm -rf dist/

## Deploy mkube-update from GHCR (no local compilation)
deploy-update:
	bash hack/pull-and-deploy.sh $(DEVICE)

## Deploy mkube-installer to bootstrap a fresh device
deploy-installer:
	bash hack/deploy-installer.sh $(DEVICE)

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
