---
title: Security
weight: 7
---

Every job runs in full isolation:

- **Ephemeral environments** — created per job, destroyed after. No state leaks between jobs.
- **Hyper-V isolation on Windows** — each container gets its own kernel. Real VM-level isolation.
- **Network firewall** — containers are blocked from RFC 1918 and link-local ranges by default. Jobs can reach the internet but not your LAN.
- **Read-only runner mount** — the GitHub Actions runner binary is bind-mounted read-only.
- **No host access** — no Docker socket, no host filesystem, no privileged mode.
