//go:build linux

package runtime

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/jobcgroup"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestRlimitsOpts_AppliesConfiguredValues(t *testing.T) {
	spec := &oci.Spec{Process: &ocispec.Process{}}
	opts := rlimitsOpts(config.RuntimeRlimits{Nofile: 4096, Nproc: 2048})
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	rls := spec.Process.Rlimits
	if len(rls) != 2 {
		t.Fatalf("len(Rlimits) = %d, want 2: %+v", len(rls), rls)
	}
	want := map[string]uint64{"RLIMIT_NOFILE": 4096, "RLIMIT_NPROC": 2048}
	for _, rl := range rls {
		w, ok := want[rl.Type]
		if !ok {
			t.Errorf("unexpected rlimit %q", rl.Type)
			continue
		}
		if rl.Soft != w || rl.Hard != w {
			t.Errorf("%s: soft=%d hard=%d, want soft=hard=%d", rl.Type, rl.Soft, rl.Hard, w)
		}
	}
}

func TestRlimitsOpts_AppliesDefaultsWhenZero(t *testing.T) {
	// Zero values must resolve to the defaults — emitting Rlimits with
	// Soft=Hard=0 would cripple the container.
	spec := &oci.Spec{Process: &ocispec.Process{}}
	opts := rlimitsOpts(config.RuntimeRlimits{})
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	want := map[string]uint64{
		"RLIMIT_NOFILE": 1024,
		"RLIMIT_NPROC":  uint64(config.RuntimeRlimits{}.Resolved().Nproc),
	}
	for _, rl := range spec.Process.Rlimits {
		if rl.Soft != want[rl.Type] || rl.Hard != want[rl.Type] {
			t.Errorf("%s: soft=%d hard=%d, want %d", rl.Type, rl.Soft, rl.Hard, want[rl.Type])
		}
	}
}

func TestRlimitsOpts_ReplacesDefaultRlimits(t *testing.T) {
	// oci.WithDefaultSpecForPlatform pre-populates RLIMIT_NOFILE=1024.
	// Our opt must overwrite (not append) so we don't end up with two
	// RLIMIT_NOFILE entries — the OCI runtime's behavior with duplicates
	// is undefined.
	spec := &oci.Spec{Process: &ocispec.Process{
		Rlimits: []ocispec.POSIXRlimit{
			{Type: "RLIMIT_NOFILE", Soft: 1024, Hard: 1024},
		},
	}}
	opts := rlimitsOpts(config.RuntimeRlimits{Nofile: 8192, Nproc: 4096})
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	if len(spec.Process.Rlimits) != 2 {
		t.Errorf("len(Rlimits) = %d, want 2 (no duplicates)", len(spec.Process.Rlimits))
	}
	seen := map[string]int{}
	for _, rl := range spec.Process.Rlimits {
		seen[rl.Type]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("rlimit %s appears %d times, want 1", k, n)
		}
	}
}

func TestRlimitsOpts_NilProcessSpec(t *testing.T) {
	// Defensive: WithDefaultSpecForPlatform always sets Process, but the
	// helper should not panic if someone composes opts in a different
	// order in the future.
	spec := &oci.Spec{}
	opts := rlimitsOpts(config.RuntimeRlimits{Nofile: 2048, Nproc: 1024})
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	if spec.Process == nil {
		t.Fatal("Process is nil after rlimitsOpts ran")
	}
	if len(spec.Process.Rlimits) != 2 {
		t.Errorf("len(Rlimits) = %d, want 2", len(spec.Process.Rlimits))
	}
}

// TestPidsLimitOpts pins the per-job process bound: a positive limit lands
// in the spec's cgroup pids controller, and "no limit" adds nothing rather
// than writing pids.max=0, which would forbid every fork.
func TestPidsLimitOpts(t *testing.T) {
	spec := &oci.Spec{}
	for _, opt := range pidsLimitOpts(32768) {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Pids == nil ||
		spec.Linux.Resources.Pids.Limit == nil || *spec.Linux.Resources.Pids.Limit != 32768 {
		t.Fatalf("pids limit not applied: %+v", spec.Linux)
	}
	if opts := pidsLimitOpts(0); len(opts) != 0 {
		t.Errorf("pidsLimitOpts(0) returned %d opts, want none", len(opts))
	}
}

// TestJobCgroupOpts: with a job memory budget the runner moves under the
// job parent cgroup (so one memory.max covers it and its dind siblings);
// without one, or when the parent cannot be prepared, it keeps its default
// cgroup.
func TestJobCgroupOpts(t *testing.T) {
	old := ensureJobCgroup
	t.Cleanup(func() { ensureJobCgroup = old })
	limits := jobcgroup.Limits{MaxBytes: 1 << 30}

	apply := func(opts []oci.SpecOpts) *oci.Spec {
		spec := &oci.Spec{Linux: &ocispec.Linux{CgroupsPath: "/ephemerd/job-1"}}
		for _, opt := range opts {
			if err := opt(context.Background(), nil, nil, spec); err != nil {
				t.Fatalf("opt: %v", err)
			}
		}
		return spec
	}

	ensureJobCgroup = func(string, string, jobcgroup.Limits) error { return nil }
	if got := apply(jobCgroupOpts("job-1", limits, slog.Default())).Linux.CgroupsPath; got != jobcgroup.RunnerPath("job-1") {
		t.Errorf("CgroupsPath = %q, want %q", got, jobcgroup.RunnerPath("job-1"))
	}
	if opts := jobCgroupOpts("job-1", jobcgroup.Limits{}, slog.Default()); len(opts) != 0 {
		t.Errorf("no budget produced %d opts, want none", len(opts))
	}
	ensureJobCgroup = func(string, string, jobcgroup.Limits) error { return errors.New("read-only cgroupfs") }
	if got := apply(jobCgroupOpts("job-1", limits, slog.Default())).Linux.CgroupsPath; got != "/ephemerd/job-1" {
		t.Errorf("CgroupsPath after a failed Ensure = %q, want the default kept (fail-open)", got)
	}
}
