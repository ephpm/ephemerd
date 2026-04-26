# CI build deps for macOS — outputs just binaries for VM overlay.
#
# Extract:
#   id=$(docker create ephpm/ephemerd:runner-ci-macos-deps)
#   docker cp $id:/deps ./macos-deps
#   docker rm $id
#
# Copy macos-deps/ into the macOS VM base image at /opt/ephemerd-ci/.

FROM golang:1.26-bookworm AS builder

ARG RUNNER_VERSION=2.333.1
ARG GOLANGCI_LINT_VERSION=2.11.4

RUN mkdir -p /deps/pkg/runner/embed /deps/bin /deps/pkg/vm/embed

RUN curl -fsSL -o "/deps/pkg/runner/embed/actions-runner-osx-arm64-${RUNNER_VERSION}.tar.gz" \
    "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-osx-arm64-${RUNNER_VERSION}.tar.gz" && \
    curl -fsSL -o "/deps/pkg/runner/embed/actions-runner-osx-x64-${RUNNER_VERSION}.tar.gz" \
    "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-osx-x64-${RUNNER_VERSION}.tar.gz"

RUN curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-darwin-arm64.tar.gz" | \
    tar -xz --strip-components=1 -C /deps/bin "golangci-lint-${GOLANGCI_LINT_VERSION}-darwin-arm64/golangci-lint" && \
    mv /deps/bin/golangci-lint /deps/bin/golangci-lint-arm64 && \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-darwin-amd64.tar.gz" | \
    tar -xz --strip-components=1 -C /deps/bin "golangci-lint-${GOLANGCI_LINT_VERSION}-darwin-amd64/golangci-lint" && \
    mv /deps/bin/golangci-lint /deps/bin/golangci-lint-amd64

# Build mage for darwin/arm64 + darwin/amd64. `go build <package>` needs to run
# inside a module, so we spin up a throwaway one to pin the version.
RUN mkdir -p /tmp/magebuild && cd /tmp/magebuild && \
    go mod init scratch && \
    go get github.com/magefile/mage@latest && \
    GOOS=darwin GOARCH=arm64 go build -o /deps/bin/mage-darwin-arm64 github.com/magefile/mage && \
    GOOS=darwin GOARCH=amd64 go build -o /deps/bin/mage-darwin-amd64 github.com/magefile/mage

RUN touch /deps/pkg/vm/embed/ephemerd-linux

COPY entrypoint-cache.sh /deps/entrypoint-cache.sh
RUN chmod +x /deps/entrypoint-cache.sh

FROM scratch
COPY --from=builder /deps /deps
