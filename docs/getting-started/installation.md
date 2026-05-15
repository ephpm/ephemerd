---
title: Installation
weight: 1
---

## Download a release

Download the latest binary for your platform from [GitHub Releases](https://github.com/ephpm/ephemerd/releases).

Binaries are available for Linux (amd64, arm64), macOS (arm64), and Windows (amd64).

## Install

The `install` command copies the binary into place, creates the data directory with a default config file, and registers a system service (systemd, launchd, or Windows service):

```bash
sudo ./ephemerd install
```

This performs three steps:

1. Copies the binary to `/usr/local/bin/ephemerd` (Linux/macOS) or `C:\Program Files\ephemerd\ephemerd.exe` (Windows).
2. Creates the data directory at `/var/lib/ephemerd` (Linux/macOS) or `C:\ProgramData\ephemerd` (Windows) with a starter `config.toml`.
3. Registers and enables the system service.

To skip service registration (for example, if you want to run ephemerd manually):

```bash
sudo ./ephemerd install --no-service
```

## Build from source

ephemerd uses [Mage](https://magefile.org/) as its build system. With Go 1.24+ and Mage installed:

```bash
git clone https://github.com/ephpm/ephemerd.git
cd ephemerd
mage build
```

This downloads embedded dependencies (runner binary, CNI plugins, containerd shim, runc) and compiles the `ephemerd` binary for the current OS.

For Windows (two-stage build that embeds both Windows and Linux components):

```bash
mage build:windows
```

## Uninstall

To remove ephemerd, the binary, system service, and all data:

```bash
sudo ephemerd uninstall
```

This stops the service, removes the service registration, deletes the binary, and removes the data directory.

To keep your configuration and logs (data directory) while removing everything else:

```bash
sudo ephemerd uninstall --keep-data
```
