//go:build !linux

package runtime

import "github.com/containerd/containerd/v2/pkg/oci"

// seccompOpts is a no-op on non-Linux platforms (Windows uses Hyper-V isolation).
func seccompOpts() []oci.SpecOpts {
	return nil
}
