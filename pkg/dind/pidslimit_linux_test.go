//go:build linux

package dind

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/jobcgroup"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// TestJobMemoryOpts: a sibling of a job with a memory budget is placed under
// the job parent cgroup, whose limits are (re)written first; with no budget,
// or when the parent cannot be prepared, the sibling keeps its default.
func TestJobMemoryOpts(t *testing.T) {
	old := ensureJobCgroup
	t.Cleanup(func() { ensureJobCgroup = old })
	limits := jobcgroup.Limits{MaxBytes: 4 << 30, HighBytes: 3 << 30}

	var ensured []string
	ensureJobCgroup = func(_ string, jobID string, l jobcgroup.Limits) error {
		if l != limits {
			t.Errorf("Ensure got %+v, want %+v", l, limits)
		}
		ensured = append(ensured, jobID)
		return nil
	}
	apply := func(opts []oci.SpecOpts) string {
		spec := &oci.Spec{Linux: &ocispec.Linux{CgroupsPath: "/default/path"}}
		for _, opt := range opts {
			if err := opt(context.Background(), nil, &containers.Container{ID: "abc123"}, spec); err != nil {
				t.Fatalf("opt: %v", err)
			}
		}
		return spec.Linux.CgroupsPath
	}

	if got, want := apply(jobMemoryOpts("job-1", limits, slog.Default())), jobcgroup.Dir("job-1")+"/abc123"; got != want {
		t.Errorf("CgroupsPath = %q, want %q", got, want)
	}
	if len(ensured) != 1 || ensured[0] != "job-1" {
		t.Errorf("Ensure calls = %v, want one for job-1", ensured)
	}
	if opts := jobMemoryOpts("job-1", jobcgroup.Limits{}, slog.Default()); len(opts) != 0 {
		t.Errorf("no budget produced %d opts, want none", len(opts))
	}
	ensureJobCgroup = func(string, string, jobcgroup.Limits) error { return errors.New("no memory controller") }
	if got := apply(jobMemoryOpts("job-1", limits, slog.Default())); got != "/default/path" {
		t.Errorf("CgroupsPath after a failed Ensure = %q, want the default kept (fail-open)", got)
	}
}
