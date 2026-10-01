//go:build darwin

package main

import (
	"log/slog"
	"net"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

// vzNAT is the subnet Apple's Virtualization.framework NATs guests onto. The VM
// code already relies on this (pkg/vm/linuxvm_darwin.go and macosvm_darwin.go
// both sweep 192.168.64.0/24 to find a guest in ARP), so this is the same
// documented behaviour rather than a new assumption.
//
// It is used only to LOCATE a candidate host address among the local
// interfaces. The address is then probed, and dropped if it does not answer —
// so if Apple ever changes the range, the result is "no proxy env" (a
// bandwidth cost) rather than a black-holed address (a broken build).
var vzNAT = &net.IPNet{IP: net.IPv4(192, 168, 64, 0), Mask: net.CIDRMask(24, 32)}

// probeTimeout is short on purpose: this runs on the daemon's startup path, the
// address is on a local interface, and a slow answer is indistinguishable from
// no answer for our purposes.
const probeTimeout = 2 * time.Second

// resolveJobProxyEnv rewrites cache-proxy env so jobs on this Mac can actually
// reach the proxies, and drops anything that cannot be verified.
//
// WHY: jobs here do not run on the host. Linux jobs run in containers inside a
// Linux VM, macOS jobs inside a macOS VM, both behind the Vz NAT. The proxies
// advertise the CNI bridge gateway (10.88.0.1), which from inside a VM is the
// VM's own bridge, where nothing listens. The proxies are on the HOST.
//
// Measured on this fleet's Mac, 169 days uptime, before this change:
//
//	192.168.64.1:8082 -> 200 (0.195s)     the host, on the Vz NAT
//	10.88.0.1:8082     -> 000 (4s timeout) the advertised value
//	10.88.x interfaces on the host: 0
//	caches: gomod 0B, cargo 0B, ghrel 0B, composer 0B
//
// The proxies were healthy throughout. Only the advertised address was wrong,
// and the existing health gate could not see it because it probes loopback,
// which always answers.
func resolveJobProxyEnv(env []string, log *slog.Logger) []string {
	if len(env) == 0 {
		return env
	}
	if log == nil {
		log = slog.Default()
	}

	host, ok := proxies.HostAddrInSubnet(vzNAT)
	if !ok {
		// No interface on the guest NAT: the VM subsystem is not up, or Apple
		// moved the range. Advertise nothing rather than guess.
		log.Warn("no host address found on the VM NAT; not advertising cache proxies to jobs",
			"nat", vzNAT.String())
		return nil
	}

	rewritten := proxies.RewriteEnvHost(env, host)

	// Probe every advertised address. Unlike Linux, there is no
	// created-later bridge to wait for here — the address is on a live local
	// interface, so a failure now is a real failure.
	for _, hp := range proxies.EnvHostPorts(rewritten) {
		if !proxies.Reachable(hp, probeTimeout) {
			log.Warn("rewritten cache-proxy address did not answer; not advertising cache proxies to jobs",
				"address", hp, "host", host)
			return nil
		}
	}

	log.Info("cache proxies advertised to jobs on the VM-visible host address",
		"host", host, "was", "cni bridge gateway", "env", rewritten)
	return rewritten
}
