---
title: CRI Passthrough
weight: 10
---

ephemerd exposes `crictl` as a built-in debug subcommand for inspecting the embedded containerd instance via the Kubernetes CRI API.

## Context

The same blank import that brings in containerd's native gRPC services (`github.com/containerd/containerd/v2/cmd/containerd/builtins`) also registers containerd's **CRI plugin**. This means the socket that ephemerd already listens on speaks both the native containerd API and the Kubernetes CRI.

Unlike `ctrctl` (which shells out to a separate `ctr` binary), `crictl` is linked into the ephemerd binary itself -- no external tool needs to be on `PATH`.

## Usage

```sh
ephemerd crictl <any crictl command>
```

Works on Linux and Windows. The endpoint is preconfigured to the embedded containerd CRI socket, so operators never need to pass `--runtime-endpoint`.

Examples:

```sh
ephemerd crictl version        # runtime name/version
ephemerd crictl info           # full CRI status (runtime + image service)
ephemerd crictl images         # images known to the CRI image service
ephemerd crictl ps -a          # containers (including exited)
ephemerd crictl pods           # sandboxes
ephemerd crictl logs <id>      # container stdout/stderr
ephemerd crictl inspect <id>   # JSON container status
ephemerd crictl stats          # resource usage
```

Any flag crictl accepts upstream works -- `--timeout`, `--debug`, `--output json|yaml`, etc.

## How It Works

`pkg/containerd/crictl.go` implements the in-process call:

```go
func ExecCrictl(socketPath string, args []string) error {
    endpoint := crictlEndpoint(socketPath)
    os.Setenv("CONTAINER_RUNTIME_ENDPOINT", endpoint)
    os.Setenv("IMAGE_SERVICE_ENDPOINT", endpoint)

    os.Args = append([]string{"crictl"}, args...)
    crictl.Main()
    return nil
}
```

Three load-bearing details:

1. **Environment variables, not flags.** crictl reads `CONTAINER_RUNTIME_ENDPOINT` / `IMAGE_SERVICE_ENDPOINT` as the default endpoint, and any user-supplied `--runtime-endpoint` on the command line overrides. This is less invasive than rewriting argv.
2. **`os.Args` is rewritten** so crictl's internal urfave/cli v2 app sees itself as `argv[0] = "crictl"`. The host CLI (urfave/cli v3) has already finished parsing -- `SkipFlagParsing: true` on the `crictl` subcommand hands the tail of argv through untouched.
3. **`crictl.Main()` can `os.Exit`.** On unrecoverable errors it calls `logrus.Fatal`. That is acceptable for a leaf subcommand whose process is meant to terminate after one invocation.

### Why a Fork

Upstream `kubernetes-sigs/cri-tools` ships crictl as `package main` with an unexported `main()`. In Go you cannot import `package main`. The k3s project maintains a fork with a two-line patch: rename `package main` to `package crictl` and export `Main()`. ephemerd reuses that fork via a `replace` directive in `go.mod`:

```
require sigs.k8s.io/cri-tools v1.34.0
replace sigs.k8s.io/cri-tools => github.com/k3s-io/cri-tools v1.34.0-k3s2
```

### CRI URI Construction

| Platform | Socket path | CRI endpoint URI |
|----------|------------|-----------------|
| Linux | `<datadir>/containerd/containerd.sock` | `unix://<datadir>/containerd/containerd.sock` |
| Windows | `\\.\pipe\ephemerd-containerd` | `npipe:////./pipe/ephemerd-containerd` |

Windows named-pipe URIs use forward slashes after the scheme -- the path is `//./pipe/<name>`, prefixed with `npipe://` to yield the four-slash form crictl expects.

## Platform Notes

- **Linux**: CRI is fully supported by containerd v2. All crictl commands behave as they would against a standalone containerd install.
- **Windows**: containerd v2 ships a native Windows CRI implementation (Hyper-V isolated containers). A handful of CRI features that assume Linux semantics (cgroups, mount propagation flags) are no-ops or return errors -- this mirrors upstream containerd behavior.
- **WSL-to-Windows**: when the Windows host routes Linux jobs to the WSL worker, `ephemerd crictl` on the host only sees the Windows containerd CRI. To inspect WSL-side Linux containers, use `wsl -- ephemerd crictl ...` inside the distro.

## Typical Debugging Workflow

```sh
# Are the runtime and image services healthy?
ephemerd crictl info | jq '.status'

# What's running right now?
ephemerd crictl ps

# What happened to a job container that exited?
ephemerd crictl ps -a
ephemerd crictl logs <container-id>
ephemerd crictl inspect <container-id>

# Inspect a pulled image
ephemerd crictl inspecti <image-ref>
```

## Trade-Offs

- **Binary size**: crictl and its transitive k8s deps add roughly 30-40 MB to the binary. Acceptable given the Windows binary already ships at 550 MB+.
- **Version drift**: the k3s-io fork is tied to k3s/rke2 release cadence. If k3s stops publishing a matching tag, the fallback is vendoring a copy of `cmd/crictl/main.go` with the same two-line patch in-repo.
- **Single invocation per process**: `crictl.Main()` leaves logrus, env vars, and `os.Args` in a mutated state. Each call should be from a fresh `ephemerd crictl` process.
