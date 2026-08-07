//go:build !linux

package runtime

import (
	"log/slog"

	"github.com/containerd/containerd/v2/pkg/oci"
)

// apparmorOpts is a no-op on non-Linux platforms. Windows uses Hyper-V
// isolation and darwin runs jobs inside a Linux VM, where the in-VM ephemerd
// (a linux build) applies the profile. Nothing is logged here: the absence of
// AppArmor on Windows/darwin is not a degraded state, unlike on Linux.
func apparmorOpts(_ *slog.Logger) []oci.SpecOpts {
	return nil
}
