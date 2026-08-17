package dind

import (
	"fmt"
	"net"
	goruntime "runtime"
)

// Transport selects how a per-job fake Docker daemon is exposed to the job
// container. It is a runtime choice, not a compile-time one, because the
// right answer depends on whether the job container shares the host kernel.
//
//   - A runc container shares the host's kernel and mount namespace plumbing,
//     so a bind-mounted unix socket works and is the cheapest, most
//     Docker-native option: the CLI finds /var/run/docker.sock by itself and
//     the socket is reachable by nothing outside that container's mount
//     namespace.
//   - A VM-isolated container (Kata on Linux, Hyper-V isolation on Windows)
//     has its OWN kernel. A bind-mounted unix socket arrives in the guest as
//     an inode with no listening endpoint behind it, so every connect(2)
//     returns ECONNREFUSED. There is nothing to fix in the socket transport —
//     the socket's connectability lives in the host kernel and does not cross
//     the VM boundary. Such jobs get a TCP listener on the bridge gateway
//     instead, handed over as DOCKER_HOST, which the guest reaches over IP
//     like any other network service.
//
// Nothing about the served API differs between the two; only the wire.
type Transport string

const (
	// TransportAuto lets New pick the platform default: a unix socket on
	// Linux/macOS, TCP on Windows. The zero value, so a construction site
	// that does not set Transport keeps today's behavior.
	TransportAuto Transport = ""

	// TransportUnixSocket serves on a per-job unix socket which the caller
	// bind-mounts into the container at /var/run/docker.sock.
	TransportUnixSocket Transport = "unix"

	// TransportTCP serves on an ephemeral TCP port bound to the container
	// network's gateway address. The caller injects DOCKER_HOST and must
	// call SetRunnerIP once the container's address is known, so the port
	// is firewalled to that one container.
	TransportTCP Transport = "tcp"
)

// resolveTransport applies the platform rules to a requested transport.
//
// Windows is not negotiable: runhcs supports neither the OCI Type:"bind"
// mount for a socket nor named-pipe sharing into a Hyper-V-isolated
// container, so TCP is the only transport that has ever worked there and
// the knob must not be able to break it.
//
// Everywhere else the caller decides, defaulting to the unix socket.
func resolveTransport(requested Transport, goos string) Transport {
	if goos == "windows" {
		return TransportTCP
	}
	if requested == TransportTCP {
		return TransportTCP
	}
	return TransportUnixSocket
}

// listen starts the per-job Docker API listener on the server's transport
// and records the endpoint the container should use to reach it.
func (s *Server) listen() (net.Listener, error) {
	if s.transport == TransportTCP {
		return s.listenTCP()
	}
	return s.listenUnix()
}

// listenTCP starts the listener on an ephemeral TCP port bound to the
// container network's gateway address — the HCN NAT gateway on Windows, the
// CNI bridge address (10.88.0.1) on Linux — so a VM-isolated job container
// can reach it over IP without any mount.
//
// Port 0 means pick an ephemeral port: each per-job server gets its own
// endpoint. The chosen port is embedded in s.endpoint and handed to the job
// as DOCKER_HOST.
//
// SECURITY. Binding the gateway makes the port reachable from EVERY container
// on that network, and the Docker API served here authenticates nothing. The
// firewall scope is therefore the only authenticator, and it is applied
// separately by SetRunnerIP — see the comment there. listen only records the
// port: the owning container's address does not exist yet at this point,
// because the endpoint URI computed below has to go into the container's OCI
// spec (as DOCKER_HOST) before the container, and hence its network endpoint,
// is created.
func (s *Server) listenTCP() (net.Listener, error) {
	// Production: bind the gateway the job containers can route to. Tests:
	// fall back to localhost when no networking manager is wired up (unit
	// tests in this package don't boot a bridge).
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
	s.listenPort = tcpAddr.Port

	s.endpoint = fmt.Sprintf("tcp://%s:%d", host, tcpAddr.Port)
	return ln, nil
}

// EndpointPort returns the TCP port this server's listener bound, or 0 on the
// unix-socket transport. Exported for tests and diagnostics that need to
// assert the firewall scope matches the listener.
func (s *Server) EndpointPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenPort
}

// platformGOOS is the OS whose platform rules apply — the transport choice in
// resolveTransport and the sibling-container gate in checkWindowsSiblingGate.
// A variable rather than a direct runtime.GOOS reference so tests can exercise
// the Windows rules from a Linux test run, and the Linux rules from a Windows
// one (this repo is developed on both).
var platformGOOS = goruntime.GOOS
