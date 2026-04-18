---
title: "^# "
---


## Context

`ephemerd` embeds containerd as a Go library (k3s model, see `pkg/containerd/server.go`). The same blank import that brings in containerd's native gRPC services — `github.com/containerd/containerd/v2/cmd/containerd/builtins` — also registers containerd's **CRI plugin**, so the socket we already listen on (`\\.\pipe\ephemerd-containerd` on Windows, `<data-dir>/containerd/containerd.sock` on Linux) speaks both the native containerd API *and* the Kubernetes CRI.

That makes it natural to expose `crictl`, the upstream Kubernetes CRI CLI, as a first-class debug subcommand. Compared to `ctrctl` (which shells out to a separate `ctr` binary), `crictl` is linked into the `ephemerd` binary itself — no external tool needs to be on `PATH`.

## Goal

```
ephemerd crictl <any crictl command>
```

Works on Linux and Windows. No binary downloads. The endpoint is preconfigured to the embedded containerd CRI socket, so operators never have to pass `--runtime-endpoint`.

## Why a fork

Upstream [`kubernetes-sigs/cri-tools`](https://github.com/kubernetes-sigs/cri-tools) ships crictl as `package main` with an unexported `main()`. In Go you cannot import `package main`, so the upstream binary cannot be called as a library.

rke2 and k3s solved this by forking cri-tools with a two-line patch: rename `package main` → `package crictl` and export `Main()`. We reuse that fork via a `replace` directive:

```
// go.mod
require sigs.k8s.io/cri-tools v1.34.0
replace sigs.k8s.io/cri-tools => github.com/k3s-io/cri-tools v1.34.0-k3s2
```

The `v1.34.x` line matches our `k8s.io/cri-api v0.34.1` (pulled transitively by `containerd/v2 v2.2.2`). Bump both together when upgrading containerd.

## How the call works

`pkg/containerd/crictl.go`:

```go
func ExecCrictl(socketPath string, args []string) error {
    endpoint := crictlEndpoint(socketPath) // unix:// or npipe://
    os.Setenv("CONTAINER_RUNTIME_ENDPOINT", endpoint)
    os.Setenv("IMAGE_SERVICE_ENDPOINT", endpoint)

    os.Args = append([]string{"crictl"}, args...)
    crictl.Main()
    return nil
}
```

Three load-bearing details:

1. **Environment variables, not flags.** crictl reads `CONTAINER_RUNTIME_ENDPOINT` / `IMAGE_SERVICE_ENDPOINT` as the default endpoint, and any user-supplied `--runtime-endpoint` / `-r` on the command line still overrides. This is less invasive than rewriting argv.
2. **`os.Args` is rewritten** so crictl's internal urfave/cli v2 app sees itself as `argv[0] = "crictl"`. Our host CLI (urfave/cli **v3**) has already finished parsing by the time this runs — `SkipFlagParsing: true` on the `crictl` subcommand hands the tail of argv through untouched.
3. **`crictl.Main()` can `os.Exit`.** On unrecoverable errors it calls `logrus.Fatal`. That is acceptable for a leaf subcommand whose process is meant to terminate after one invocation. Do not call `ExecCrictl` from inside `serve` or any other long-running path.

### CRI URI construction

| GOOS    | Socket path                           | CRI endpoint URI                           |
|---------|---------------------------------------|--------------------------------------------|
| Linux   | `/var/lib/ephemerd/containerd/containerd.sock` | `unix:///var/lib/ephemerd/containerd/containerd.sock` |
| Windows | `\\.\pipe\ephemerd-containerd`        | `npipe:////./pipe/ephemerd-containerd`     |

Windows named-pipe URIs use forward slashes after the scheme — the path is `//./pipe/<name>`, prefixed with `npipe://` to yield the four-slash form crictl expects.

## Usage

With a live `ephemerd serve` running:

```sh
ephemerd crictl version        # runtime name/version
ephemerd crictl info           # full CRI status (runtime + image service)
ephemerd crictl images         # images known to the CRI image service
ephemerd crictl ps -a          # containers (including exited)
ephemerd crictl pods           # sandboxes
ephemerd crictl logs <id>      # container stdout/stderr
ephemerd crictl exec -it <id> sh
ephemerd crictl inspect <id>   # JSON container status
ephemerd crictl inspecti <ref> # JSON image status
ephemerd crictl stats          # resource usage
```

Any flag crictl accepts upstream works — `--timeout`, `--debug`, `--output json|yaml`, etc. Run `ephemerd crictl --help` for the full list.

### Typical debugging workflow

```sh
# Are the runtime and image services healthy?
ephemerd crictl info | jq '.status'

# What's running right now?
ephemerd crictl ps

# What happened to a job container that exited?
ephemerd crictl ps -a
ephemerd crictl logs <container-id>
ephemerd crictl inspect <container-id>

# Inspect the pulled image
ephemerd crictl inspecti <image-ref>
```

## Platform notes

- **Linux**: CRI is fully supported by containerd v2. All crictl commands behave as they would against a standalone containerd install.
- **Windows**: containerd v2 ships a native Windows CRI implementation (Hyper-V isolated containers). crictl speaks npipe natively — no shim needed. A handful of CRI features that assume Linux semantics (cgroups, mount propagation flags) are no-ops or return errors; this mirrors upstream containerd and is not an ephemerd-specific limitation.
- **WSL-to-Windows**: when the Windows host routes Linux jobs to the WSL worker (see `docs/arch/windows-single-scheduler.md`), `ephemerd crictl` on the host only sees the Windows containerd CRI. To inspect the WSL-side Linux containers, `wsl -- ephemerd crictl ...` inside the distro.

## Testing

- `pkg/containerd/crictl_test.go` — unit tests for the endpoint URI translation (both OSes).
- `test/e2e/crictl_test.go` (build tags `e2e privileged`) — runs crictl against a real embedded containerd and asserts that `version`, `info`, `images`, and `ps -a` reach the CRI and return expected output. The e2e suite's `TestMain` doubles as a subprocess entry point: it re-execs the test binary and dispatches to `ExecCrictl` so `crictl.Main()`'s `os.Exit` on error cannot tear down the parent test process.

## Trade-offs

- **Binary size**: crictl and its transitive k8s deps (`cri-api`, `cri-client`, `kubectl`, `kubelet`) add roughly 30–40 MB to the Windows binary. Acceptable given we already ship 550 MB+.
- **Version drift**: the k3s-io fork is tied to k3s/rke2 release cadence. If k3s stops publishing a tag matching our `cri-api` minor, switch to vendoring a copy of `cmd/crictl/main.go` with the same two-line patch in-repo.
- **Single crictl invocation per process**: `crictl.Main()` leaves logrus, env vars, and `os.Args` in a mutated state, and may `os.Exit` via `logrus.Fatal`. Each call should be from a fresh `ephemerd crictl` process.
