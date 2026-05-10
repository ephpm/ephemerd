//go:build !linux

package dind

import "errors"

// startPortForwardProxy is a no-op on non-Linux platforms — the dind subsystem
// only runs inside the Linux VM, so this build is reached only by Windows /
// macOS host-side compiles where the Linux ephemerd-linux is the one that
// actually serves dind.
func startPortForwardProxy(_, _, _, _, _ string) (func(), error) {
	return nil, errors.New("port forwarding only implemented on Linux")
}
