//go:build linux

package runtime

import (
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// seccompOpts returns the default seccomp profile for Linux containers.
// This blocks dangerous syscalls (mount, ptrace, bpf, kexec, etc.)
// while allowing everything apt/dpkg and typical CI jobs need.
func seccompOpts() []oci.SpecOpts {
	return []oci.SpecOpts{seccomp.WithDefaultProfile()}
}
