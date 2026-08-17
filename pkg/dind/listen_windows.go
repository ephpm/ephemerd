//go:build windows

package dind

import (
	"errors"
	"net"
)

// listenUnix is unreachable on Windows: resolveTransport pins every Windows
// server to TransportTCP, because runhcs supports neither the OCI Type:"bind"
// mount a unix socket would need nor named-pipe sharing into a
// Hyper-V-isolated container. It exists so the shared dispatcher in listen.go
// compiles, and returns an error rather than silently binding something the
// container could never reach.
func (s *Server) listenUnix() (net.Listener, error) {
	return nil, errors.New("dind: the unix-socket transport is not available on Windows (use TransportTCP)")
}
