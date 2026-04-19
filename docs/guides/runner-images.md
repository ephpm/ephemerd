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

Custom images extend the upstream GitHub Actions runner image (`ghcr.io/actions/actions-runner:latest`) and add your project's dependencies. This is important -- the runner image includes the GitHub Actions runner binary that ephemerd needs to execute jobs.

### Linux

```dockerfile
FROM ghcr.io/actions/actions-runner:latest

USER root

# Install your project's build dependencies
RUN apt-get update && apt-get install -y \
    build-essential cmake autoconf automake \
    git curl wget pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Add language runtimes, SDKs, etc.
# RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
# RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs

USER runner
```

For multi-arch builds (amd64 + arm64):

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
    -t ghcr.io/your-org/ci-image:latest --push .
```

### Windows

```dockerfile
# escape=`
FROM ghcr.io/actions/actions-runner:latest-win

SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop';"]

# Install your build tools
RUN Invoke-WebRequest -Uri "https://go.dev/dl/go1.26.1.windows-amd64.zip" -OutFile go.zip; `
    Expand-Archive go.zip -DestinationPath C:\; `
    Remove-Item go.zip
ENV PATH="C:\go\bin;${PATH}"
```

Windows images must be built on a Windows host.

### macOS (artifact image)

macOS VMs don't run containers, so the image is just a way to deliver pre-built tools. Use a `FROM scratch` image with binaries copied from a builder stage:

```dockerfile
FROM golang:1.26-bookworm AS builder
RUN GOOS=darwin GOARCH=arm64 go build -o /deps/bin/mage github.com/magefile/mage

FROM scratch
COPY --from=builder /deps /deps
```

ephemerd pulls this image, extracts the layers, and mounts them into the macOS VM via virtio-fs. The tools are available immediately without download or compilation.

## Per-Repo Image Overrides

Override the default image for specific repositories in the config:

```toml
[runner]
default_image = "ghcr.io/your-org/ci-image:latest"

[runner.repo_images]
"my-go-project" = "ghcr.io/your-org/go-ci:latest"
"my-rust-project" = "ghcr.io/your-org/rust-ci:latest"
```

## One Image, Every Host

The same Linux container image runs identically on Linux, Windows (via WSL2), and macOS (via Virtualization.framework). In all three cases, containerd is the runtime that pulls and executes the image. There is no need to maintain separate images per host platform.

## Reference: ephemerd CI Images

ephemerd's own CI uses custom runner images that pre-cache all build dependencies. These live in the [`images/`](https://github.com/ephpm/ephemerd/tree/feat/ci-runner-images/images) directory and serve as a real-world example:

| Image | Base | What it caches |
|-------|------|----------------|
| `runner-ci-linux` | `ghcr.io/actions/actions-runner:latest` | Go, Mage, runner archive, CNI plugins, containerd shim, runc, golangci-lint |
| `runner-ci-windows` | `ghcr.io/actions/actions-runner:latest-win` | Go, Mage, runner archive (Windows + Linux), golangci-lint |
| `runner-ci-macos` | `scratch` | Runner archive (macOS), Mage, golangci-lint (cross-compiled for darwin) |

The Linux image supports multi-arch (amd64 + arm64) via `docker buildx`. Each image includes an entrypoint script that copies the cached dependencies into the workspace so `mage ci` runs without downloading anything.
