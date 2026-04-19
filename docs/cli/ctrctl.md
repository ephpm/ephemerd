---
title: ctrctl
weight: 12
---

Passthrough to containerd's `ctr` CLI. All arguments after `ctrctl` are forwarded directly to `ctr` with the correct containerd socket path automatically configured.

```
ephemerd ctrctl [ctr-args...]
```

This is similar to how `rke2 ctr` works -- it provides direct access to the embedded containerd instance for debugging and inspection. Flag parsing is skipped; everything after `ctrctl` is passed through verbatim.

## Examples

```bash
# List running containers
ephemerd ctrctl -n ephemerd containers list

# List snapshots
ephemerd ctrctl -n ephemerd snapshots ls

# Check image status
ephemerd ctrctl -n ephemerd images check

# List running tasks
ephemerd ctrctl -n ephemerd tasks list

# Pull an image manually
ephemerd ctrctl -n ephemerd images pull ghcr.io/actions/actions-runner:latest

# Get containerd version info
ephemerd ctrctl version
```

## Notes

- The `-n ephemerd` flag selects the `ephemerd` namespace, which is where all job containers and images live.
- The socket path is derived from the data directory. Use `--data-dir` (before `ctrctl`) to target a different instance.
- On Linux, the socket is a Unix domain socket at `<data-dir>/containerd/containerd.sock`.
- On Windows, the socket is a named pipe at `\\.\pipe\ephemerd-containerd`.
