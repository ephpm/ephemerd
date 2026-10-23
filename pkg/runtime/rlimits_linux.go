//go:build linux

package runtime

import (
	"context"
	"log/slog"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/jobcgroup"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// jobCgroupOpts places the runner container under its job's parent cgroup
// when the job has a memory budget, so the budget covers the runner and its
// dind siblings together (see pkg/jobcgroup). With no budget it adds nothing
// and the runner keeps its historical /ephemerd/<id> cgroup.
//
// Fail-open: if the parent cannot be created or limited, the runner keeps
// its default cgroup and the job runs unlimited, as it did before the
// setting existed. A protection that cannot be applied should not also
// cost the build.
func jobCgroupOpts(id string, limits jobcgroup.Limits, log *slog.Logger) []oci.SpecOpts {
	if !limits.Enabled() {
		return nil
	}
	return []oci.SpecOpts{
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
			if err := ensureJobCgroup(jobcgroup.Root, id, limits); err != nil {
				log.Warn("job memory limit not applied; the job runs unlimited", "id", id, "error", err)
				return nil
			}
			if s.Linux == nil {
				s.Linux = &ocispec.Linux{}
			}
			s.Linux.CgroupsPath = jobcgroup.RunnerPath(id)
			return nil
		},
	}
}

// ensureJobCgroup is jobcgroup.Ensure, a var so tests can run the spec
// option without a real cgroupfs.
var ensureJobCgroup = jobcgroup.Ensure

// pidsLimitOpts sets the container's cgroup pids.max. limit <= 0 adds
// nothing (no limit). dind applies the same limit to sibling containers
// (dind.pidsLimitOpt) — see config.RuntimeConfig.PidsLimit.
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
