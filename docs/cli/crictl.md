---
title: crictl
weight: 17
---

Access the embedded containerd CRI (Container Runtime Interface) endpoint. Runs `crictl` commands in-process against ephemerd's containerd -- no external `crictl` binary required.

## Usage

```
ephemerd crictl [crictl-args...]
```

All arguments after `crictl` are passed directly to the in-process crictl implementation.

## Examples

```bash
# List running containers
ephemerd crictl ps

# List all containers (including stopped)
ephemerd crictl ps -a

# List images
ephemerd crictl images

# Get containerd info
ephemerd crictl info

# Inspect a container
ephemerd crictl inspect <container-id>

# View container logs
ephemerd crictl logs <container-id>
```

## How it works

Unlike a `ctr`-style passthrough that shells out to an external binary, `crictl` is linked in-process using the upstream crictl library from `github.com/kubernetes-sigs/cri-tools`. It connects to ephemerd's containerd CRI socket directly. No external binary is needed.

The CRI interface is the same API that Kubernetes uses to manage containers, so the output format and commands will be familiar if you've debugged containers on a Kubernetes node.

## See also

- [Architecture: CRI Passthrough](/architecture/cri-passthrough/) -- design details
