# CI runner image for Linux — ephemerd's self-hosted runner with build deps pre-cached.
#
# Supports linux/amd64 and linux/arm64 via multi-platform build.
# TARGETARCH is set automatically by Docker buildx.
#
# Build:
#   docker buildx build --platform linux/amd64,linux/arm64 -t ephpm/ephemerd:runner-ci-linux .
#
# Use as default runner image:
#   [runner]
#   default_image = "ephpm/ephemerd:runner-ci-linux"

FROM ghcr.io/actions/actions-runner:latest

# Pinned versions — keep in sync with mage/download/download.go
ARG RUNNER_VERSION=2.335.1
ARG CNI_VERSION=1.6.2
ARG CONTAINERD_VERSION=2.2.2
ARG RUNC_VERSION=1.3.4
ARG GOLANGCI_LINT_VERSION=2.11.4
ARG GO_VERSION=1.26.1
ARG TARGETARCH

USER root

RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:/usr/local/bin:${PATH}"

# GOBIN=/usr/local/bin so mage is on PATH for the runner user (not just root).
RUN GOBIN=/usr/local/bin go install github.com/magefile/mage@latest

# Runner archive (x64/arm64 naming)
RUN RUNNER_ARCH=$([ "${TARGETARCH}" = "amd64" ] && echo "x64" || echo "arm64") && \
    mkdir -p /opt/ephemerd-ci/pkg/runner/embed && \
    curl -fsSL -o "/opt/ephemerd-ci/pkg/runner/embed/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"

# CNI plugins
RUN mkdir -p /opt/ephemerd-ci/pkg/cni/embed && \
    curl -fsSL -o "/opt/ephemerd-ci/pkg/cni/embed/cni-plugins-linux-${TARGETARCH}-v${CNI_VERSION}.tgz" \
    "https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-${TARGETARCH}-v${CNI_VERSION}.tgz"

# containerd-shim-runc-v2
RUN mkdir -p /opt/ephemerd-ci/pkg/containerd/embed && \
    curl -fsSL "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz" | \
    tar -xz --strip-components=1 -C /opt/ephemerd-ci/pkg/containerd/embed bin/containerd-shim-runc-v2

# runc
RUN curl -fsSL -o /opt/ephemerd-ci/pkg/containerd/embed/runc \
    "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${TARGETARCH}" && \
    chmod +x /opt/ephemerd-ci/pkg/containerd/embed/runc

# golangci-lint
RUN mkdir -p /opt/ephemerd-ci/bin && \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}.tar.gz" | \
    tar -xz --strip-components=1 -C /opt/ephemerd-ci/bin "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}/golangci-lint"

# go:embed placeholder
RUN mkdir -p /opt/ephemerd-ci/pkg/vm/embed && \
    touch /opt/ephemerd-ci/pkg/vm/embed/ephemerd-linux

COPY entrypoint-cache.sh /opt/ephemerd-ci/entrypoint-cache.sh
RUN chmod +x /opt/ephemerd-ci/entrypoint-cache.sh

USER runner
