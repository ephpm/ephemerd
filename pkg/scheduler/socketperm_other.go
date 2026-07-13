//go:build !windows

package scheduler

import (
	"fmt"
	"os"
)

// secureControlSocket restricts the control socket to its owner. On POSIX the
// AF_UNIX socket file's mode bits are enforced, so chmod 0600 (owner rw only)
// is sufficient: the daemon runs as root/the service user and only that user
// may connect.
func secureControlSocket(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
