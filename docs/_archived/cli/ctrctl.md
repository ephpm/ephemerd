# ephemerd ctrctl

Debug the embedded containerd by passing commands through to `ctr`. Similar to `rke2 ctr` from the rke2 world.

## Usage

```
ephemerd ctrctl [ctr-args...]
```

All arguments after `ctrctl` are passed directly to `ctr` with the correct socket path pre-configured.

## Examples

```bash
# List all containers
ephemerd ctrctl containers list

# List snapshots
ephemerd ctrctl snapshots ls

# Inspect an image
ephemerd ctrctl images check

# List tasks (running containers)
ephemerd ctrctl tasks list
```

## How it works

Runs the `ctr` binary (containerd CLI) with the `--address` flag pointing to ephemerd's containerd socket. Does not require the ephemerd daemon to be running — only the containerd socket.
