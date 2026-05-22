//go:build linux

package runtime

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// rlimitsOpts sets RLIMIT_NOFILE and RLIMIT_NPROC on the container's OCI
// process spec. We deliberately replace the rlimits slice (rather than
// append) so the containerd default RLIMIT_NOFILE=1024 entry from
// oci.WithDefaultSpecForPlatform doesn't end up duplicated.
//
// The hard limit is set equal to the soft limit. Raising the hard limit
// from inside the container requires CAP_SYS_RESOURCE, which we
// intentionally don't grant — see containerCapabilities.
func rlimitsOpts(rl config.RuntimeRlimits) []oci.SpecOpts {
	resolved := rl.Resolved()
	return []oci.SpecOpts{
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
			if s.Process == nil {
				s.Process = &ocispec.Process{}
			}
			s.Process.Rlimits = []ocispec.POSIXRlimit{
				{
					Type: "RLIMIT_NOFILE",
					Soft: uint64(resolved.Nofile),
					Hard: uint64(resolved.Nofile),
				},
				{
					Type: "RLIMIT_NPROC",
					Soft: uint64(resolved.Nproc),
					Hard: uint64(resolved.Nproc),
				},
			}
			return nil
		},
	}
}
