//go:build !linux

package runtime

import (
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
)

// rlimitsOpts is a no-op on non-Linux platforms. Windows Hyper-V isolated
// containers and macOS Vz VMs don't use the POSIX rlimit model — host-side
// limits are governed by the VM/HCS configuration instead.
func rlimitsOpts(_ config.RuntimeRlimits) []oci.SpecOpts {
	return nil
}
