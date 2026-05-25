---
title: Embedded containerd
weight: 2
---

ephemerd runs containerd in-process as a Go library, following the model established by k3s and rke2. No external containerd binary or system service is needed.

## How It Works

The `pkg/containerd/server.go` package imports containerd v2 as a library:

```go
import (
    ctdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
    _ "github.com/containerd/containerd/v2/cmd/containerd/builtins"
)
```

The blank import of `builtins` registers all containerd plugins (services, snapshotters, runtimes, CRI) with the global plugin registry. Without it, the server would start empty with no gRPC services.

On startup, `server.New()`:

1. Extracts embedded shim and runc binaries into `<datadir>/bin/` (Linux only -- Windows uses Hyper-V isolation, which does not need a shim).
2. Adds the bin directory to `PATH` so containerd can find runc.
3. Creates the data directory structure.
4. Builds a `srvconfig.Config` with root, state, and socket paths.
5. Calls `ctdserver.New(ctx, cfg)` to create the in-process server.
6. Creates a gRPC listener on the platform-appropriate socket and serves in a background goroutine.
7. Also creates a tTRPC listener for task/event APIs.
8. Optionally creates a TCP listener for remote access (used by the Windows or macOS host to connect to the in-VM containerd).
9. Connects an in-process containerd client and waits for it to become ready (up to 15 seconds).

The server, gRPC listeners, and client all run in the same process. On shutdown, `Server.Stop()` closes the client, stops the server, cancels the context, and waits for the background goroutines to finish.

## Socket Paths

The socket type differs by platform:

| Platform | Socket path | Listener implementation |
|----------|------------|------------------------|
| Linux | `<datadir>/containerd/containerd.sock` | Unix socket (`net.Listen("unix", ...)`) |
| macOS | `<datadir>/containerd/containerd.sock` | Unix socket |
| Windows | `\\.\pipe\ephemerd-containerd` | Named pipe (`go-winio.ListenPipe(...)`) |

The `SocketPath()` function in `pkg/containerd/server.go` returns the correct path based on `runtime.GOOS`.

### TCP Listener

When `TCPPort` is set in the config (e.g., `--containerd-tcp-port 10000`), the server also listens on TCP. This is used for:

- **Windows host to Hyper-V Linux VM**: the Windows scheduler connects to the in-VM containerd via TCP since named pipes do not cross the VM boundary.
- **macOS host to Linux VM**: the macOS host connects to containerd inside the Virtualization.framework Linux VM via TCP over NAT.

The TCP bind address defaults to `127.0.0.1` but can be configured to `0.0.0.0` for VM environments where the host is on a different network interface.

## Data Directory Layout

```
<datadir>/
  containerd/
    root/                     # content store, snapshots, image metadata
    state/                    # runtime state (task PIDs, container metadata)
    containerd.sock           # gRPC socket (Linux/macOS)
    containerd.sock.ttrpc     # tTRPC socket for task/event APIs
  bin/                        # extracted shim + runc (Linux only)
    containerd-shim-runc-v2
    runc
  runners/                    # per-job runner workdirs
  jobs/                       # per-job temp state (docker sockets, logs)
```

Default data directory:

- Linux: `/var/lib/ephemerd`
- macOS: `/var/lib/ephemerd`
- Windows: `C:\ProgramData\ephemerd`

## Snapshotter Differences

The snapshotter is the filesystem backend that containerd uses to unpack OCI image layers and create container root filesystems.

| Platform | Snapshotter | Notes |
|----------|------------|-------|
| Linux | `overlayfs` | Standard overlay filesystem. Efficient copy-on-write layer stacking. |
| Windows | `windows` | Windows-native snapshotter. Uses Hyper-V isolation for containers. Not overlayfs. |

The snapshotter is selected automatically by containerd based on the platform. No configuration is needed.

## Shim Binaries

On Linux, containerd needs two external binaries to run containers:

- **containerd-shim-runc-v2**: the container shim process that manages the container lifecycle on behalf of containerd.
- **runc**: the OCI runtime that actually creates and runs the container process using Linux namespaces and cgroups.

These are embedded in the ephemerd binary via `go:embed` (in `pkg/containerd/shim_linux.go`) and extracted to `<datadir>/bin/` at startup. On Windows, these are not needed because Hyper-V isolation uses the `runhcs` runtime which is part of the Windows OS.

## No External Dependencies

The key benefit of embedding containerd is zero external dependencies:

- No `apt install containerd` or `brew install containerd`.
- No version mismatches between ephemerd and containerd.
- No systemd service files or init scripts.
- No socket permission configuration.
- The containerd version is locked at compile time and ships inside the binary.
