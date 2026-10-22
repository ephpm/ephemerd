//go:build !linux

package dind

import "github.com/containerd/containerd/v2/pkg/oci"

// pidsLimitOpts is a no-op off Linux: pids.max is a Linux cgroup control, and
// dind siblings only run on Linux (see nonPrivilegedHardeningOpts).
func pidsLimitOpts(_ int64) []oci.SpecOpts {
	return nil
}
