---
title: Security
weight: 5
---

ephemerd is designed so that CI jobs cannot escape their environment, access the host, or interfere with other jobs. Nearly every isolation mechanism is on by default with no configuration required -- the one exception is **egress filtering on Windows**, which the default NAT network cannot enforce at all and which requires opting into L2Bridge. That exception is spelled out under [Network Firewall](#network-firewall) below; everything else here is the out-of-the-box behavior.

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

The intent is that containers cannot reach private network ranges:

- `10.0.0.0/8` (RFC 1918)
- `172.16.0.0/12` (RFC 1918)
- `192.168.0.0/16` (RFC 1918)
- `169.254.0.0/16` (link-local)

This keeps jobs from scanning or reaching other machines on your LAN, cloud metadata services (169.254.169.254), or other containers, while outbound internet access stays open so jobs can fetch dependencies and talk to external APIs.

**How well that intent holds depends on the platform. Read the Windows section — the default there does not enforce.**

### Linux and macOS — enforced

On Linux this is enforced with iptables rules in the CNI bridge configuration. The container's own subnet (default `10.88.0.0/16`) is excluded so containers can talk to each other.

There are two chains, because Linux routes the two kinds of traffic differently:

- **`EPHEMERD-FORWARD`**, jumped from `FORWARD`, governs traffic the host *routes* on a container's behalf — everything bound for another machine. This is where the four private ranges above are rejected.
- **`EPHEMERD-INPUT`**, jumped from `INPUT`, governs traffic a container addresses to the *host itself* — the bridge gateway (`10.88.0.1`), the host's own LAN address, or any other address the host owns. That traffic is delivered locally and never reaches `FORWARD`, so the rules above do not apply to it and a separate policy is required.

`EPHEMERD-INPUT` is **default deny**. It allows only:

- DNS (udp/tcp 53) to the bridge gateway;
- the gateway ports ephemerd serves to jobs — today that is the Go module proxy's port, added only when `[module_proxy] enabled = true`;
- replies to connections the host itself opened toward a container, and the host's own loopback traffic.

Everything else a container sends to the host is dropped, including the rest of the gateway's ports, the host's LAN address, and the ephemerd control plane. The deny is a rule in ephemerd's own chain rather than a reliance on the host's `INPUT` policy, so a node whose policy is the usual `ACCEPT` is still covered. The Docker API that `dind` exposes to a job is a unix socket bind-mounted into the container on Linux, not a TCP port, so it is unaffected either way.

The gateway allow-list is exposed to hostile job code with no authentication in front of it, so it stays as short as the features in use require. Control-plane ports are ignored rather than opened, even if something asks for them.

IPv6 has no equivalent `INPUT` chain, deliberately: the CNI bridge is IPv4-only, so containers have no v6 address and no v6 path to the host. A v6 default-deny would have to match by destination and would break the host's own link-local traffic.

On a Mac host there are two distinct job types, and they are enforced by two different mechanisms:

- **Linux jobs on a Mac** run as containers inside the Linux VM sidecar. They get the identical in-VM iptables stack described above, and the sidecar is itself NAT-hidden behind the host.
- **macOS-native jobs** run in their own per-job macOS VM, which has no containers and no iptables. Each VM gets a `pfctl` ruleset applied at job start that blocks outbound traffic to the same four ranges, with a carve-out for the Virtualization.framework NAT subnet (`192.168.64.0/24`) so the VM can still reach its gateway and the host.

Both paths block the same ranges; only the tool differs (iptables vs. `pfctl`).

### Windows default (HNS NAT) — NOT enforced

> **Job containers on the default Windows network can reach your entire LAN, including any management interfaces on it.** Treat a Windows runner on the default network as if it were an unfiltered host on your network.

By default, Windows job containers attach to an HNS **NAT** network on the Hyper-V vSwitch. On that path ephemerd installs **no egress filtering at all** -- it logs that container egress is unfiltered and points you at `network.l2bridge_egress`. This is deliberate: no software mechanism on that stack works, which was established by exhaustive testing on real hardware, not inferred. (Earlier versions applied per-endpoint HCN ACL policies on NAT anyway; those were removed in v0.2.0 once they were proven inert, because shipping an inert control is worse than shipping none.)

- **Host WFP filters** (`netsh`, the WFP API, every layer including IPFORWARD and OUTBOUND) never see container egress. WinNAT's translation path does not present the packet to an inspectable filtering layer, even though the packet does traverse `tcpip.sys`.
- **HNS Switch ACLs** are a VFP construct, and VFP is not engaged on a NAT switch. They apply successfully and do nothing.
- **Enabling VFP** on the NAT switch default-denies everything — HNS only programs selective VFP policy for L2Bridge and Overlay.
- **The Hyper-V firewall** (`New-NetFirewallHyperVRule`) has nothing to bind to: Hyper-V-isolated containers on the NAT switch never register a VM creator, so `Get-NetFirewallHyperVVMCreator` returns nothing.
- **`netsh` host rules** are post-NAT. Container traffic is indistinguishable from the host's own by then, so any rule broad enough to catch a container also blackholes the host — including the host's own DNS if your resolver is a LAN address.

There is no configuration that makes the NAT path filterable. If you need enforced egress on Windows, use L2Bridge.

### Windows L2Bridge — the enforcing path

Setting `network.l2bridge_egress = true` moves job containers onto an L2Bridge network, where VFP *is* engaged and the ACL ladder genuinely enforces. This is the same stack Kubernetes Windows CNIs (Calico, Antrea) use in production.

```toml
[network]
l2bridge_egress = true
host_nic        = "Ethernet 2"          # dedicated NIC to bridge onto (see below)
ip_pool         = "192.0.2.192/27"      # reserved range, see below
```

**A reserved `ip_pool` your DHCP server will never lease is mandatory.** On L2Bridge, containers are addressed on your LAN rather than behind NAT, so ephemerd must be told which addresses it may hand out. There is deliberately **no default** — any built-in guess would collide with live DHCP leases. Size it for at least `runner.max_concurrent` containers, and add the matching exclusion on your DHCP server *before* enabling. Container egress ACLs rewrite each container's source MAC to the host NIC's, so the containers appear on the segment as the host itself.

**A dedicated NIC is strongly recommended.** Creating the L2Bridge builds an external Hyper-V vSwitch on `host_nic` and migrates its IP onto a `vEthernet (<name>)` adapter. If that is the host's only NIC — the one you administer it over — there is a connectivity blip during creation, and a failure mid-creation can leave a remote node unreachable. A second NIC dedicated to container traffic keeps the management path untouched. ephemerd tolerates the vSwitch rename, so once the network exists, daemon restarts are safe. Wi-Fi adapters are untested and not recommended: Hyper-V external switches on 802.11 historically require bridging workarounds.

`subnet`, `gateway`, and DNS are derived from `host_nic` at startup; set them explicitly only to override. Startup fails fast with a message naming the offending key rather than guessing.

**Understand the trade-offs before enabling:**

- Containers hold **real LAN addresses** and are routable L2 peers of your network. The ACLs are load-bearing — a mis-scoped pool or block list means full LAN access, a worse blast radius than NAT.
- The block list covers the container's own subnet and its default gateway. Containers still *route* through the gateway but cannot *address* it.
- If `dind` or the Go module proxy is enabled, containers must be able to reach the host (that is how they reach the Docker API and `GOPROXY`), so a `/32` allow for the host is added automatically. Because a port-scoped Switch ACL disables the whole VFP port, that allow covers **all** host ports — so do not run anything on a Windows runner host that you would not expose to job containers. With both features disabled, the host stays blocked.
- **Migrating an existing node requires a reboot**, not just a service restart: creating an L2Bridge network beside a live NAT network leaves HNS in a broken state. Reserve the pool, drain the node, set the keys, then reboot.
- Anti-spoofing is not currently enforced — a container can forge a source address on the segment.

#### What L2Bridge contains, and what it does not

L2Bridge enforces where a container may send **unicast IP traffic**. The VFP ACL ladder blocks the RFC1918 ranges, the container's own subnet, and the default gateway, and job containers cannot address other machines on your LAN. That is the guarantee, and it holds.

Because the host's `/32` allow cannot be port-scoped at the VFP layer, ephemerd adds a host-firewall backstop that blocks the dangerous Windows management ports from the container pool, on **both TCP and UDP**: 135 (RPC endpoint mapper), 139, 445 (SMB), 3389 (RDP, which negotiates a UDP transport on the same number), 5985/5986 (WinRM) and 47001 (WSMan). It also blocks the broadcast/multicast name-resolution protocols from the pool — NBNS (UDP 137), the NetBIOS datagram service (138), mDNS (5353) and LLMNR (5355) — so a job cannot poison the *host's* name resolution or answer its queries. The per-job `dind` allow is unaffected: it is a single TCP port scoped to the owning container's `/32`.

What none of that contains:

> **A job container on L2Bridge is a real L2 peer of your network for broadcast and multicast purposes.** It shares a broadcast domain with every other device on the segment. It can emit ARP, LLMNR, NBNS and mDNS onto that segment and see the broadcast and multicast traffic on it — including other hosts' name-resolution queries. The classic responder-style attack (answer a query first, capture a Net-NTLMv2 challenge/response from the asker, relay or crack it offline) is available against **other machines on the segment**, not against the ephemerd host.

This is not a bug in the rule set and no host-side rule can fix it: a host firewall on the ephemerd node never sees traffic between two *other* L2 peers. It is inherent to putting containers on the LAN, which is the same property that makes VFP egress enforcement possible in the first place.

**An isolated VLAN for the container pool is the real mitigation for that class**, and it pairs with L2Bridge rather than replacing it. Put `host_nic` on a VLAN (or a physically separate segment) that carries nothing but the container pool and the host's bridged address, with an uplink that denies RFC1918. The broadcast domain then contains only job containers, which have nothing to poison, and the VLAN's uplink policy backstops the VFP ladder with a control the job cannot reach at all. If your Windows runners handle untrusted code — public forks, third-party contributors — treat the VLAN as required rather than optional.

### Local runs

`ephemerd run` uses whatever network the host's `config.toml` specifies, so a local run on an L2Bridge host is filtered the same way a real job is. With no config, or on a Windows host without `l2bridge_egress`, it falls back to the default NAT network and logs a warning that the job's egress is unfiltered.

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
