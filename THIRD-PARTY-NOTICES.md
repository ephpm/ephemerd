# Third-party notices

## Scope

The MIT license in this repository applies to ephemerd's own source code.
It does NOT apply to everything inside the release binaries. An ephemerd
release binary is an aggregate: it embeds (via `go:embed`) prebuilt
third-party components that are downloaded at build time
(`mage/download/download.go` pins the versions), each distributed under its
own license, listed below.

Redistributing an ephemerd release binary means redistributing these
components. Several of the licenses below (notably GPL-2.0) require making
the corresponding source code available — see "Source availability" at the
end. This file is a good-faith summary for orientation and is not legal
advice.

## Embedded components

The Linux VM boot artifacts (used on Windows and macOS hosts to run Linux
jobs in a VM) come from Alpine Linux:

- **Linux kernel** (`vmlinuz`, plus kernel modules in the initrd) —
  **GPL-2.0-only** (with the Linux syscall exception).
  Taken from the Alpine Linux `linux-virt` package; Alpine's kernel source
  and patches are at <https://gitlab.alpinelinux.org/alpine/aports>
  (`community/linux-virt`), upstream at <https://kernel.org>.
- **BusyBox** (in the initrd) — **GPL-2.0-only**.
  <https://busybox.net>; Alpine package source at
  <https://gitlab.alpinelinux.org/alpine/aports>.
- **e2fsprogs** (in the initrd, for formatting the VM data disk) —
  **GPL-2.0** (libraries under LGPL-2.0 / MIT).
  <http://e2fsprogs.sourceforge.net>.
- **Alpine Linux minirootfs** (the VM root filesystem, plus the `gcompat`
  and `iptables` packages added to it) — an aggregate of packages each under
  its own license, including:
  - **musl libc** — **MIT**. <https://musl.libc.org>
  - **BusyBox** — **GPL-2.0-only** (as above)
  - **apk-tools, alpine-baselayout** and other Alpine base packages —
    predominantly **GPL-2.0**. <https://gitlab.alpinelinux.org/alpine>
  - **iptables** — **GPL-2.0**. <https://netfilter.org>
  - **gcompat** — **NCSA/MIT-style**. <https://git.adelielinux.org/adelie/gcompat>

Container runtime and CI components embedded on all platforms that use them:

- **runc** — **Apache-2.0**. <https://github.com/opencontainers/runc>
- **containerd shim** (`containerd-shim-runc-v2`) — **Apache-2.0**.
  <https://github.com/containerd/containerd>
- **CNI plugins** — **Apache-2.0**.
  <https://github.com/containernetworking/plugins>
- **GitHub Actions runner** — **MIT**.
  <https://github.com/actions/runner>

In addition, ephemerd links containerd and many other Go modules as
libraries; their licenses (predominantly Apache-2.0, MIT, and BSD) are
declared in `go.mod` and can be inventoried with a tool such as
`go-licenses` against this repository.

## Source availability

The GPL-licensed components above are embedded as unmodified binaries taken
from the Alpine Linux package repositories at the versions pinned in
`mage/download/download.go`. Their complete corresponding source code is
available from Alpine Linux at <https://dl-cdn.alpinelinux.org/alpine/> and
<https://gitlab.alpinelinux.org/alpine/aports> for the matching Alpine
release, and from each upstream project linked above. If you are unable to
obtain the corresponding source for a GPL component embedded in an ephemerd
release, open an issue on this repository and we will provide it.
