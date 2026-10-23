//go:build linux

package dind

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// pidsLimitOpts sets a sibling's cgroup pids.max. limit <= 0 adds nothing.
// It mirrors runtime's runner-container option; runtime imports dind, so
// the two cannot share one helper. See config.RuntimeConfig.PidsLimit.
func pidsLimitOpts(limit int64) []oci.SpecOpts {
	if limit <= 0 {
		return nil
	}
	return []oci.SpecOpts{
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
			if s.Linux == nil {
				s.Linux = &ocispec.Linux{}
			}
			if s.Linux.Resources == nil {
				s.Linux.Resources = &ocispec.LinuxResources{}
			}
			l := limit
			s.Linux.Resources.Pids = &ocispec.LinuxPids{Limit: &l}
			return nil
		},
	}
}
