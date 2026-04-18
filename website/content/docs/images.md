---
title: Runner Images
weight: 6
---

ephemerd uses standard OCI images. Build them with Docker:

## Linux and Windows jobs (OCI containers)

Use the standard `container:` key in your workflow:

```yaml
jobs:
  build-php:
    runs-on: [self-hosted, linux, x64]
    container:
      image: ghcr.io/myorg/php-builder:latest
    steps:
      - uses: actions/checkout@v4
      - run: make build

  build-windows:
    runs-on: [self-hosted, windows, x64]
    container:
      image: ghcr.io/myorg/windows-build:latest
    steps:
      - uses: actions/checkout@v4
      - run: nmake
```

## macOS jobs (VMs)

macOS jobs run in ephemeral VMs, not containers. Set `EPHEMERD_IMAGE` in the job's env to select which VM snapshot to boot:

```yaml
jobs:
  build-ios:
    runs-on: [self-hosted, macos, arm64]
    env:
      EPHEMERD_IMAGE: xcode15
    steps:
      - uses: actions/checkout@v4
      - run: xcodebuild -scheme MyApp
```

## Building custom images

```dockerfile
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    build-essential cmake autoconf automake \
    git curl wget pkg-config

# Add your project-specific tools
# RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
```

```bash
docker build -t ghcr.io/your-org/ephemerd-build:latest .
docker push ghcr.io/your-org/ephemerd-build:latest
```

The same image runs on every host — Linux directly, Windows via Hyper-V Linux VM, macOS via Virtualization.framework Linux VM.
