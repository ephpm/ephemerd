// Package vm manages virtual machines for cross-OS job execution.
//
// ephemerd uses VMs in two scenarios:
//
//  1. Linux VM (long-running): On Windows and macOS hosts, a lightweight Linux VM
//     runs containerd for Linux jobs. Same OCI images as native Linux.
//     - Windows: WSL2 distro with embedded ephemerd binary (Hyper-V fallback for Server)
//     - macOS: Virtualization.framework Linux VM
//
//  2. macOS VM (per-job): On macOS hosts, ephemeral macOS VMs run macOS-native
//     jobs (Xcode, Swift, etc.). Each job gets a clone-on-write copy of a base
//     image that is destroyed after the job completes.
//
// Platform-specific implementations are in *_darwin.go, *_windows.go, and *_linux.go.
package vm

import (
	"context"
	"log/slog"

	"github.com/containerd/containerd/v2/client"
)

// LinuxVMConfig configures the long-running Linux VM for Linux jobs on non-Linux hosts.
type LinuxVMConfig struct {
	// DataDir is the ephemerd data directory. VM assets live under <DataDir>/vm/linux/.
	DataDir string

	// CPUs is the number of virtual CPUs. Defaults to 2.
	CPUs uint

	// MemoryMB is the VM memory in megabytes. Defaults to 2048.
	MemoryMB uint64

	// DiskSizeGB is the VM root disk size in gigabytes (sparse). Defaults to 50.
	DiskSizeGB uint64

	// ContainerdPort is the port containerd listens on inside the VM. Defaults to 10000.
	ContainerdPort uint32

	Log *slog.Logger
}

// SetDefaults applies default values for unconfigured fields.
func (c *LinuxVMConfig) SetDefaults() {
	if c.CPUs == 0 {
		c.CPUs = 2
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 2048
	}
	if c.DiskSizeGB == 0 {
		c.DiskSizeGB = 50
	}
	if c.ContainerdPort == 0 {
		c.ContainerdPort = 10000
	}
}

// LinuxVM is a long-running Linux VM that hosts containerd for Linux jobs.
// Implemented per-platform: Virtualization.framework on macOS, Hyper-V on Windows.
type LinuxVM interface {
	// Client returns a containerd client connected to containerd inside the VM.
	Client() *client.Client

	// DispatchAddr returns the address of the dispatch gRPC server running
	// inside the VM (e.g. "localhost:10001"). Empty if dispatch is unavailable.
	DispatchAddr() string

	// Stop gracefully shuts down the VM.
	Stop()
}

// MacOSVMConfig configures per-job macOS VMs (macOS hosts only).
type MacOSVMConfig struct {
	// DataDir is the ephemerd data directory. VM assets live under <DataDir>/vm/macos/.
	DataDir string

	// BaseImage is the path to the provisioned macOS disk image.
	// Created via 'ephemerd vm setup-macos' or manually.
	BaseImage string

	// CPUs per macOS VM. Defaults to 4.
	CPUs uint

	// MemoryMB per macOS VM. Defaults to 8192.
	MemoryMB uint64

	Log *slog.Logger
}

// SetDefaults applies default values for unconfigured fields.
func (c *MacOSVMConfig) SetDefaults() {
	if c.CPUs == 0 {
		c.CPUs = 4
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 8192
	}
}

// MacOSVM is an ephemeral macOS VM for a single job.
// Only available on macOS hosts via Virtualization.framework.
type MacOSVM interface {
	// Start boots the VM from a clone-on-write copy of the base image.
	Start(ctx context.Context) error

	// RunnerAddress returns the address to connect to the GitHub runner inside the VM.
	// Typically ssh://... or a vsock address for injecting the JIT config.
	RunnerAddress() string

	// Wait blocks until the VM exits. Returns the exit code.
	Wait(ctx context.Context) (int, error)

	// Stop forcefully stops the VM and deletes the clone.
	Stop()
}
