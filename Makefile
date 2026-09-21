PLUGIN_NAME ?= ghcr.io/changemakerstudios/docker-volume-flasharray
PLUGIN_TAG  ?= dev
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORM    ?= linux/amd64
BUILD_DIR   ?= build/plugin

.PHONY: all build test lint rootfs plugin push enable disable clean

all: test plugin

build: ## compile the binary locally
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/docker-volume-flasharray ./cmd/docker-volume-flasharray

test:
	go vet ./...
	go test ./... -race -count=1

lint:
	golangci-lint run ./...

rootfs: ## export the plugin rootfs with buildx (no docker create/export dance)
	rm -rf $(BUILD_DIR) && mkdir -p $(BUILD_DIR)/rootfs
	docker buildx build --platform $(PLATFORM) --build-arg VERSION=$(VERSION) \
		--output type=local,dest=$(BUILD_DIR)/rootfs .
	cp plugin/config.json $(BUILD_DIR)/config.json

plugin: rootfs ## create the managed plugin locally
	-docker plugin rm -f $(PLUGIN_NAME):$(PLUGIN_TAG) 2>/dev/null
	docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) $(BUILD_DIR)
	@echo "created $(PLUGIN_NAME):$(PLUGIN_TAG)"

push: ## push to the registry (docker login first)
	docker plugin push $(PLUGIN_NAME):$(PLUGIN_TAG)

enable: ## enable on this host (expects /etc/docker-volume-flasharray/flasharray.json)
	docker plugin enable $(PLUGIN_NAME):$(PLUGIN_TAG)

disable:
	docker plugin disable -f $(PLUGIN_NAME):$(PLUGIN_TAG)

clean:
	rm -rf bin build

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'
