BINARY := ephemerd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# GitHub Actions runner version to embed
RUNNER_VERSION ?= 2.333.1

# Detect OS/arch for runner download
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
RUNNER_OS := $(if $(filter windows,$(GOOS)),win,$(if $(filter darwin,$(GOOS)),osx,linux))
RUNNER_ARCH := $(if $(filter arm64,$(GOARCH)),arm64,x64)
RUNNER_EXT := $(if $(filter windows,$(GOOS)),zip,tar.gz)
RUNNER_TARBALL := actions-runner-$(RUNNER_OS)-$(RUNNER_ARCH)-$(RUNNER_VERSION).$(RUNNER_EXT)
RUNNER_EMBED_DIR := pkg/runner/embed

LDFLAGS := -ldflags "-X main.version=$(VERSION) -X github.com/ephpm/ephemerd/pkg/runner.Version=$(RUNNER_VERSION)"

.PHONY: build clean test lint download-runner generate

build: download-runner
	go build $(LDFLAGS) -o $(BINARY) ./cmd/ephemerd/

download-runner: $(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL)

$(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL):
	@mkdir -p $(RUNNER_EMBED_DIR)
	@echo "Downloading GitHub Actions runner $(RUNNER_VERSION) for $(RUNNER_OS)/$(RUNNER_ARCH)..."
	curl -fsSL -o $(RUNNER_EMBED_DIR)/$(RUNNER_TARBALL) \
		"https://github.com/actions/runner/releases/download/v$(RUNNER_VERSION)/$(RUNNER_TARBALL)"

clean:
	rm -f $(BINARY)
	rm -f $(RUNNER_EMBED_DIR)/actions-runner-*

test:
	go test ./...

lint:
	golangci-lint run ./...

generate:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/v1/ephemerd.proto
