---
title: How It Works
weight: 2
---

## Linux

Containers run directly on the host via the embedded containerd. No VM needed — fastest path.

```mermaid
graph TB
    GH[GitHub] -->|webhook / poll| E[ephemerd]

    subgraph "Linux Host"
        E -->|create container| CTD[containerd — embedded]
        CTD -->|OCI container| R[Runner + Job]
        R -->|job complete| E
        E -->|destroy container| CTD
    end
```

## Windows

Windows jobs run in Hyper-V isolated containers (each gets its own kernel). Linux jobs are dispatched to a WSL2 distro via gRPC — ephemerd embeds an Alpine rootfs and a cross-compiled Linux binary, imports a WSL distro on startup, and runs containerd inside it. The Windows host runs a single scheduler that routes jobs by OS label.

```mermaid
graph TB
    GH[GitHub] -->|webhook| E[ephemerd.exe]

    subgraph "Windows Host"
        E -->|Windows job| CTD[containerd native]
        CTD -->|Hyper-V container| WR[Windows Runner]

        E -->|Linux job via gRPC| WSL[WSL2 distro — Alpine]
        WSL -->|containerd in WSL| LC[OCI Container]
        LC --> LR[Linux Runner]
    end
```

## macOS

A long-running lightweight Linux VM (via Apple's Virtualization.framework) hosts containerd for Linux jobs — same OCI images, same Dockerfiles. macOS-native jobs (Xcode, Swift) get their own ephemeral macOS VM cloned from a base image via APFS copy-on-write (instant, no data copied until writes occur).

```mermaid
graph TB
    GH[GitHub] -->|webhook| E[ephemerd]

    subgraph "macOS Host (Apple Silicon)"
        E -->|Linux job| LVM[Linux VM — Virtualization.framework]
        LVM -->|containerd in VM| LC[OCI Container]
        LC --> LR[Linux ARM64 Runner]

        E -->|macOS job| MVM[macOS VM — APFS clone-on-write]
        MVM --> MR[macOS Runner + Xcode]
    end
```

## Dual-Purpose Hosts

A single machine can serve multiple job types:

| Host | Linux jobs | Native OS jobs |
|------|-----------|----------------|
| Linux x86_64 | containerd direct | — |
| Linux arm64 | containerd direct | — |
| Windows x86_64 | Hyper-V Linux VM | Hyper-V Windows containers |
| macOS arm64 | Virtualization.framework Linux VM | macOS VM (clone-on-write) |

**A Windows box and a Mac Mini covers every combination:** linux/amd64, linux/arm64, windows/amd64.
