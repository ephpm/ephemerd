package proxies

import (
	"net"
	"net/url"
	"strings"
	"time"
)

// Cache proxies bind the CNI bridge gateway and advertise that same address to
// jobs. That is correct on a Linux host, where job containers sit on the bridge
// — and wrong everywhere else.
//
// On macOS and Windows hosts, container jobs do NOT run on the host. They run
// inside a Linux VM (and macOS jobs inside a macOS VM), reached over the
// hypervisor's NAT. From in there, the bridge gateway address resolves to the
// VM's OWN bridge, where nothing is listening. The proxies are on the HOST.
//
// Measured on mfl-mac-arm64 2026-09-30, 169 days uptime:
//
//	192.168.64.1:8082  -> 200  (0.195s)   host address on the Vz NAT
//	10.88.0.1:8082      -> 000  (4s timeout, advertised value)
//	10.88.x interfaces on the host: 0
//
// Every cache on that node read 0 bytes: gomod, cargo, ghrel, composer. The
// proxies were healthy the whole time — only the address handed to jobs was
// unreachable, and the health gate could not see it because it probes over
// loopback, which always answers.

// RewriteEnvHost rewrites the host in each "KEY=http://host:port/path" entry to
// host, preserving scheme, port and path. Entries that are not URLs are passed
// through untouched, because proxies also emit plain-value vars.
//
// Rewriting rather than re-rendering is deliberate: each proxy already knows
// how to build its own env (GOPROXY, CARGO_*, GHREL_PROXY, the lot), and only
// the authority is wrong. Re-deriving that per platform would duplicate seven
// proxies' env logic and let the copies drift.
func RewriteEnvHost(env []string, host string) []string {
	if host == "" || len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			out = append(out, e)
			continue
		}
		if port := u.Port(); port != "" {
			u.Host = net.JoinHostPort(host, port)
		} else {
			u.Host = host
		}
		out = append(out, k+"="+u.String())
	}
	return out
}

// HostAddrVisibleTo returns the local address that peerIP can reach this host
// on: the address of whichever local interface shares a subnet with peerIP.
//
// Derived rather than hardcoded. Apple's Virtualization.framework usually NATs
// on 192.168.64.0/24 and Hyper-V picks its own, so a constant per platform
// would be a guess that silently rots when the hypervisor changes its range.
// Asking "which of my addresses is on the VM's network" is true by
// construction.
//
// ok is false when nothing matches, and callers MUST treat that as "do not
// advertise" rather than falling back to a guess — a wrong address is what
// caused the outage this exists to fix.
func HostAddrVisibleTo(peerIP string) (string, bool) {
	peer := net.ParseIP(peerIP)
	if peer == nil {
		return "", false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ipnet.Contains(peer) {
			return ipnet.IP.String(), true
		}
	}
	return "", false
}

// HostAddrInSubnet returns this host's own IPv4 address inside subnet, which is
// the address a guest on that subnet can reach the host on.
//
// Separate from HostAddrVisibleTo because the caller often knows the
// hypervisor's NAT range but not any particular guest's address — at daemon
// start there may be no guest yet.
//
// Lives here, rather than in the platform file that uses it, so it is covered
// by tests that run on every OS. The darwin build cannot be compiled on a
// non-macOS machine (its vz dependency needs macOS cgo), so keeping logic out
// of that file is the difference between tested and merely plausible.
func HostAddrInSubnet(subnet *net.IPNet) (string, bool) {
	if subnet == nil {
		return "", false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		if subnet.Contains(ipnet.IP) {
			return ipnet.IP.String(), true
		}
	}
	return "", false
}

// Reachable reports whether a TCP connection to hostPort succeeds.
//
// This is the gate the old loopback health probe could not provide. A proxy
// that answers on 127.0.0.1 tells you the process is alive; it tells you
// nothing about whether the address being advertised to jobs exists. Probing
// the advertised address is the only check that would have caught either the
// macOS breakage or the Windows one.
//
// A failure means "do not inject this var". That is the safe direction: a
// missing GOPROXY costs bandwidth, while a black-holed one costs a build —
// Windows jobs HANG on an unreachable proxy rather than failing over, which is
// why proxy env is suppressed there wholesale today.
func Reachable(hostPort string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// EnvHostPorts extracts the host:port authority of every URL-valued entry, so
// a caller can probe each advertised address before injecting it.
func EnvHostPorts(env []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range env {
		_, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			continue
		}
		hp := u.Host
		if u.Port() == "" {
			switch u.Scheme {
			case "http":
				hp = net.JoinHostPort(u.Hostname(), "80")
			case "https":
				hp = net.JoinHostPort(u.Hostname(), "443")
			default:
				continue
			}
		}
		if !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	return out
}
