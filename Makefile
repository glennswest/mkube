BINARY    := mkube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
DEVICE    ?= 192.168.8.1
REGISTRY  ?= registry.gt.lo:5000
IMAGE     := $(REGISTRY)/$(BINARY):edge
MKUBE_API ?= http://192.168.200.2:8082
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local tarball deploy deploy-tarball test lint clean mocks \
        build-registry build-installer build-update build-agent build-all \
        deploy-update deploy-installer \
        build-pve-deploy build-mkube-boot deploy-pvex-registry deploy-pvex-boot deploy-pvex-mkube \
        build-test test-integration

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

## Build mkube-agent for amd64 (runs on bare metal CoreOS hosts)
build-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o dist/mkube-agent ./cmd/mkube-agent/

## Build all binaries for the target architecture
build-all: build build-registry build-installer build-update build-pve-deploy build-agent

## Create RouterOS-compatible docker-save tarball (no Docker needed)
tarball: build
	@mkdir -p dist
	@bash hack/make-tarball.sh dist/$(BINARY)-$(ARCH) deploy/config.yaml dist/$(BINARY)-$(ARCH).tar

## Deploy via local registry — build container, push, mkube-update picks it up.
## Also syncs config + boot-order to the device so they stay current.
## After push, waits up to 90s for mkube-update to swap in the new binary
## and verifies the running commit matches what was just built.
deploy: build deploy-config
	cp dist/$(BINARY)-$(ARCH) mkube
	podman build --platform linux/$(ARCH) -f Dockerfile.scratch -t $(IMAGE) .
	rm -f mkube
	podman push --tls-verify=false $(IMAGE)
	@echo "Pushed $(IMAGE) — waiting for mkube-update to swap binary..."
	@EXPECT_COMMIT=$(COMMIT); \
	for i in $$(seq 1 18); do \
		RUNNING=$$(curl -sf $(MKUBE_API)/healthz 2>/dev/null | grep '^commit:' | awk '{print $$2}'); \
		if [ "$$RUNNING" = "$$EXPECT_COMMIT" ]; then \
			echo "VERIFIED: running commit $$RUNNING matches build"; \
			exit 0; \
		fi; \
		if [ $$i -eq 1 ]; then \
			echo "  expecting commit: $$EXPECT_COMMIT, currently running: $${RUNNING:-unknown}"; \
		fi; \
		sleep 5; \
	done; \
	echo "WARNING: after 90s, running commit ($${RUNNING:-unknown}) != expected ($$EXPECT_COMMIT)"; \
	echo "Check mkube-update logs on the device"

## Sync config and boot-order files to the device
deploy-config:
	@CONFIG="deploy/rose1-config.yaml"; \
	if [ ! -f "$$CONFIG" ]; then CONFIG="deploy/config.yaml"; fi; \
	echo "Syncing $$CONFIG to $(DEVICE)..."; \
	scp -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$$CONFIG" admin@$(DEVICE):/raid1/volumes/kube.gt.lo/config/config.yaml
	@if [ -f deploy/boot-order.yaml ]; then \
		scp -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
			deploy/boot-order.yaml admin@$(DEVICE):/raid1/volumes/kube.gt.lo/config/boot-order.yaml; \
		echo "Config + boot-order synced"; \
	else \
		echo "Config synced (no boot-order.yaml)"; \
	fi

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

## Build mkube-test integration test CLI
build-test:
	go build $(GOFLAGS) -o dist/mkube-test ./cmd/mkube-test/

## Run live integration tests against rose1
test-integration: build-test
	./dist/mkube-test --api $(MKUBE_API)

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
