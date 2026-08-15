//go:build windows

package networking

import (
	"fmt"
	"net"
	"strings"
)

// adapterNameCandidates returns the adapter names to try for a configured
// network.host_nic, in order.
//
// Creating the L2Bridge network CONSUMES the configured NIC: HNS binds an
// external vSwitch to it and the host's side of it re-enumerates as
// "vEthernet (<name>)". So on every daemon start after the first, the raw
// configured name no longer exists — resolving only the literal name made the
// daemon crash-loop on restart ("no adapter with that name on this host",
// found on metal 2026-08-13). Deleting the network reverses the rename, so
// the unwrapped form must be tried too when the operator configured the
// vEthernet name.
func adapterNameCandidates(name string) []string {
	candidates := []string{name}
	if inner, ok := strings.CutPrefix(name, "vEthernet ("); ok {
		if inner, ok := strings.CutSuffix(inner, ")"); ok {
			candidates = append(candidates, inner)
		}
	} else {
		candidates = append(candidates, "vEthernet ("+name+")")
	}
	return candidates
}

// hostIPv4OnInterface returns the first routable IPv4 address configured on the
// named adapter, together with its network. On Windows the adapter name is the
// friendly name shown by Get-NetAdapter, which is also what the HNS
// NetAdapterName network policy expects — the same string in both places.
// The vEthernet-renamed form of the adapter is accepted too (see
// adapterNameCandidates).
//
// This lives in a Windows-only file because it reads the live host; the address
// arithmetic it feeds (resolveL2BridgePlan and friends) stays in l2bridge.go so
// it builds and is tested on every platform.
func hostIPv4OnInterface(name string) (net.IP, *net.IPNet, error) {
	var iface *net.Interface
	var err error
	for _, candidate := range adapterNameCandidates(name) {
		iface, err = net.InterfaceByName(candidate)
		if err == nil {
			break
		}
	}
	if iface == nil {
		return nil, nil, fmt.Errorf("network.host_nic %q: no adapter with that name on this host "+
			"(also tried %v; check `Get-NetAdapter`): %w", name, adapterNameCandidates(name)[1:], err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, nil, fmt.Errorf("network.host_nic %q: reading adapter addresses: %w", name, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
			continue
		}
		// Normalize to a 4-byte IP and a /n IPv4 mask.
		ones, bits := ipnet.Mask.Size()
		if bits != 32 {
			continue
		}
		return v4, &net.IPNet{IP: v4.Mask(ipnet.Mask), Mask: net.CIDRMask(ones, 32)}, nil
	}
	return nil, nil, fmt.Errorf("network.host_nic %q (resolved adapter %q): adapter has no routable IPv4 address "+
		"(an APIPA/link-local-only adapter cannot bridge); give the adapter a static or DHCP address, "+
		"or set network.host_nic to the adapter that carries the LAN", name, iface.Name)
}
