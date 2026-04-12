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

## Supported Versions

Security fixes are applied to the latest release only. We do not backport fixes to older versions.
