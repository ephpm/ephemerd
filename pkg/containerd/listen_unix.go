//go:build !windows

package containerd

import "net"

func listen(address string) (net.Listener, error) {
	return net.Listen("unix", address)
}
