---
title: Windows Hyper-V Dispatch
weight: 3
aliases:
  - /architecture/windows-wsl-dispatch/
---

On Windows, ephemerd runs a single scheduler that handles both Windows and Linux jobs. Windows jobs run natively as Hyper-V isolated containers. Linux jobs are dispatched via gRPC to a Hyper-V Linux VM that ephemerd boots and manages directly.

## Why a Hyper-V VM (not WSL2)

An earlier revision dispatched Linux jobs to a WSL2 distro. That works when ephemerd runs as a user process, but Windows Services execute as `LocalSystem`, and WSL2 has no `LocalSystem` support — calling `wsl --import` or `wsl --exec` from `LocalSystem` fails with `0x80370102` / `WSL_E_USER_NOT_REGISTERED`. The Hyper-V Compute Service (HCS) has no such restriction, so ephemerd creates the Linux VM by calling `vmcompute.dll` directly. The same code path works for an interactive user *and* for the installed Windows service.

## Architecture

One poller on Windows dispatches Linux jobs to the Hyper-V VM via gRPC. The VM runs `ephemerd serve --containerd-only` plus a dispatch worker — no scheduler, no GitHub credentials.

```
Windows Host (ephemerd.exe serve):
  +-- Containerd (Windows, named pipe + 127.0.0.1 TCP)
  +-- Scheduler (single poller for ALL jobs)
  |   +-- Windows job -> local Runtime.Create() on Windows containerd
  |   +-- Linux job -> gRPC DispatchClient -> Hyper-V VM dispatch server
  +-- Hyper-V Linux VM boot (HCS / vmcompute.dll)

Hyper-V Linux VM (ephemerd serve --containerd-only):
  +-- Containerd (Linux, TCP :10000)
  +-- Persistent VHDX rootfs (data dir / containerd state)
  +-- Embedded Linux ephemerd binary, runner, CNI, gcompat, iptables
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

The Linux-specific code must run inside the Linux VM. The gRPC dispatch layer bridges the gap: the Windows scheduler sends job requests to the in-VM worker, which creates Linux containers using its own Linux-compiled runtime.

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

### Dispatch Server (Linux VM side)

Implemented in `pkg/scheduler/dispatch.go`. The `dispatchServer` struct wraps a `*runtime.Runtime` and a map of active `RunnerEnv` objects:

- **CreateJob**: calls `rt.Create(ctx, id, image, jitConfig)`, stores the resulting `RunnerEnv` by ID.
- **WaitJob**: looks up the env by ID, calls `rt.Wait(ctx, env)`, returns the exit code.
- **DestroyJob**: looks up the env by ID, calls `rt.Destroy(ctx, env)`, removes it from the map.

`StartDispatchServer(port, rt, log)` starts a TCP gRPC listener on `0.0.0.0:<port>`. It binds to all interfaces so the host (outside the VM) can reach it.

### Dispatch Client (Windows side)

Also in `pkg/scheduler/dispatch.go`. The `DispatchClient` struct holds a gRPC connection and provides `Create`, `Wait`, `Destroy`, and `Close` methods. The scheduler stores it as `LinuxDispatcher` and uses it when routing Linux-labeled jobs.

### Containerd-Only Mode

When the in-VM ephemerd boots with `--containerd-only`:

1. Starts embedded containerd with a TCP listener on `0.0.0.0:10000`.
2. Extracts the runner binary and CNI plugins from its embedded payload.
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
2. Hyper-V Linux VM boots in background: containerd-only + dispatch worker.
3. GitHub job queued with `runs-on: [self-hosted, linux, x64]`.
4. Windows scheduler sees it, detects `"linux"` label and `LinuxDispatcher != nil`.
5. Registers JIT runner with `["self-hosted", "linux", "x64"]` labels.
6. Calls `dispatcher.Create(name, image, jitConfig)` -- gRPC to the VM IP.
7. Dispatch server in VM creates a Linux container using its local Runtime.
8. Windows scheduler calls `dispatcher.Wait(name)` -- blocks until job completes.
9. Windows scheduler calls `dispatcher.Destroy(name)` -- cleans up container + networking in the VM.
10. Windows jobs follow the normal local Runtime flow.

## Hyper-V VM Lifecycle

The Linux VM is managed by `pkg/vm/linuxvm_windows.go` via the HCS (Host Compute Service) API:

- On startup, the embedded Linux kernel (`vmlinuz`) and initrd (containing a pre-baked Alpine rootfs + the cross-compiled Linux `ephemerd` binary) are written into `<DataDir>/vm/linux/`.
- A persistent VHDX root disk is created on first boot at `<DataDir>/containerd/linux-root/root.vhdx` (default 100 GB). Image content and containerd metadata live here, so a host restart doesn't re-pull every image.
- ephemerd builds an HCS compute system document for a KernelDirect (LCOW) boot and calls `vmcompute.dll` directly. We don't use hcsshim's `uvm.CreateLCOW` because it assumes a Microsoft GCS is running inside the VM (vsock-based), and we run a normal Linux userspace instead.
- An HCN endpoint on the Default Switch is attached to the VM. ephemerd watches WMI events to discover the assigned IP, then connects:
  - `<vm-ip>:10000` -- containerd gRPC (only used by buildkit and per-job runtime calls; jobs themselves see a unix socket inside the VM).
  - `<vm-ip>:10001` -- dispatch gRPC (CreateJob / WaitJob / DestroyJob).
- The Linux ephemerd binary launches with `--containerd-only`. No PEM file, no config.toml, no GitHub credentials inside the VM.
- On shutdown, ephemerd asks HCS to terminate the compute system and releases the HCN endpoint. The VHDX persists for the next boot.

The VM boots asynchronously in a background goroutine. Windows jobs can run immediately while the Linux VM starts up. Linux jobs queue until the dispatch client is connected to the VM.

## Pre-Baked Rootfs

The rootfs inside the initrd is an Alpine minirootfs with gcompat and iptables baked in at compile time. This eliminates network-dependent `apk add` calls during boot. See [Pre-baked rootfs]({{< relref "pre-baked-rootfs" >}}).

## What This Architecture Removes

Compared to the earlier dual-scheduler approach:

- No PEM file copy into the worker.
- No config.toml rewriting for the worker.
- No duplicate GitHub polling from the worker.
- No GitHub App token refresh in the worker.
- The Linux worker has no GitHub credentials at all.

Compared to the earlier WSL2-based worker:

- Works under `LocalSystem`, so the installed Windows service can manage Linux jobs.
- No dependency on the `wsl.exe` toolchain or any WSL distro registration.
- Boot is deterministic — same kernel, same initrd, same VHDX root every time.
