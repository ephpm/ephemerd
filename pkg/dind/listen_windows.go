//go:build windows

package dind

import (
	"fmt"
	"net"
)

// listen starts the per-job Docker API listener on TCP, bound to the HCN
// NAT gateway IP so Hyper-V-isolated runner containers on the same NAT can
// reach it without any mount. Windows named pipe sharing into isolated
// containers needs HCS config plumbing; TCP sidesteps that entirely.
//
// Port 0 means pick an ephemeral port — each per-job server gets its own
// endpoint. The chosen port is embedded in s.endpoint and handed back to
// the runner via DOCKER_HOST.
func (s *Server) listen() (net.Listener, error) {
	// Production: bind to the HCN NAT gateway so CI job containers on the
	// NAT can reach it. Tests: fall back to localhost when no networking
	// manager is wired up (unit tests in this package don't boot the HCN
	// bridge).
	host := "127.0.0.1"
	if s.network != nil {
		if gw := s.network.GatewayIP(); gw != "" {
			host = gw
		}
	}
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", host+":0", err)
	}

	// net.Listen("tcp", ":0") gives us the actual port in ln.Addr().
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("unexpected listener address type: %T", ln.Addr())
	}

	// On the L2Bridge path the container reaches this listener at the host's LAN
	// address, where the host's own inbound Windows Firewall default-denies it
	// (the VFP host /32 allow only governs the container's egress port), so an
	// inbound allow is required. It is NOT opened here.
	//
	// The allow must be scoped to the owning container's /32 — the fake Docker
	// API served on this port is unauthenticated, so anything wider hands every
	// other job's container control of this job's daemon. That address does not
	// exist yet: the runtime constructs and starts this server before it calls
	// networking.Manager.Setup, because the endpoint URI computed below has to
	// go into the container's OCI spec as DOCKER_HOST. So listen() only records
	// the bound port, and SetRunnerIP opens the allow once the runtime knows the
	// address — still before the container is created, so there is no window in
	// which the container is running without it.
	s.listenPort = tcpAddr.Port

	s.endpoint = fmt.Sprintf("tcp://%s:%d", host, tcpAddr.Port)
	return ln, nil
}
