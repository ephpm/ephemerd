//go:build !linux

package runtime

import "github.com/containerd/containerd/v2/pkg/oci"

// apparmorOpts is a no-op on non-Linux platforms. Windows uses Hyper-V
// isolation and darwin runs jobs inside a Linux VM, where the in-VM ephemerd
// (a linux build) applies the profile.
func apparmorOpts() []oci.SpecOpts {
	return nil
}
