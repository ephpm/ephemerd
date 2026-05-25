---
title: Dev Setup
weight: 1
---

## Prerequisites

- **Go 1.26+** (module requires 1.26.1)
- **[Mage](https://magefile.org/)** -- install with `go install github.com/magefile/mage@latest`
- **Linux, macOS (Apple Silicon), or Windows**

## Clone and build

```bash
git clone https://github.com/ephpm/ephemerd.git
cd ephemerd

# Download all embedded dependencies (runner binary, CNI plugins, containerd shim, Alpine rootfs)
mage download:all

# Build for the current platform
mage build

# Run linter + tests + build (same pipeline as CI)
mage ci
```

## Individual Mage targets

| Target | Description |
|---|---|
| `mage lint` | Download golangci-lint and run it |
| `mage test` | Download embedded deps and run all tests |
| `mage build:build` | Compile ephemerd for the current OS |
| `mage build:windows` | Two-stage Windows build (cross-compiles and embeds the Linux binary that runs inside the Hyper-V Linux VM) |
| `mage e2e` | Unprivileged e2e tests (requires `GITHUB_TOKEN`) |
| `mage e2eall` | All e2e tests including privileged (requires root + containerd) |
| `mage e2eforgejo` | Forgejo provider e2e (requires Docker with compose) |
| `mage e2egitea` | Gitea provider e2e (requires Docker with compose) |
| `mage e2egitlab` | GitLab CE provider e2e (requires Docker with compose) |
| `mage e2egithub` | GitHub provider e2e using a fake in-process API server |
| `mage e2ewoodpecker` | Woodpecker CI provider e2e (requires Docker with compose) |
| `mage generate` | Regenerate protobuf Go code |
| `mage docs` | Build the docs site (downloads Hugo first) |
| `mage docsServe` | Start the Hugo dev server for local preview |
| `mage clean` | Remove all downloaded assets and build artifacts |
| `mage ci` | Run download, lint, test, and build (same as GitHub Actions) |

Run `mage -l` for the full list of available targets.
