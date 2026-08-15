# Windows container egress: WFP IPFORWARD investigation (negative result)

> **Addendum (2026-08, resolved).** A host-side software path was subsequently
> found and shipped: HNS **L2Bridge** networks with per-endpoint **VFP Switch
> ACLs** (`network.l2bridge_egress`), proven on metal. The container is a peer on
> the host LAN (no NAT), so the VFP dataplane filters its egress at the vSwitch
> port. The "network-level isolation only" conclusion below therefore applies
> specifically to the default **NAT** stack this investigation used — where it
> still holds, and where ephemerd now installs nothing and logs that egress is
> unfiltered — **not universally**. See `docs/guides/security.md`. The body below
> is preserved unchanged as the historical record for the NAT stack.

## TL;DR

On Windows Server 2025 (build 26100) with a Hyper-V-isolated job container on an
HNS **NAT** network, **host-side Windows Filtering Platform (WFP) filters cannot
enforce container egress.** The container's forwarded + SNAT'd traffic does not
traverse the host WFP engine at any inspectable IPv4 layer — not
`FWPM_LAYER_IPFORWARD_V4`, not `FWPM_LAYER_INBOUND_IPPACKET_V4`, and not
`FWPM_LAYER_OUTBOUND_IPPACKET_V4`. WFP is provably functional on the host (it
blocks the host's own traffic), so this is not a "filter didn't apply" problem;
the packets are handled inside the Hyper-V virtual-switch / HNS datapath, which
is beside the host's `tcpip.sys` WFP hooks.

This closes a fifth mechanism after four prior failures (see below). The
conclusion for the fleet is that **Windows container egress restriction requires
network-level isolation (a VLAN / managed switch), not a host-side software
filter.** This is the one platform where ephemerd cannot reach Linux parity
(nftables `FORWARD`) purely in the daemon.

This document exists so nobody spends a sixth night rediscovering it. It records
what was tried, the exact evidence, and a reproducer.

## Why WFP IPFORWARD looked like the answer

ephemerd blocks container→RFC1918 on Linux in the `FORWARD` chain
(`pkg/networking/firewall_linux.go`): the host routes+NATs for the container, so
the FORWARD hook is where container-originated egress is catchable. The Windows
analogue of the Linux FORWARD chain is `FWPM_LAYER_IPFORWARD_V4`. Every prior
Windows attempt sat at a layer this forwarded/NAT'd path skips:

| # | Mechanism | Why it failed on metal |
|---|-----------|------------------------|
| 1 | Host Windows Firewall, source-scoped `localip=10.88/16` (PR #136) | WinNAT rewrites the source before the host ALE layer; a source-scoped rule matches nothing. Also disqualified: a host-level rule would block the node's own RFC1918 (Prometheus scrapes from 192.168.10.45). |
| 2 | `New-NetFirewallHyperVRule` scoped by container `VMCreatorId` (PR #140) | No container VMCreator exists on this host, even with a live container. |
| 3 | HNS endpoint ACLs (`RuleType=Switch`) | Applied but **not enforced**: VFP is not managing the ports on this NAT switch (all `vfpctrl` ops error). |
| 4 | Route blackhole / in-container firewall | Defeated by proxy-ARP; `MpsSvc` cannot start inside the container. |

The theory for attempt #5: put a **BLOCK filter** (no callout driver, no kernel
signing) at `FWPM_LAYER_IPFORWARD_V4`, scoped to the ephemerd NAT vSwitch
arrival interface, denying RFC1918 while leaving the container subnet and the
internet alone. Zero-config, removable, Linux-parity.

## The stack under test

- Node: `mfl-win-amd64-101`, Windows Server 2025 build 26100, Hyper-V-isolated
  job container on HNS NAT network `ephemerd` (10.88.0.0/16, gw 10.88.0.1).
- NAT vSwitch host adapter: `vEthernet (ephemerd)`, **ifIndex 9**,
  InterfaceGuid `{A99E8F02-DC01-4487-BDFA-564474AD6B91}`, NET_LUID
  `0x6008003000000`.
- LAN adapter: `Ethernet`, **ifIndex 7**. The host reaches the management LAN
  (e.g. 192.168.10.45) via ifIndex 7 with source **192.168.14.249** — i.e. the
  container's egress is SNAT'd to the host's LAN address.

Two facts about the datapath that already hint at the outcome:

```
PS> Get-NetNat
   (no output — the HNS NAT network is NOT a NetNat object)

PS> Get-NetIPInterface -AddressFamily IPv4 | ? ifIndex -in 7,9
   ifIndex InterfaceAlias       Forwarding
   ------- --------------       ----------
         9 vEthernet (ephemerd)  Disabled
         7 Ethernet              Disabled
```

IP forwarding is **Disabled** on both interfaces, and there is no `NetNat`
object — the classic host IP-forwarding/WinNAT path is not what's moving these
packets. The Hyper-V virtual switch datapath is.

## What `FWPM_LAYER_IPFORWARD_V4` actually exposes here

The tailscale/wf binding enumerates the live layer's fields. On this host:

```
Layer IP Forward v4 Layer (IPFORWARD_V4) fields=15
  IP_SOURCE_ADDRESS            netip.Addr
  IP_DESTINATION_ADDRESS       netip.Addr
  IP_LOCAL_INTERFACE           uint64      <- arrival interface at this layer
  IP_FORWARD_INTERFACE         uint64      <- outbound/nexthop interface
  SOURCE_INTERFACE_INDEX       uint32
  DESTINATION_INTERFACE_INDEX  uint32
  IP_PHYSICAL_ARRIVAL_INTERFACE  uint64
  IP_PHYSICAL_NEXTHOP_INTERFACE  uint64
  ... (FLAGS, COMPARTMENT_ID, profile IDs)
```

Note there is **no `IP_ARRIVAL_INTERFACE`** field at this layer on this build —
the arrival interface is `IP_LOCAL_INTERFACE`, and the destination is
`IP_DESTINATION_ADDRESS` (not `IP_REMOTE_ADDRESS`). Both scoping conditions the
original plan named therefore had to be remapped before any test was valid.

## Experiments and results

Each experiment installs a persistent BLOCK filter (or a matrix of them) under
an ephemerd WFP provider/sublayer, then a real containment run
(`.github/workflows/containment.yml`, branch `test/containment-suite`) probes the
four fleet management planes from inside a Hyper-V-isolated job. To avoid cutting
the runner's own control channel, single-IP experiments target **Grafana
(192.168.10.45:3000)** only — blocking it does not disconnect the runner, so a
"Grafana blocked, other three reachable" result would have been the clean,
scoped success signal.

The containment "Fleet management planes" step prints `FAIL: reached <name>` for
every plane it **could** reach. A working scoped block would drop the Grafana
line and keep the other three.

| # | Layer | Conditions | Grafana result |
|---|-------|-----------|----------------|
| 1 | `IPFORWARD_V4` | `IP_DESTINATION_ADDRESS=192.168.10.45` **AND** `IP_LOCAL_INTERFACE=if9` | reached (not blocked) |
| 2 | `IPFORWARD_V4` | `IP_DESTINATION_ADDRESS=192.168.10.45` only | reached (not blocked) |
| 3 | `INBOUND_IPPACKET_V4` | `IP_LOCAL_ADDRESS=192.168.10.45` **AND** `IP_LOCAL_INTERFACE=if9` | reached (not blocked) |
| 4 | **matrix** (7 filters at once, all blocking Grafana): inbound local/remote × if9/any, ipforward dest × if9/any, **outbound remote (post-NAT, any iface)** | reached (not blocked) |

Matrix run (id `31354660618`) installed all seven filters:

```
  in-local-if      installed     (INBOUND_IPPACKET_V4, IP_LOCAL_ADDRESS + if9)
  in-remote-if     installed     (INBOUND_IPPACKET_V4, IP_REMOTE_ADDRESS + if9)
  in-local-any     installed     (INBOUND_IPPACKET_V4, IP_LOCAL_ADDRESS)
  in-remote-any    installed     (INBOUND_IPPACKET_V4, IP_REMOTE_ADDRESS)
  fwd-dest-if      installed     (IPFORWARD_V4, IP_DESTINATION_ADDRESS + if9)
  fwd-dest-any     installed     (IPFORWARD_V4, IP_DESTINATION_ADDRESS)
  out-remote-any   installed     (OUTBOUND_IPPACKET_V4, IP_REMOTE_ADDRESS)
ephemerd filters present: 7
```

Containment Windows egress step, that same run:

```
FAIL: reached Proxmox coyotes at 192.168.5.1:8006
FAIL: reached Proxmox kings at 192.168.5.2:8006
FAIL: reached Incus daemon at 192.168.12.113:8443
FAIL: reached Grafana at 192.168.10.45:3000
```

All four reached — none of the seven filters caught the container. The
load-bearing one is **`out-remote-any`**: after SNAT the container's packet
*must* leave the host on ifIndex 7 with destination 192.168.10.45. A dest-only
block at `OUTBOUND_IPPACKET_V4` did not stop it.

## Positive controls — WFP *is* enforced on this host

To rule out "the filters are silently inert" (the failure mode of the HNS ACLs
in attempt #3), the identical layer + field + value was pointed at the **host's
own** traffic:

```
# ALE_AUTH_CONNECT_V4, block IP_REMOTE_ADDRESS=1.1.1.1
host->1.1.1.1:443 reachable=False        <- host lost 1.1.1.1

# OUTBOUND_IPPACKET_V4, block IP_REMOTE_ADDRESS=1.1.1.1  (same layer+field as out-remote-any)
host->1.1.1.1:443 reachable=False        <- host lost 1.1.1.1

# after removing the ephemerd provider
host->1.1.1.1 restored=True
```

So `OUTBOUND_IPPACKET_V4` with `IP_REMOTE_ADDRESS` **does** block traffic that
enters the host WFP engine — it blocked the host. It just never sees the
container's SNAT'd packet. WFP is functional; the container's egress is simply
off the host's WFP-filtered path.

## Conclusion

On this WinNAT/HNS + Hyper-V-isolated stack:

1. WFP filters are applied and enforced (host traffic is blockable at ALE and
   OUTBOUND layers).
2. The container's forwarded + SNAT'd egress is **not classified** at any host
   IPv4 WFP layer that is either (a) container-distinguishable (arrival interface
   ifIndex 9, or pre-NAT source 10.88/16 at inbound) or (b) even visible
   post-NAT by destination at outbound.
3. Therefore the NAT + forward happens inside the Hyper-V vSwitch / HNS
   datapath, out-of-band of the host `tcpip.sys` WFP hooks. This is consistent
   with `Get-NetNat` being empty, per-interface Forwarding being Disabled, and
   the attempt-#3 finding that VFP is not managing the switch ports.

**Host-side software (WFP included) cannot enforce Windows container egress on
this stack.** The remaining enforcement point is the network itself: put the
Windows runner on an isolated VLAN / managed switch whose uplink denies the
RFC1918 management ranges. That is the recommended and only proven fix for the
Windows platform.

`pkg/networking/network_windows.go` keeps its per-endpoint HNS block ACLs as a
best-effort layer (they are cheap and correct where VFP *does* manage ports on
other Windows builds), but on this build they are inert and must not be relied
on — the VLAN is the control.

## Reproducer

A standalone Windows spike drove every experiment above via the pure-Go
`github.com/tailscale/wf` binding (no callout driver, no kernel signing). It was
intentionally **not** merged into the daemon or `go.mod` — there is no working
mechanism to integrate, and adding a WFP dependency for a negative result is
noise. The essential shape is small enough to reproduce inline:

```go
//go:build windows
package main

// go get github.com/tailscale/wf golang.org/x/sys/windows
//
// Resolve the NAT vSwitch interface LUID (arrival interface) and install a
// BLOCK filter at the chosen layer scoped to it + a destination. Change `layer`,
// the destination field, and the arrival field to reproduce each row above.

import (
	"net/netip"
	"unsafe"

	"github.com/tailscale/wf"
	"golang.org/x/sys/windows"
)

var (
	iphlp   = windows.NewLazySystemDLL("iphlpapi.dll")
	toLuid  = iphlp.NewProc("ConvertInterfaceIndexToLuid")
	provID  = wf.ProviderID{Data1: 0x1d6d3d8e, Data2: 0x6c9a, Data3: 0x4e7b, Data4: [8]byte{0x9f, 0x2a, 0x3b, 0x7c, 0x1e, 0x5d, 0x9a, 0x40}}
	subID   = wf.SublayerID{Data1: 0x2e7e4e9f, Data2: 0x7dab, Data3: 0x4f8c, Data4: [8]byte{0xa0, 0x3b, 0x4c, 0x8d, 0x2f, 0x6e, 0xab, 0x51}}
	ruleID  = wf.RuleID{Data1: 0x3f8f5fa0, Data2: 0x8ebc, Data3: 0x40fd, Data4: [8]byte{0xb1, 0x4c, 0x5d, 0x9e, 0x30, 0x7f, 0xbc, 0x62}}
)

func luid(ifindex uint32) uint64 {
	var l uint64
	toLuid.Call(uintptr(ifindex), uintptr(unsafe.Pointer(&l)))
	return l
}

func main() {
	s, _ := wf.New(&wf.Options{Name: "ephemerd-wfp-spike", Dynamic: false})
	defer s.Close()
	s.AddProvider(&wf.Provider{ID: provID, Name: "ephemerd", Persistent: true})
	s.AddSublayer(&wf.Sublayer{ID: subID, Name: "ephemerd-egress", Provider: provID, Persistent: true, Weight: 0xffff})

	// Row 1: IPFORWARD_V4, arrival = NAT vSwitch (ifIndex 9), dest = Grafana.
	s.AddRule(&wf.Rule{
		ID: ruleID, Name: "ephemerd-spike-block", Layer: wf.LayerIPForwardV4,
		Sublayer: subID, Provider: provID, Weight: 1000, Action: wf.ActionBlock,
		Persistent: true,
		Conditions: []*wf.Match{
			{Field: wf.FieldIPDestinationAddress, Op: wf.MatchTypeEqual, Value: netip.MustParsePrefix("192.168.10.45/32")},
			{Field: wf.FieldIPLocalInterface, Op: wf.MatchTypeEqual, Value: luid(9)},
		},
	})
	// Then dispatch the containment suite and read the Windows egress step.
	// Positive control: swap Layer -> wf.LayerOutboundIPPacketV4,
	// Field -> wf.FieldIPRemoteAddress, dest -> 1.1.1.1/32, drop the interface
	// condition, and Test-NetConnection 1.1.1.1 from the host: it goes False,
	// proving the engine enforces — the container just never reaches it.
	//
	// Cleanup: s.DeleteRule(ruleID); s.DeleteSublayer(subID); s.DeleteProvider(provID)
}
```

Field mapping that matters (this build): at `IPFORWARD_V4` the destination is
`IP_DESTINATION_ADDRESS` and the arrival interface is `IP_LOCAL_INTERFACE`
(there is no `IP_ARRIVAL_INTERFACE`); at `INBOUND_IPPACKET_V4` the destination of
a forwarded packet is `IP_LOCAL_ADDRESS` (REMOTE is the source) and the arrival
interface is again `IP_LOCAL_INTERFACE`; at `OUTBOUND_IPPACKET_V4` the
destination is `IP_REMOTE_ADDRESS`.
