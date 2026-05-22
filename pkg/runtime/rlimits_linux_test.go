//go:build linux

package runtime

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/ephpm/ephemerd/pkg/config"
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
	// Zero values must produce the containerd-default 1024 entries —
	// emitting Rlimits with Soft=Hard=0 would cripple the container.
	spec := &oci.Spec{Process: &ocispec.Process{}}
	opts := rlimitsOpts(config.RuntimeRlimits{})
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
	for _, rl := range spec.Process.Rlimits {
		if rl.Soft != 1024 || rl.Hard != 1024 {
			t.Errorf("%s: soft=%d hard=%d, want 1024/1024", rl.Type, rl.Soft, rl.Hard)
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
