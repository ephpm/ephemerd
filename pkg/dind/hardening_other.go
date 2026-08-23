//go:build !linux

package dind

import (
	"log/slog"

	"github.com/containerd/containerd/v2/pkg/oci"
)

// nonPrivilegedHardeningOpts is a no-op off Linux. Dind siblings are Linux-only
// — checkWindowsSiblingGate rejects a sibling create on a Windows host before
// this is ever reached — but the symbol must exist for the cross-platform build
// of containers.go.
func nonPrivilegedHardeningOpts(_ *slog.Logger) []oci.SpecOpts {
	return nil
}
