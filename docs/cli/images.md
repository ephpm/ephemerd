---
title: images
weight: 10
---

List cached OCI container images from the embedded containerd instance.

```
ephemerd images
```

## Behavior

Connects directly to containerd's socket (not via the daemon's gRPC control API) and lists all images in the `ephemerd` namespace.

### Example output

```
IMAGE                                                        SIZE
ghcr.io/actions/actions-runner:latest                        1.2 GB
mcr.microsoft.com/windows/servercore:ltsc2022                5.8 GB
```

If no images are cached, prints "No cached images."

## Notes

- The containerd socket path is derived from the data directory (set via `--data-dir` or `EPHEMERD_DATA_DIR`).
- On Linux, the socket is a Unix domain socket.
- On Windows, the socket is a named pipe (`\\.\pipe\ephemerd-containerd`).
- Images are pulled automatically when a job starts if not already cached. This command shows what is currently available locally.
