# Security Policy

## Reporting a Vulnerability

**Please do not open public GitHub issues for security vulnerabilities.**

Report vulnerabilities privately through GitHub's built-in security reporting:

1. Go to the [Security](https://github.com/ephpm/ephemerd/security) tab of this repository
2. Click **"Report a vulnerability"**
3. Fill out the advisory form

This uses [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability), which keeps the report visible only to maintainers until a fix is released.

Include as much of the following as you can:

- Description of the vulnerability
- Steps to reproduce
- Affected platforms (Linux, Windows, macOS)
- Impact assessment (what an attacker could achieve)

We will acknowledge reports within 7 days and aim to release a fix within 30 days for critical issues.

## Security Model

ephemerd runs untrusted CI/CD workloads. Its security model assumes that **code running inside a job is hostile** and must be prevented from:

1. **Escaping the container/VM** — reaching the host filesystem, processes, or other jobs
2. **Accessing internal networks** — containers are firewalled from RFC 1918 and link-local ranges
3. **Escalating privileges** — containers run with a minimal capability set, no `CAP_SYS_ADMIN` or `CAP_NET_ADMIN`
4. **Persisting after completion** — job environments are destroyed immediately after the runner exits

### Platform-Specific Isolation

| Platform | Isolation Mechanism |
|----------|-------------------|
| Linux | OCI containers with seccomp profiles, restricted capabilities, CNI bridge firewall |
| Windows | Hyper-V isolated containers (separate kernel per job), HCN per-endpoint ACL policies |
| macOS | Virtualization.framework VMs with APFS copy-on-write snapshots |

### What Is In Scope

- Container/VM escape (gaining access to host or other jobs)
- Network isolation bypass (reaching RFC 1918, link-local, or host-only addresses from a job)
- Credential leakage (JIT tokens, GitHub App keys, or PATs exposed to unauthorized parties)
- Privilege escalation (gaining capabilities beyond the allowed set)
- Denial of service against the daemon itself (not resource exhaustion from legitimate jobs)
- Authentication/authorization bypass in the gRPC control API

### What Is Out of Scope

- A job reading its own JIT runner token (by design — the runner needs it)
- Resource exhaustion from jobs within configured concurrency limits
- Vulnerabilities in the GitHub Actions runner binary itself (report to [actions/runner](https://github.com/actions/runner))
- Vulnerabilities in containerd, runc, or runhcs (report to their respective projects)
- Social engineering or phishing attacks

## Operator Responsibilities and Prerequisites

ephemerd's isolation model has hard dependencies on the surrounding
configuration. The following are the operator's responsibility, not ephemerd's:

### Require approval for outside collaborators (GitHub)

ephemerd dispatches a runner for any `workflow_job` webhook that targets a
`self-hosted` label in a tracked repo. **The primary control that keeps a
fork/pull-request from an untrusted author off your self-hosted runners is
GitHub's own setting**, "Require approval for all outside collaborators" (or
stricter) under *Settings → Actions → General → Fork pull request workflows*.
Enable it. Without it, a fork PR can run its workflow on your runner the moment
it is opened.

As defense-in-depth, ephemerd also supports an **optional dispatch allowlist**
(`[github].dispatch_policy`) that gates which webhook jobs may dispatch a
runner:

```toml
[github.dispatch_policy]
allowed_repos   = ["my-repo"]   # only these repos may dispatch (default: all tracked repos)
required_labels = ["ephemerd"]  # a job must carry one of these labels (default: none)
```

Both fields default to empty (no restriction), preserving existing behavior.
This is a safety net layered *under* the GitHub approval gate, not a substitute
for it.

### Firewall the host<->VM dispatch port from job containers

On Windows (Hyper-V) and macOS (Vz) hosts, Linux jobs run in a long-lived Linux
VM and the host drives job lifecycle over a gRPC dispatch channel
(`CreateJob`/`WaitJob`/`DestroyJob`). That channel:

- **Requires a shared bearer token** (auto-generated and stored as
  `[dispatch].token` in `config.toml`, delivered to the VM through the same
  channel as the rest of the config). Every RPC is checked with a constant-time
  comparison; an absent or wrong token is rejected with `Unauthenticated`.
- **Must still be firewalled off from job containers.** The token authenticates
  the host, but the port should not be reachable by workloads at all. The
  in-VM worker installs bridge control-port firewall rules for exactly this;
  operators running a custom network topology must ensure job containers cannot
  reach the dispatch port.

### Do not expose the metrics endpoint

`[metrics]` is disabled by default. When enabled it is **unauthenticated** and
now binds to `127.0.0.1` by default (`[metrics].bind_addr`). If you scrape from
another host, set `bind_addr = "0.0.0.0"` *and* firewall the port and/or front
it with TLS (`tls_cert`/`tls_key`).

### Privileged Docker-in-Docker is opt-in

`docker run --privileged` from inside a job (fake-Docker/DinD) is **rejected by
default** on all platforms (`[dind].allow_privileged = false`). A privileged
sibling container is effectively root on whatever host runs the backing
containerd — the VM on Windows/macOS, the bare host on Linux. Only enable it
when every workload is trusted.

> **Future work (not yet implemented):** scoping privileged workloads to a
> dedicated, separately-isolated runner pool so that enabling privileged DinD
> for one repo does not widen the blast radius for others. Today the mitigation
> is the default-off posture above plus the per-platform VM fence.

## Supply-Chain Integrity

Binaries embedded into or executed inside runner VMs are pinned by SHA256 and
verified after download (build fails hard on mismatch): the GitHub Actions
runner, CNI plugins, the containerd distribution tarball, and runc. Hashes live
in `mage/download/download.go` (`pinnedSHA256`) and are refreshed from the
upstream published checksums whenever a version constant is bumped.

> **Known gap:** the Alpine base minirootfs and APK packages fetched from
> `dl-cdn.alpinelinux.org` are not yet SHA256-pinned, because that CDN prunes
> superseded revisions within weeks (pinning requires mirroring them first, as
> is already done for the `linux-virt` kernel APK). These are marked
> `TODO(security)` in the code and tracked for follow-up.

## Supported Versions

Security fixes are applied to the latest release only. We do not backport fixes to older versions.
