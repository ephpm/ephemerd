---
title: Security
weight: 5
---

ephemerd is designed so that CI jobs cannot escape their environment, access the host, or interfere with other jobs. Every isolation mechanism is on by default with no configuration required.

## Ephemeral Environments

Each job gets a fresh environment created at job start and destroyed on completion. No state carries over between jobs -- no leftover files, processes, environment variables, or network connections. This eliminates an entire class of supply-chain attacks where a compromised job poisons the environment for subsequent runs.

On Linux and Windows, this means a new container per job. On macOS, each job gets a clone-on-write copy of the base VM disk image that is deleted after the job finishes.

## Kernel-Level Isolation

### Linux: Namespaces and Cgroups

Linux containers run with standard namespace isolation (PID, mount, network, UTS, IPC) and cgroup resource limits enforced by runc. Each container has its own process tree, filesystem view, and network stack.

### Windows: Hyper-V Isolation

Windows containers run with Hyper-V isolation, meaning each container gets its own Windows kernel instance. This is stronger than process-level isolation -- a kernel exploit in one container cannot reach another container or the host. Hyper-V isolation is the default and only supported mode for ephemerd on Windows.

### macOS: Virtualization.framework

macOS jobs run in full virtual machines via Apple's Virtualization.framework. Each VM has its own kernel, memory space, and virtual hardware. The VM is destroyed after the job completes.

## Network Firewall

By default, containers are blocked from reaching private network ranges:

- `10.0.0.0/8` (RFC 1918)
- `172.16.0.0/12` (RFC 1918)
- `192.168.0.0/16` (RFC 1918)
- `169.254.0.0/16` (link-local)

This prevents jobs from scanning or accessing other machines on your local network, cloud metadata services (169.254.169.254), or other containers. Outbound internet access is allowed so jobs can fetch dependencies, push artifacts, and interact with external APIs.

On Linux, these rules are enforced via iptables in the CNI bridge configuration. On Windows, per-endpoint HCN ACL policies block the same ranges.

The container's own subnet (default `10.88.0.0/16`) is excluded from the block list so containers can communicate with their gateway for outbound NAT.

## Capability Restrictions

Linux containers run with a minimal set of capabilities:

| Capability | Purpose |
|-----------|---------|
| `CAP_CHOWN` | dpkg chown on installed files |
| `CAP_DAC_OVERRIDE` | Write to directories owned by other users |
| `CAP_FOWNER` | chmod/utimes on files not owned by process |
| `CAP_FSETID` | Preserve SUID/SGID bits (sudo, passwd) |
| `CAP_KILL` | Signal processes (service restarts) |
| `CAP_SETGID` | adduser/addgroup in package scripts |
| `CAP_SETUID` | setuid in package scripts |
| `CAP_SYS_CHROOT` | chroot in package scripts |
| `CAP_NET_BIND_SERVICE` | Bind to ports below 1024 |

Notably absent are `CAP_SYS_ADMIN` (no mount, no BPF, no namespace manipulation), `CAP_NET_ADMIN` (no network reconfiguration), `CAP_NET_RAW` (no raw sockets), and `CAP_MKNOD` (no creating device nodes). This set covers `apt-get install`, `sudo`, `adduser`, and service management -- the operations CI jobs commonly need -- while blocking privilege escalation paths.

`CAP_MKNOD` is worth calling out because container runtimes commonly grant it. Without it a job cannot create a block device node for the host disk and read it raw. The container's device cgroup denies access to any such node anyway, so dropping the capability removes the reliance on that single layer rather than closing an otherwise-open hole. No package in the supported runner images needs it at install time.

## Seccomp Profiles

On Linux, containers run with containerd's default seccomp profile. This blocks dangerous syscalls including:

- `mount`, `umount2` -- no filesystem mounting
- `ptrace` -- no process tracing or debugging other processes
- `bpf` -- no eBPF programs
- `kexec_load`, `kexec_file_load` -- no kernel replacement
- `reboot` -- no host reboot
- `sethostname`, `setdomainname` -- no hostname changes
- `init_module`, `finit_module` -- no kernel module loading

The profile allows all syscalls that standard CI operations need (process creation, file I/O, networking, signal handling).

## AppArmor Profiles

On Linux, ephemerd also confines job containers with a generated AppArmor profile named `ephemerd-default`, equivalent to what Docker installs as `docker-default`. Seccomp filters syscalls; AppArmor constrains file operations, which syscall filtering does not distinguish between. Among other things the profile denies:

- Writes to most of `/proc/sys` and `/sys`
- Writes to `/proc/sysrq-trigger`, and any access to `/proc/mem`, `/proc/kmem`, `/proc/kcore`
- Access to `/sys/firmware` and `/sys/kernel/security`
- `mount`, independently of the seccomp filter
- `ptrace` of processes outside the container's own profile

Two carve-outs are deliberate and inherited from the upstream template: `/sys/fs/cgroup/**` and `/proc/sys/kernel/shm*` remain writable, because containerized workloads legitimately need both.

This is a defense-in-depth layer, not the only thing protecting kernel interfaces. The container's OCI spec already mounts `/sys` read-only, marks `/proc/sys` and `/proc/sysrq-trigger` read-only, masks `/proc/kcore` and friends, and installs a deny-all device cgroup. AppArmor is a fourth, independent layer behind those, seccomp, and the capability set.

### Fail-open behavior

AppArmor confinement is best-effort. If the host cannot enforce it, ephemerd logs the reason and starts the job **unconfined by AppArmor** rather than failing the job. Hard-failing would take an entire pool offline on any host that simply does not ship AppArmor -- a common case, since AppArmor is a Debian/Ubuntu/SUSE feature and is absent or disabled on RHEL-family hosts, which use SELinux instead. Every other layer (seccomp, capabilities, read-only mounts, device cgroup, namespaces) still applies.

Confinement is skipped when any of the following is true:

- The kernel has AppArmor disabled (`/sys/module/apparmor/parameters/enabled` is not `Y`)
- securityfs is not mounted at `/sys/kernel/security/apparmor`
- `apparmor_parser` is not installed
- Loading the profile fails
- The profile is present but loaded in `complain` mode, which logs violations instead of blocking them

### Checking whether your jobs are confined

ephemerd logs one line whenever confinement status changes -- not once per job, so a healthy node prints it once, when it creates its first job container, and then stays quiet. Confined:

```
level=INFO msg="job containers are AppArmor confined" profile=ephemerd-default mode=enforce
```

Unconfined, with the specific reason:

```
level=WARN msg="job containers are running WITHOUT AppArmor confinement; seccomp, capability limits and the read-only /proc,/sys mounts still apply" profile=ephemerd-default reason="apparmor_parser is not installed or not in PATH"
```

If you see the `WARN`, install the `apparmor` and `apparmor-utils` packages and restart ephemerd. On a host that is confined, `aa-status` lists `ephemerd-default` in enforce mode.

Note that the profile is loaded into the kernel at runtime and is not persisted to `/etc/apparmor.d`. Reloading AppArmor (`systemctl reload apparmor`) or upgrading the apparmor package drops it; ephemerd detects this before the next container starts and reloads the profile automatically.

## No Host Access

Containers have no access to the host environment:

- **No Docker socket.** There is no `/var/run/docker.sock` mounted into containers. Jobs cannot start sibling containers or interact with the host's container runtime. (When the fake Docker socket feature is enabled for Forgejo/Gitea, it intercepts Docker API calls and translates them into sandboxed containerd operations rather than providing real Docker access.)
- **No host filesystem.** The container's root filesystem is an overlayfs snapshot of the OCI image. The only host path mounted into the container is the runner binary directory, and it is used read-only for runner execution.
- **No privileged mode.** Containers are never started with `--privileged` or equivalent. The OCI spec does not include elevated privileges.
- **No host networking.** Each container gets its own network namespace with a veth pair connected to a bridge (Linux) or an HCN endpoint connected to a NAT network (Windows). The container cannot see or interact with host network interfaces.
