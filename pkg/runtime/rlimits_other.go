//go:build !linux

package runtime

import (
	"log/slog"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/jobcgroup"
)

// rlimitsOpts is a no-op on non-Linux platforms. Windows Hyper-V isolated
// containers and macOS Vz VMs don't use the POSIX rlimit model — host-side
// limits are governed by the VM/HCS configuration instead.
func rlimitsOpts(_ config.RuntimeRlimits) []oci.SpecOpts {
	return nil
}

// pidsLimitOpts is a no-op off Linux: pids.max is a Linux cgroup control.
func pidsLimitOpts(_ int64) []oci.SpecOpts {
	return nil
}

// jobCgroupOpts is a no-op off Linux: the job memory budget is a Linux
// cgroup v2 control (Windows containers have their own memory limit).
func jobCgroupOpts(_ string, _ jobcgroup.Limits, _ *slog.Logger) []oci.SpecOpts {
	return nil
}
