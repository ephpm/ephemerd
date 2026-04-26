//go:build !windows

package dind

import (
	"fmt"
	"net"
	"os"
)

// listen starts the per-job Docker API listener on a unix socket at s.sockPath.
// Sets s.endpoint to "unix://<sockPath>" — the container sees the socket
// bind-mounted at /var/run/docker.sock and docker CLI auto-discovers it.
//
// The socket is chmod'd to 0666 so that container processes running as
// non-root users (the official GitHub Actions runner image runs as UID
// 1654 "runner") can connect to it. The socket lives only inside the
// per-job container's mount namespace and the trust boundary is the
// container itself, so world-writable here matches Docker's default
// behavior in --group docker setups.
func (s *Server) listen() (net.Listener, error) {
	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", s.sockPath, err)
	}
	if err := os.Chmod(s.sockPath, 0o666); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", s.sockPath, err)
	}
	s.endpoint = "unix://" + s.sockPath
	return ln, nil
}
