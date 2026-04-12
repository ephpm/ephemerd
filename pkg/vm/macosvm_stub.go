//go:build !darwin

package vm

import (
	"context"
	"fmt"
)

// NewMacOSVM is only available on macOS hosts.
func NewMacOSVM(_ MacOSVMConfig, _ string) (MacOSVM, error) {
	return nil, fmt.Errorf("macOS VMs are only supported on macOS hosts (Virtualization.framework)")
}

// stubMacOSVM satisfies the MacOSVM interface on non-darwin platforms.
// It is never instantiated — NewMacOSVM always returns an error.
type stubMacOSVM struct{}

func (stubMacOSVM) WriteJITConfig(string) error                       { return fmt.Errorf("not supported") }
func (stubMacOSVM) Start(context.Context) error                       { return fmt.Errorf("not supported") }
func (stubMacOSVM) WaitForRunner(context.Context) (string, error)     { return "", fmt.Errorf("not supported") }
func (stubMacOSVM) RunnerAddress() string                             { return "" }
func (stubMacOSVM) Wait(context.Context) (int, error)                 { return 1, fmt.Errorf("not supported") }
func (stubMacOSVM) Stop()                                             {}
