# Single-Poller Architecture: Dispatch Linux Jobs to WSL via gRPC

## Context

The previous dual-scheduler approach worked but was suboptimal:
- WSL booted the full 443MB ephemerd binary (slow start: ~2-3 min binary copy via 9P)
- Two independent schedulers polled GitHub (duplicate API calls)
- WSL needed GitHub App PEM + config.toml copied in (complex setup)
- WSL ran its own scheduler (double concurrency management)

## Architecture

**One poller on Windows dispatches Linux jobs to WSL via gRPC.** WSL runs containerd-only plus a dispatch worker — no scheduler.

```
Windows Host (ephemerd.exe serve):
  +-- Containerd (Windows, named pipe)
  +-- Scheduler (single poller for ALL jobs)
  |   +-- Windows job -> local Runtime.Create() on Windows containerd
  |   +-- Linux job -> gRPC DispatchClient -> WSL dispatch server
  +-- WSL VM boot (containerd-only + dispatch worker)

WSL (ephemerd serve --containerd-only):
  +-- Containerd (Linux, TCP :10000)
  +-- Runner extracted, CNI extracted, networking initialized
  +-- Dispatch gRPC server (TCP :10001)
      +-- CreateJob(id, image, jitConfig) -> local Runtime.Create()
      +-- WaitJob(id) -> local Runtime.Wait()
      +-- DestroyJob(id) -> local Runtime.Destroy()
```

## Why gRPC Dispatch Instead of Direct Containerd Client

`Runtime.Create()` uses `goruntime.GOOS` throughout for platform-specific behavior:
- OCI spec format (Hyper-V isolation vs Linux namespaces)
- Snapshotter ("windows" vs "overlayfs")
- Networking (HCN on Windows, CNI on Linux)
- Container I/O (NullIO on Windows, LogFile on Linux)
- Runner mount paths (`C:\actions-runner` vs `/actions-runner`)

A Windows-compiled Runtime **cannot** create Linux containers. The Linux-specific code must run inside WSL. Therefore we need a gRPC dispatch layer.

## Protobuf Service

Added to `api/v1/ephemerd.proto`:

```protobuf
service Dispatch {
  rpc CreateJob(CreateJobRequest) returns (CreateJobResponse);
  rpc WaitJob(WaitJobRequest) returns (WaitJobResponse);
  rpc DestroyJob(DestroyJobRequest) returns (DestroyJobResponse);
}

message CreateJobRequest {
  string id = 1;
  string image = 2;
  string jit_config = 3;
}
message CreateJobResponse {}

message WaitJobRequest { string id = 1; }
message WaitJobResponse { uint32 exit_code = 1; }

message DestroyJobRequest { string id = 1; }
message DestroyJobResponse {}
```

## Key Components

### Dispatch Server (runs in WSL, `pkg/scheduler/dispatch.go`)

- `dispatchServer` struct wrapping `*runtime.Runtime` + `map[string]*runtime.RunnerEnv`
- `CreateJob`: calls `rt.Create(ctx, req.Id, req.Image, req.JitConfig)`, stores env
- `WaitJob`: looks up env by ID, calls `rt.Wait(ctx, env)`, returns exit code
- `DestroyJob`: looks up env by ID, calls `rt.Destroy(ctx, env)`, removes from map
- `StartDispatchServer(port int, rt *runtime.Runtime, log)` starts TCP gRPC listener

### Dispatch Client (runs on Windows, `pkg/scheduler/dispatch.go`)

- `DispatchClient` struct with gRPC connection
- `Create(ctx, id, image, jitConfig) error`
- `Wait(ctx, id) (uint32, error)`
- `Destroy(ctx, id) error`
- `NewDispatchClient(addr string) (*DispatchClient, error)` connects to WSL dispatch server

### Containerd-Only Mode Extension (`cmd/ephemerd/main.go`)

Previously just blocked on `<-ctx.Done()`. Now also:
- Extracts runner + CNI
- Initializes networking
- Creates local `runtime.Runtime`
- Starts dispatch gRPC server on `containerdTCPPort + 1`

### Scheduler Routing (`pkg/scheduler/scheduler.go`)

- `LinuxDispatcher *DispatchClient` field added to `Config`
- `handleQueued()`: if job has "linux" label AND `LinuxDispatcher != nil`, calls `handleLinuxJob()`
- `handleLinuxJob()`: registers JIT runner with `["self-hosted", "linux", "x64"]` labels, dispatches via gRPC
- `buildLabelsForOS(os string)` helper takes target OS instead of using `goruntime.GOOS`

### WSL VM Changes (`pkg/vm/linuxvm_windows.go`)

- Removed PEM copy and config.toml rewriting (WSL worker doesn't need GitHub credentials)
- Restored `--containerd-only` flag in the WSL launch command
- After containerd is ready, connects a dispatch gRPC client to port `containerdPort + 1`
- New `DispatchClient()` method on `LinuxVM` interface

## End-to-End Flow

1. Windows host starts -> native containerd + single scheduler
2. WSL VM boots in background -> containerd-only + dispatch worker
3. GitHub job queued with `runs-on: [self-hosted, linux, x64]`
4. Windows scheduler sees it -> has "linux" label -> `LinuxDispatcher != nil` -> calls `handleLinuxJob()`
5. `handleLinuxJob()` registers JIT runner with `["self-hosted", "linux", "x64"]` labels
6. Calls `dispatcher.Create(name, image, jitConfig)` -> gRPC to WSL
7. WSL dispatch server creates Linux container using its local Runtime
8. Windows scheduler calls `dispatcher.Wait(name)` -> blocks until job completes
9. Windows scheduler calls `dispatcher.Destroy(name)` -> cleans up
10. Reverse: Windows job -> normal local Runtime flow

## What This Removed

- PEM file copy into WSL
- Config.toml rewriting for WSL
- Duplicate GitHub polling from WSL
- GitHub App token refresh in WSL
- `PrivateKeyPath` and `ConfigFile` from `LinuxVMConfig`

## What Stays the Same

- WSL distro lifecycle (import, terminate, unregister)
- gcompat + iptables installation
- UTF-16LE WSL output decoding
- Binary copy (still needed - containerd runs in-process)
- `canHandleJob()` for macOS/other OS filtering
