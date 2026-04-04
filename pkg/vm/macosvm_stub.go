//go:build !darwin

package vm

import "fmt"

// NewMacOSVM is only available on macOS hosts.
func NewMacOSVM(_ MacOSVMConfig, _ string) (MacOSVM, error) {
	return nil, fmt.Errorf("macOS VMs are only supported on macOS hosts (Virtualization.framework)")
}
