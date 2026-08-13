//go:build windows

package networking

import (
	"fmt"
	"net"
)

// hostIPv4OnInterface returns the first routable IPv4 address configured on the
// named adapter, together with its network. On Windows the adapter name is the
// friendly name shown by Get-NetAdapter, which is also what the HNS
// NetAdapterName network policy expects — the same string in both places.
//
// This lives in a Windows-only file because it reads the live host; the address
// arithmetic it feeds (resolveL2BridgePlan and friends) stays in l2bridge.go so
// it builds and is tested on every platform.
func hostIPv4OnInterface(name string) (net.IP, *net.IPNet, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, nil, fmt.Errorf("network.host_nic %q: no adapter with that name on this host "+
			"(check `Get-NetAdapter`): %w", name, err)
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
	return nil, nil, fmt.Errorf("network.host_nic %q: adapter has no routable IPv4 address "+
		"(an APIPA/link-local-only adapter cannot bridge); give the adapter a static or DHCP address, "+
		"or set network.host_nic to the adapter that carries the LAN", name)
}
