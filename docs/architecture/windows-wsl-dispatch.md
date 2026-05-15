---
title: Windows WSL Dispatch
weight: 3
---

On Windows, ephemerd runs a single scheduler that handles both Windows and Linux jobs. Windows jobs run natively in Hyper-V containers. Linux jobs are dispatched to a WSL2 worker via gRPC.

## Architecture

One poller on Windows dispatches Linux jobs to WSL via gRPC. WSL runs containerd-only plus a dispatch worker -- no scheduler, no GitHub credentials.

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

## Why gRPC Dispatch

A Windows-compiled `Runtime.Create()` cannot create Linux containers. The runtime code uses `runtime.GOOS` throughout for platform-specific behavior:

- OCI spec format (Hyper-V isolation vs Linux namespaces)
- Snapshotter selection (`"windows"` vs `"overlayfs"`)
- Networking setup (HCN on Windows, CNI on Linux)
- Container I/O (`cio.NullIO` on Windows, log file on Linux)
- Runner mount paths (`C:\actions-runner` vs `/actions-runner`)

The Linux-specific code must run inside WSL. The gRPC dispatch layer bridges the gap: the Windows scheduler sends job requests to the WSL worker, which creates Linux containers using its own Linux-compiled runtime.

## Protobuf Dispatch Service

The Dispatch service is defined in `api/v1/ephemerd.proto` alongside the Control service:

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

### Dispatch Server (WSL side)

Implemented in `pkg/scheduler/dispatch.go`. The `dispatchServer` struct wraps a `*runtime.Runtime` and a map of active `RunnerEnv` objects:

- **CreateJob**: calls `rt.Create(ctx, id, image, jitConfig)`, stores the resulting `RunnerEnv` by ID.
- **WaitJob**: looks up the env by ID, calls `rt.Wait(ctx, env)`, returns the exit code.
- **DestroyJob**: looks up the env by ID, calls `rt.Destroy(ctx, env)`, removes it from the map.

`StartDispatchServer(port, rt, log)` starts a TCP gRPC listener on `0.0.0.0:<port>`. It binds to all interfaces so the host (outside the VM) can reach it.

### Dispatch Client (Windows side)

Also in `pkg/scheduler/dispatch.go`. The `DispatchClient` struct holds a gRPC connection and provides `Create`, `Wait`, `Destroy`, and `Close` methods. The scheduler stores it as `LinuxDispatcher` and uses it when routing Linux-labeled jobs.

### Containerd-Only Mode

When WSL boots ephemerd with `--containerd-only`:

1. Starts embedded containerd with a TCP listener.
2. Extracts the runner binary and CNI plugins.
3. Initializes networking (CNI bridge, stale bridge cleanup).
4. Creates a local `runtime.Runtime`.
5. Starts the dispatch gRPC server on `containerdPort + 1` (default port 10001).
6. Blocks until shutdown -- no scheduler, no GitHub polling.

### Scheduler Routing

In `pkg/scheduler/scheduler.go`, when a job arrives:

1. `handleQueued()` checks if the job has a `"linux"` label AND `LinuxDispatcher != nil`.
2. If yes, calls `handleLinuxJob()`.
3. `handleLinuxJob()` registers a JIT runner with `["self-hosted", "linux", "x64"]` labels.
4. Dispatches the job via gRPC: `Create` -> `Wait` -> `Destroy`.

Windows-labeled jobs go through the normal local `Runtime.Create()` path.

## End-to-End Flow

1. Windows host starts: native containerd + single scheduler.
2. WSL VM boots in background: containerd-only + dispatch worker.
3. GitHub job queued with `runs-on: [self-hosted, linux, x64]`.
4. Windows scheduler sees it, detects `"linux"` label and `LinuxDispatcher != nil`.
5. Registers JIT runner with `["self-hosted", "linux", "x64"]` labels.
6. Calls `dispatcher.Create(name, image, jitConfig)` -- gRPC to WSL.
7. WSL dispatch server creates a Linux container using its local Runtime.
8. Windows scheduler calls `dispatcher.Wait(name)` -- blocks until job completes.
9. Windows scheduler calls `dispatcher.Destroy(name)` -- cleans up container + networking in WSL.
10. Windows jobs follow the normal local Runtime flow.

## WSL VM Lifecycle

The WSL VM is managed by `pkg/vm/linuxvm_windows.go`:

- On startup, imports a WSL distro from the embedded pre-built rootfs.
- Runs the Linux ephemerd binary from `/mnt/c/` (Windows disk mount, avoids slow 9P copy into the distro).
- Launches with `--containerd-only` -- no GitHub credentials are needed in WSL.
- After containerd is ready, connects a dispatch gRPC client to port `containerdPort + 1`.
- On shutdown, the distro is unregistered via `wsl --unregister`.

The WSL VM boots asynchronously in a background goroutine. Windows jobs can run immediately while the WSL worker starts up. Linux jobs queue until the dispatch client is connected.

## Pre-Baked Rootfs

The WSL rootfs is an Alpine minirootfs with gcompat and iptables baked in at compile time. This eliminates network-dependent `apk add` calls during boot. See [Pre-baked rootfs]({{< relref "pre-baked-rootfs" >}}).

## What This Architecture Removes

Compared to the earlier dual-scheduler approach:

- No PEM file copy into WSL.
- No config.toml rewriting for WSL.
- No duplicate GitHub polling from WSL.
- No GitHub App token refresh in WSL.
- WSL has no GitHub credentials at all.
