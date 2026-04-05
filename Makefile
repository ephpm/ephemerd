BINARY := ephemerd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# GitHub Actions runner version to embed
RUNNER_VERSION ?= 2.333.1

# CNI plugins version to embed
CNI_VERSION ?= 1.6.2

# containerd shim + runc versions to embed
CONTAINERD_VERSION ?= 2.2.2
RUNC_VERSION ?= 1.3.4

# Detect OS/arch for runner download
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
RUNNER_OS := $(if $(filter windows,$(GOOS)),win,$(if $(filter darwin,$(GOOS)),osx,linux))
RUNNER_ARCH := $(if $(filter arm64,$(GOARCH)),arm64,x64)
RUNNER_EXT := $(if $(filter windows,$(GOOS)),zip,tar.gz)
RUNNER_TARBALL := actions-runner-$(RUNNER_OS)-$(RUNNER_ARCH)-$(RUNNER_VERSION).$(RUNNER_EXT)
RUNNER_EMBED_DIR := pkg/runner/embed

CNI_ARCH := $(if $(filter arm64,$(GOARCH)),arm64,amd64)
CNI_TARBALL := cni-plugins-linux-$(CNI_ARCH)-v$(CNI_VERSION).tgz
CNI_EMBED_DIR := pkg/cni/embed

SHIM_EMBED_DIR := pkg/containerd/embed

LDFLAGS := -ldflags "\
	-X main.version=$(VERSION) \
	-X github.com/ephpm/ephemerd/pkg/runner.Version=$(RUNNER_VERSION) \
	-X github.com/ephpm/ephemerd/pkg/cni.Version=$(CNI_VERSION)"

.PHONY: build clean test lint download-runner download-cni download-shim

build: download-runner download-cni download-shim
	go build $(LDFLAGS) -o $(BINARY) ./cmd/ephemerd/

download-runner: $(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL)

$(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL):
	@mkdir -p $(RUNNER_EMBED_DIR)
	@echo "Downloading GitHub Actions runner $(RUNNER_VERSION) for $(RUNNER_OS)/$(RUNNER_ARCH)..."
	curl -fsSL -o $(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL) \
		"https://github.com/actions/runner/releases/download/v$(RUNNER_VERSION)/$(RUNNER_TARBALL)"

download-cni: $(CNI_EMBED_DIR)/$(CNI_TARBALL)

$(CNI_EMBED_DIR)/$(CNI_TARBALL):
	@mkdir -p $(CNI_EMBED_DIR)
	@echo "Downloading CNI plugins $(CNI_VERSION) for linux/$(CNI_ARCH)..."
	curl -fsSL -o $(CNI_EMBED_DIR)/$(CNI_TARBALL) \
		"https://github.com/containernetworking/plugins/releases/download/v$(CNI_VERSION)/$(CNI_TARBALL)"

download-shim: $(SHIM_EMBED_DIR)/containerd-shim-runc-v2 $(SHIM_EMBED_DIR)/runc

$(SHIM_EMBED_DIR)/containerd-shim-runc-v2:
	@mkdir -p $(SHIM_EMBED_DIR)
	@echo "Downloading containerd-shim-runc-v2 from containerd $(CONTAINERD_VERSION)..."
	curl -fsSL "https://github.com/containerd/containerd/releases/download/v$(CONTAINERD_VERSION)/containerd-$(CONTAINERD_VERSION)-linux-$(CNI_ARCH).tar.gz" \
		| tar -xzf - -C $(SHIM_EMBED_DIR) --strip-components=1 bin/containerd-shim-runc-v2

$(SHIM_EMBED_DIR)/runc:
	@mkdir -p $(SHIM_EMBED_DIR)
	@echo "Downloading runc $(RUNC_VERSION)..."
	curl -fsSL -o $(SHIM_EMBED_DIR)/runc \
		"https://github.com/opencontainers/runc/releases/download/v$(RUNC_VERSION)/runc.$(CNI_ARCH)"
	chmod +x $(SHIM_EMBED_DIR)/runc

clean:
	rm -f $(BINARY)
	rm -f $(RUNNER_EMBED_DIR)/actions-runner-*
	rm -f $(CNI_EMBED_DIR)/cni-plugins-*
	rm -f $(SHIM_EMBED_DIR)/containerd-shim-runc-v2 $(SHIM_EMBED_DIR)/runc

test:
	go test ./...

lint:
	golangci-lint run ./...
