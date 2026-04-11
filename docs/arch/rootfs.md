# Pre-baked WSL Rootfs: Alpine + gcompat + iptables

## Context

Every WSL distro boot (both `serve` and `run` paths) needs two packages that aren't in the stock Alpine minirootfs:

- **gcompat** — glibc compatibility shim required by `containerd-shim-runc-v2`, which is built against glibc
- **iptables** — required by CNI plugins for container network NAT rules

Previously these were installed at runtime via `apk add --no-cache gcompat iptables` after each WSL distro import. This had several problems:

- **10-30s of boot time** spent downloading and installing packages over the network
- **DNS flakes** — WSL networking isn't always ready immediately after distro import, requiring a retry loop with 3 attempts and 3-second backoffs
- **The only network-dependent step** in the entire distro boot sequence
- **Multiplied cost** — `ephemerd run` creates a fresh distro per invocation, paying this penalty every time

## Solution

Build the rootfs at compile time by downloading the stock Alpine minirootfs and the individual `.apk` package files from the Alpine CDN, then combining them into a single tarball. No container runtime required — it's pure Go using `archive/tar` and `compress/gzip`.

## How It Works

`mage download:rootfs` (implemented in `mage/download/download.go`) does:

1. Downloads `alpine-minirootfs-{version}-x86_64.tar.gz` from the Alpine CDN
2. Downloads 7 `.apk` files (gcompat, iptables, and their transitive dependencies) from the same CDN
3. Creates `pkg/vm/embed/ephemerd-rootfs-{version}-x86_64.tar.gz` by:
   - Copying all tar entries from the base minirootfs
   - Extracting the data section from each APK and appending those entries

The APK format is straightforward: each `.apk` is 2-3 concatenated gzip streams (signature, control metadata, data). The data stream is a tar containing the actual filesystem files. The code walks through the gzip streams using `gz.Multistream(false)` + `gz.Reset(br)`, skipping metadata entries (names starting with `.` but not `./`), and copies everything else into the output tarball.

### Package List

Pinned to Alpine 3.21.3 — update when bumping `AlpineVersion`:

| Package | Why |
|---------|-----|
| `gcompat` | glibc compat layer for containerd-shim-runc-v2 |
| `libucontext` | dependency of gcompat |
| `musl-obstack` | dependency of gcompat |
| `iptables` | CNI plugin networking (NAT rules) |
| `libxtables` | dependency of iptables |
| `libmnl` | dependency of iptables |
| `libnftnl` | dependency of iptables |

These are all in the `main` Alpine repo. Versions are discovered from the APKINDEX.

## Output

```
pkg/vm/embed/ephemerd-rootfs-3.21.3-x86_64.tar.gz   (~4.1 MB)
```

Embedded via `//go:embed embed/ephemerd-rootfs-*.tar.gz` in `pkg/vm/embed_windows.go`.

## What Changed at Runtime

Before:
```
importDistro → mkdir -p /var/lib/ephemerd → apk add (retry x3, 2min timeout) → launch
```

After:
```
importDistro → mkdir -p /var/lib/ephemerd → launch
```

The `installEphemerd()` and `NewRunDistro()` functions no longer run `apk add`. The rootfs already has everything needed.

## Adding New Packages

1. Find the package name and version in the [Alpine package database](https://pkgs.alpinelinux.org/packages)
2. Add an entry to `rootfsPackages` in `mage/download/download.go` with name, version, and repo (`main` or `community`)
3. Include any transitive dependencies not already listed
4. Delete the old `ephemerd-rootfs-*.tar.gz` from `pkg/vm/embed/` and re-run `mage download:rootfs`
