---
title: "^# "
---


List OCI container images cached by the embedded containerd.

## Usage

```
ephemerd images [--data-dir <path>]
```

## What it does

Connects to the embedded containerd socket and lists all images in the content store. Shows the image reference (registry/repo:tag) and size.

This is useful for checking which runner and job images are cached locally — cached images don't need to be pulled on the next job, reducing startup time.

## How it works

Connects directly to the containerd socket (`<data-dir>/containerd.sock` on Linux, `\\.\pipe\ephemerd-containerd` on Windows) using the containerd Go client. Does not require the ephemerd daemon to be running — only containerd's data directory.
