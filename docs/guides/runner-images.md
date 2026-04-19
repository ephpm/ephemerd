---
title: Runner Images
weight: 3
---

ephemerd uses OCI container images to define the execution environment for each job. The image determines what tools, runtimes, and system packages are available during the workflow run.

## Default Images

When no custom image is specified, ephemerd uses a platform-appropriate default:

| Platform | Default image |
|----------|--------------|
| Linux | `ghcr.io/actions/actions-runner:latest` |
| Windows | `mcr.microsoft.com/windows/servercore:ltsc20XX` (auto-detected from host OS build) |

## Specifying an Image

### Linux and Windows

Use the `container:` key in your workflow YAML. ephemerd's embedded containerd pulls the image on first use and caches it for subsequent jobs.

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
    container: ghcr.io/your-org/ci-image:latest
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

Any image in any OCI-compliant registry works -- Docker Hub, GitHub Container Registry, Amazon ECR, self-hosted registries, etc.

### macOS

macOS jobs run in per-job VMs, not containers. To specify a base VM image, set the `EPHEMERD_IMAGE` environment variable in your workflow. This maps to a VM snapshot defined in the `[vm.macos]` config section.

```yaml
jobs:
  build:
    runs-on: [self-hosted, macos]
    env:
      EPHEMERD_IMAGE: ghcr.io/your-org/macos-xcode16:latest
    steps:
      - run: xcodebuild -version
```

When `EPHEMERD_IMAGE` is set, ephemerd pulls the OCI image via containerd and extracts its layers into a directory shared with the macOS VM via virtio-fs. This makes pre-built tools and SDKs available inside the VM without needing to install them on every run.

## Building Custom Images

Custom images start from any base and add your project's dependencies. A simple Dockerfile:

```dockerfile
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    build-essential \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# Install project-specific tools
RUN curl -fsSL https://go.dev/dl/go1.24.3.linux-amd64.tar.gz | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"
```

Build and push:

```bash
docker build -t ghcr.io/your-org/ci-image:latest .
docker push ghcr.io/your-org/ci-image:latest
```

The image is pulled once by ephemerd and cached locally. Subsequent jobs using the same image tag start without a pull delay (unless the tag points to a new digest).

## One Image, Every Host

The same Linux container image runs identically on Linux, Windows (via WSL2), and macOS (via Virtualization.framework). In all three cases, containerd is the runtime that pulls and executes the image. There is no need to maintain separate images per host platform.

## OCI Artifact Cache for macOS

macOS VM jobs can use OCI images as a pre-built artifact cache. Package build outputs, compiled SDKs, or large dependencies into a `FROM scratch` image:

```dockerfile
FROM golang:1.24 AS builder
RUN go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

FROM scratch
COPY --from=builder /go/bin/golangci-lint /usr/local/bin/
COPY --from=builder /usr/local/go /usr/local/go
```

```bash
docker build -t ghcr.io/your-org/go-tools:latest .
docker push ghcr.io/your-org/go-tools:latest
```

When a macOS job references this image via `EPHEMERD_IMAGE`, ephemerd pulls it through containerd (which caches the layers), extracts the filesystem into a host directory, and shares it into the macOS VM via virtio-fs. The tools are available immediately without download or compilation.

This turns any OCI registry into a binary cache for macOS CI jobs.
