BINARY    := mkube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
DEVICE    ?= rose1.g10.lo
REGISTRY  ?= registry.gt.lo:5000
IMAGE     := $(REGISTRY)/$(BINARY):edge
MKUBE_API ?= http://192.168.200.2:8082
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local tarball deploy deploy-tarball test lint clean mocks \
        build-registry build-installer build-update build-all \
        deploy-update deploy-installer \
        build-pve-deploy build-mkube-boot deploy-pvex-registry deploy-pvex-boot deploy-pvex-mkube

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

## Build pve-deploy CLI (runs locally on workstation, deploys to Proxmox)
build-pve-deploy:
	go build $(GOFLAGS) -o dist/pve-deploy ./cmd/pve-deploy/

## Build mkube-boot for amd64 (runs inside Proxmox LXC)
build-mkube-boot:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o dist/mkube-boot ./cmd/mkube-boot/

## Build all binaries for the target architecture
build-all: build build-registry build-installer build-update build-pve-deploy

## Create RouterOS-compatible docker-save tarball (no Docker needed)
tarball: build
	@mkdir -p dist
	@bash hack/make-tarball.sh dist/$(BINARY)-$(ARCH) deploy/config.yaml dist/$(BINARY)-$(ARCH).tar

## Deploy via local registry — build container, push, mkube-update picks it up
deploy: build
	cp dist/$(BINARY)-$(ARCH) mkube
	podman build --platform linux/$(ARCH) -f Dockerfile.scratch -t $(IMAGE) .
	rm -f mkube
	podman push --tls-verify=false $(IMAGE)
	@echo "Pushed $(IMAGE) — mkube-update will auto-update"
	@curl -sf -X POST $(MKUBE_API)/api/v1/images/redeploy && echo "Redeploy triggered" || echo "(mkube API not reachable, update will happen on next poll)"

## Deploy via tarball + SCP (bootstrap / fallback)
deploy-tarball: tarball
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

## Deploy registry to Proxmox (CT 119)
deploy-pvex-registry: build-pve-deploy
	./dist/pve-deploy --config deploy/pvex-registry.yaml

## Deploy mkube-boot to Proxmox (CT 120)
deploy-pvex-boot: build-pve-deploy
	./dist/pve-deploy --config deploy/pvex-mkube-boot.yaml

## Deploy mkube controller to Proxmox (CT 121)
deploy-pvex-mkube: build-pve-deploy
	./dist/pve-deploy --config deploy/pvex-mkube.yaml

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
