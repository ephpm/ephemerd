//go:build !darwin

package vm

import (
	"context"
	"fmt"
	"log/slog"
)

// MacOSVMDiskFiles is the non-darwin stub. See macos_install_darwin.go.
type MacOSVMDiskFiles struct {
	DataDir       string
	DiskImage     string
	AuxStorage    string
	MachineID     string
	HardwareModel string
	RestoreIPSW   string
}

// MacOSInstallOptions is the non-darwin stub. See macos_install_darwin.go.
type MacOSInstallOptions struct {
	CustomDiskImage string
	DiskSizeGB      uint64
	CPUs            uint
	MemoryMB        uint64
}

// EnsureMacOSVMDisk is only implemented on darwin. On other platforms it
// returns an error so accidental callers fail loudly instead of silently.
func EnsureMacOSVMDisk(_ context.Context, _ string, _ MacOSInstallOptions, _ *slog.Logger) (*MacOSVMDiskFiles, error) {
	return nil, fmt.Errorf("macOS VM disk provisioning is only supported on darwin hosts")
}
