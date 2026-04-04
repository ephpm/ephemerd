//go:build linux

package vm

import "fmt"

// StartLinuxVM is not needed on Linux — containerd runs directly on the host.
func StartLinuxVM(_ LinuxVMConfig) (LinuxVM, error) {
	return nil, fmt.Errorf("Linux VM not needed on Linux hosts — containerd runs natively")
}
