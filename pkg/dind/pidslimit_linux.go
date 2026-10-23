//go:build linux

package dind

import (
	"context"
	"log/slog"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/jobcgroup"
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

// jobMemoryOpts applies the job's memory budget to a sibling by placing it
// under the job parent cgroup and (re)writing the parent's limits first. The
// default spec already puts it there — containerd derives CgroupsPath from
// the job's dind namespace, which is the parent's name — but the path is set
// explicitly so the budget cannot silently stop applying if that derivation
// ever changes. No budget adds nothing.
//
// Fail-open, like the runner side: a parent that cannot be limited leaves the
// sibling on its default cgroup, unlimited, rather than failing the create.
func jobMemoryOpts(jobID string, limits jobcgroup.Limits, log *slog.Logger) []oci.SpecOpts {
	if !limits.Enabled() || jobID == "" {
		return nil
	}
	return []oci.SpecOpts{
		func(_ context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
			if err := ensureJobCgroup(jobcgroup.Root, jobID, limits); err != nil {
				log.Warn("job memory limit not applied to sibling; it runs unlimited", "error", err)
				return nil
			}
			if s.Linux == nil {
				s.Linux = &ocispec.Linux{}
			}
			if c != nil && c.ID != "" {
				s.Linux.CgroupsPath = jobcgroup.Dir(jobID) + "/" + c.ID
			}
			return nil
		},
	}
}

// ensureJobCgroup is jobcgroup.Ensure, a var so tests can run the spec
// option without a real cgroupfs.
var ensureJobCgroup = jobcgroup.Ensure
