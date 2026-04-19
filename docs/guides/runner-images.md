---
title: Runner Images
weight: 4
---

ephemerd uses OCI container images to define the execution environment for each job. The image model differs by provider -- GitHub Actions uses a single container, while Forgejo/Gitea use a two-container model, and Woodpecker manages its own agent containers.

## GitHub Actions

### How it works

GitHub Actions jobs run inside a single container. The runner binary lives inside the image, and job steps execute in the same container. ephemerd pulls the image, starts a container, and the embedded runner picks up the job.

### Default images

| Platform | Default image |
|----------|--------------|
| Linux | `ghcr.io/actions/actions-runner:latest` |
| Windows | `mcr.microsoft.com/windows/servercore:ltsc20XX` (auto-detected) |

### Specifying an image

Use the `container:` key in your workflow YAML:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
    container: ghcr.io/your-org/ci-image:latest
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

### Building custom images

Custom images must extend the upstream GitHub Actions runner base image. This is important -- the base includes the runner binary that ephemerd needs to execute jobs.

**Linux:**

```dockerfile
FROM ghcr.io/actions/actions-runner:latest

USER root

RUN apt-get update && apt-get install -y \
    build-essential cmake autoconf automake \
    git curl wget pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Add language runtimes, SDKs, etc.
# RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y

USER runner
```

For multi-arch builds (amd64 + arm64):

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
    -t ghcr.io/your-org/ci-image:latest --push .
```

**Windows:**

```dockerfile
# escape=`
FROM ghcr.io/actions/actions-runner:latest-win

SHELL ["powershell", "-Command", "$ErrorActionPreference = 'Stop';"]

RUN Invoke-WebRequest -Uri "https://go.dev/dl/go1.26.1.windows-amd64.zip" -OutFile go.zip; `
    Expand-Archive go.zip -DestinationPath C:\; `
    Remove-Item go.zip
ENV PATH="C:\go\bin;${PATH}"
```

Windows images must be built on a Windows host.

### macOS (artifact image)

macOS jobs run in per-job VMs, not containers. Set `EPHEMERD_IMAGE` in your workflow to deliver pre-built tools via an OCI artifact image:

```yaml
jobs:
  build:
    runs-on: [self-hosted, macos]
    env:
      EPHEMERD_IMAGE: ghcr.io/your-org/macos-xcode16:latest
    steps:
      - run: xcodebuild -version
```

The image is a `FROM scratch` container with binaries copied from a builder stage. ephemerd pulls it, extracts the layers, and mounts them into the macOS VM via virtio-fs:

```dockerfile
FROM golang:1.26-bookworm AS builder
RUN GOOS=darwin GOARCH=arm64 go build -o /deps/bin/mage github.com/magefile/mage

FROM scratch
COPY --from=builder /deps /deps
```

## Forgejo / Gitea

### How it works

Forgejo and Gitea use a **two-container model**. The runner daemon (`forgejo-runner` or `act_runner`) runs in one container and creates job execution containers via the Docker API. ephemerd's fake Docker socket (`pkg/dind`) intercepts these Docker API calls and translates them to containerd operations. The job container is a sibling container managed by ephemerd, not a nested container.

```
[ runner container: forgejo-runner ] --docker API--> [ job container: ubuntu-24.04 ]
         │                                                    │
         └── fake Docker socket (pkg/dind) ──────────────────>└── containerd sibling
```

### Two images to configure

| Image | Purpose | Config key |
|-------|---------|------------|
| **Runner image** | Contains the runner daemon binary | `[runner] default_image` |
| **Job image** | Where workflow steps execute | `[forgejo] job_image` or `[gitea] job_image` |

### Default images

**Forgejo:**

| Image | Default |
|-------|---------|
| Runner | `data.forgejo.org/forgejo/runner:12` |
| Job | `gitea/runner-images:ubuntu-24.04` |

**Gitea:**

| Image | Default |
|-------|---------|
| Runner | `docker.io/gitea/act_runner:latest` |
| Job | `gitea/runner-images:ubuntu-24.04` |

### Customizing the job image

The job image is where your workflow steps actually run. This is the image you'll customize most often. It doesn't need a runner binary -- the runner daemon in the other container handles that.

```dockerfile
FROM gitea/runner-images:ubuntu-24.04

# Add your project's build dependencies
RUN apt-get update && apt-get install -y \
    build-essential cmake pkg-config \
    && rm -rf /var/lib/apt/lists/*
```

```bash
docker build -t ghcr.io/your-org/forge-job:latest .
docker push ghcr.io/your-org/forge-job:latest
```

Set it in the config:

```toml
[forgejo]
instance_url = "https://codeberg.org"
token = "runner-registration-token"
owner = "your-org"
job_image = "ghcr.io/your-org/forge-job:latest"
```

### Customizing the runner image

You rarely need to customize the runner image -- the upstream `forgejo-runner` or `act_runner` images work out of the box. If you do need to (e.g., to pin a specific runner version or add CA certificates), extend the upstream:

```dockerfile
FROM data.forgejo.org/forgejo/runner:12

# Add custom CA certs for self-hosted registries
COPY my-ca.crt /usr/local/share/ca-certificates/
RUN update-ca-certificates
```

## GitLab

### How it works

GitLab uses a **custom executor model**. The `gitlab-runner` binary drives the job lifecycle and calls ephemerd scripts for each phase: `prepare` (create container), `run` (execute steps), `cleanup` (destroy container). ephemerd doesn't discover jobs -- `gitlab-runner` polls GitLab and delegates to ephemerd.

### Images

The job image comes from the `image:` field in `.gitlab-ci.yml` -- it's part of the job payload, so no extra API call is needed. You don't configure a default image in ephemerd; GitLab handles image selection.

```yaml
# .gitlab-ci.yml
build:
  image: ghcr.io/your-org/ci-image:latest
  script:
    - make test
```

Any Docker image works. The `gitlab-runner` custom executor creates the container via ephemerd, which uses containerd to pull and run it.

## Woodpecker CI

### How it works

Woodpecker uses an **agent model**. The Woodpecker agent connects to the server via gRPC, receives pipeline definitions, and creates containers for each step. ephemerd manages the agent lifecycle -- it runs the agent binary inside a container, and the agent creates step containers via the Docker API (intercepted by ephemerd's fake Docker socket, same as Forgejo/Gitea).

### Images

Pipeline step images are defined in `.woodpecker.yml`:

```yaml
# .woodpecker.yml
steps:
  - name: build
    image: ghcr.io/your-org/ci-image:latest
    commands:
      - make test
```

The agent pulls step images via the fake Docker socket. Any OCI image works. There's no separate "runner image" to configure -- the Woodpecker agent image is managed by ephemerd internally.

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
