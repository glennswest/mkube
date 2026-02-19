BINARY    := mkube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
DEVICE    ?= rose1.gw.lo
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local tarball deploy test lint clean mocks

## Build the Go binary for the target architecture
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/$(BINARY)-$(ARCH) ./cmd/mkube/

## Build for the host platform (development)
build-local:
	go build $(GOFLAGS) -o dist/$(BINARY) ./cmd/mkube/

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

## Build mkube-update for the target architecture (only needed for development)
build-update:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/mkube-update-$(ARCH) ./cmd/mkube-update/

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
