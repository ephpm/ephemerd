package proxies

import (
	"fmt"
	"log/slog"
	"net"
)

// Listen binds a cache-proxy listener on addr, falling back to the wildcard
// address on the same port when addr itself cannot be bound.
//
// WHY THIS EXISTS (this is the bug that made the Go module proxy cache stay
// empty forever — see docs and the review notes on this change):
//
// Cache proxies are asked to listen on the CNI bridge gateway IP (e.g.
// 10.88.0.1:8082) so job containers can reach them. But ephemerd deletes the
// ephemerd0 bridge on shutdown and again on startup, and the CNI bridge
// plugin only (re)creates the bridge — and only then assigns the gateway IP
// to it — when the FIRST job container is networked. Proxies start long
// before that, during daemon boot, so net.Listen("tcp", "10.88.0.1:8082")
// fails with EADDRNOTAVAIL every single time. The caller logs a warning and
// carries on without the proxy, which means its env vars are never injected
// into any container: the cache directory is created and then never written
// to again.
//
// Binding the wildcard address fixes this without racing the bridge: a
// wildcard (INADDR_ANY) socket accepts connections addressed to interface
// addresses that appear AFTER the bind, so the gateway IP becomes reachable
// the moment CNI brings the bridge up.
//
// The requested address is always tried first, so a host where the gateway
// already exists keeps the narrower binding. When both fail (e.g. the port is
// already in use, which fails on the wildcard too) the ORIGINAL error is
// returned — the fallback never masks a real problem.
//
// SECURITY NOTE: the wildcard binding also exposes the proxy on the host's
// other interfaces. These proxies only ever serve public package-registry
// content and hold no credentials, so the exposure is limited to an open
// caching mirror. Operators who need it closed should firewall the port at
// the host edge; see docs/getting-started/configuration.md.
func Listen(addr string, log *slog.Logger) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}

	wildcard, werr := wildcardAddr(addr)
	if werr != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	ln2, err2 := net.Listen("tcp", wildcard)
	if err2 != nil {
		// The wildcard failed too (port in use, permissions). Report the
		// original failure — it is the one the operator configured for.
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	if log != nil {
		log.Info("cache proxy bound to wildcard address; the requested address is not on any interface yet (the CNI bridge is created with the first job container)",
			"requested", addr, "bound", ln2.Addr().String(), "reason", err)
	}
	return ln2, nil
}

// wildcardAddr rewrites a host:port address to the wildcard form ":port",
// which binds every interface. Returns an error for input that is not a
// valid host:port pair, so callers can fall back to reporting the original
// bind failure rather than listening somewhere unintended.
func wildcardAddr(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("splitting address %q: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("address %q has no port", addr)
	}
	return net.JoinHostPort("", port), nil
}
