BINARY    := mikrotik-kube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
DEVICE    ?= rose1.gw.lo
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local image tarball deploy test lint clean

## Build the Go binary for the target architecture
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/$(BINARY)-$(ARCH) ./cmd/mikrotik-kube/

## Build for the host platform (development)
build-local:
	go build $(GOFLAGS) -o dist/$(BINARY) ./cmd/mikrotik-kube/

## Build the container image
image:
	docker buildx build --platform linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(BINARY):$(VERSION)-$(ARCH) --load .

## Export as RouterOS-compatible rootfs tarball
tarball: image
	@mkdir -p dist
	@CONTAINER_ID=$$(docker create $(BINARY):$(VERSION)-$(ARCH)) && \
		docker export $$CONTAINER_ID -o dist/$(BINARY)-$(VERSION)-$(ARCH).tar && \
		docker rm $$CONTAINER_ID > /dev/null
	@echo "Built dist/$(BINARY)-$(VERSION)-$(ARCH).tar"

## Deploy to a MikroTik device (build + upload + configure)
deploy: tarball
	bash hack/deploy.sh $(DEVICE) dist/$(BINARY)-$(VERSION)-$(ARCH).tar

## Run tests
test:
	go test -v -race ./...

## Lint
lint:
	golangci-lint run ./...

## Clean build artifacts
clean:
	rm -rf dist/

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
