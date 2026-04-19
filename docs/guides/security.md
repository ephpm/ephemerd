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
| `CAP_MKNOD` | Create device nodes (some packages) |
| `CAP_SYS_CHROOT` | chroot in package scripts |
| `CAP_NET_BIND_SERVICE` | Bind to ports below 1024 |

Notably absent are `CAP_SYS_ADMIN` (no mount, no BPF, no namespace manipulation), `CAP_NET_ADMIN` (no network reconfiguration), and `CAP_NET_RAW` (no raw sockets). This set covers `apt-get install`, `sudo`, `adduser`, and service management -- the operations CI jobs commonly need -- while blocking privilege escalation paths.

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

## No Host Access

Containers have no access to the host environment:

- **No Docker socket.** There is no `/var/run/docker.sock` mounted into containers. Jobs cannot start sibling containers or interact with the host's container runtime. (When the fake Docker socket feature is enabled for Forgejo/Gitea, it intercepts Docker API calls and translates them into sandboxed containerd operations rather than providing real Docker access.)
- **No host filesystem.** The container's root filesystem is an overlayfs snapshot of the OCI image. The only host path mounted into the container is the runner binary directory, and it is used read-only for runner execution.
- **No privileged mode.** Containers are never started with `--privileged` or equivalent. The OCI spec does not include elevated privileges.
- **No host networking.** Each container gets its own network namespace with a veth pair connected to a bridge (Linux) or an HCN endpoint connected to a NAT network (Windows). The container cannot see or interact with host network interfaces.
