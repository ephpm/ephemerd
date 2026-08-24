//go:build !linux

package runtime

import (
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/ephpm/ephemerd/pkg/config"
)

// User-namespace remapping is a Linux-kernel mechanism (idmapped mounts +
// runc userns). On other platforms the runner container never gets a remap,
// so both helpers are no-ops regardless of config. Windows job isolation is
// provided by Hyper-V, not user namespaces.
func usernsSpecOpts(_ config.RuntimeUserns) []oci.SpecOpts { return nil }

func usernsSnapshotOpts(_ config.RuntimeUserns) []snapshots.Opt { return nil }
