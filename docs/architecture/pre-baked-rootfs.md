---
title: Pre-Baked Rootfs
weight: 7
---

The WSL and macOS Linux VM rootfs is an Alpine minirootfs with gcompat and iptables baked in at compile time. This eliminates network-dependent package installation during boot.

## Context

Every WSL distro boot (and Vz Linux VM boot) needs two packages that are not in the stock Alpine minirootfs:

- **gcompat** -- glibc compatibility shim required by `containerd-shim-runc-v2`, which is built against glibc.
- **iptables** -- required by CNI plugins for container network NAT rules.

Previously these were installed at runtime via `apk add --no-cache gcompat iptables` after each distro import. This had several problems:

- 10-30s of boot time spent downloading and installing packages over the network.
- DNS flakes -- WSL networking is not always ready immediately after distro import, requiring a retry loop with backoffs.
- The only network-dependent step in the entire distro boot sequence.
- Multiplied cost -- `ephemerd run` creates a fresh distro per invocation, paying this penalty every time.

## How It Works

`mage download:rootfs` (implemented in `mage/download/download.go`) builds the rootfs at compile time:

1. Downloads `alpine-minirootfs-{version}-x86_64.tar.gz` from the Alpine CDN.
2. Downloads 7 `.apk` files (gcompat, iptables, and their transitive dependencies) from the same CDN.
3. Creates `pkg/vm/embed/ephemerd-rootfs-{version}-x86_64.tar.gz` by:
   - Copying all tar entries from the base minirootfs.
   - Extracting the data section from each APK and appending those entries.

No container runtime is required -- the build is pure Go using `archive/tar` and `compress/gzip`.

The APK format is straightforward: each `.apk` is 2-3 concatenated gzip streams (signature, control metadata, data). The data stream is a tar containing the actual filesystem files. The code walks through the gzip streams using `gz.Multistream(false)` + `gz.Reset(br)`, skipping metadata entries, and copies the data entries into the output tarball.

## Package List

Pinned to Alpine 3.21.3 -- update when bumping `AlpineVersion` in `mage/download/download.go`:

| Package | Why |
|---------|-----|
| `gcompat` | glibc compat layer for containerd-shim-runc-v2 |
| `libucontext` | dependency of gcompat |
| `musl-obstack` | dependency of gcompat |
| `iptables` | CNI plugin networking (NAT rules) |
| `libxtables` | dependency of iptables |
| `libmnl` | dependency of iptables |
| `libnftnl` | dependency of iptables |

All packages are in the `main` Alpine repo. Versions are discovered from the APKINDEX.

## Output

```
pkg/vm/embed/ephemerd-rootfs-3.21.3-x86_64.tar.gz   (~4.1 MB)
```

Embedded via `//go:embed embed/ephemerd-rootfs-*.tar.gz` in `pkg/vm/embed_windows.go` (and `embed_darwin.go` for macOS).

## Runtime Impact

Before:

```
importDistro -> mkdir -> apk add (retry x3, 2min timeout) -> launch
```

After:

```
importDistro -> mkdir -> launch
```

The rootfs already has everything needed. No network access required during boot.

## Adding New Packages

1. Find the package name and version in the [Alpine package database](https://pkgs.alpinelinux.org/packages).
2. Add an entry to `rootfsPackages` in `mage/download/download.go` with name, version, and repo (`main` or `community`).
3. Include any transitive dependencies not already listed.
4. Delete the old `ephemerd-rootfs-*.tar.gz` from `pkg/vm/embed/` and re-run `mage download:rootfs`.
