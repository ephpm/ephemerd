package runtime

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

func applyOpts(t *testing.T, spec *oci.Spec, opts []oci.SpecOpts) {
	t.Helper()
	for _, opt := range opts {
		if err := opt(context.Background(), nil, nil, spec); err != nil {
			t.Fatalf("opt: %v", err)
		}
	}
}

// The default must stay permissive: NoNewPrivileges=false is what makes
// `sudo apt-get install` work, and that is what every existing workflow
// is written against.
func TestNewPrivilegesOpts_AllowLeavesEscalationAvailable(t *testing.T) {
	spec := &oci.Spec{Process: &ocispec.Process{NoNewPrivileges: true}}
	applyOpts(t, spec, newPrivilegesOpts(true))

	if spec.Process.NoNewPrivileges {
		t.Error("NoNewPrivileges = true with allow=true, want false — sudo would break in every job")
	}
}

// The hardened path must set the field explicitly rather than relying on
// the containerd default spec, so it can't regress if that default moves.
func TestNewPrivilegesOpts_DenySetsNoNewPrivilegesExplicitly(t *testing.T) {
	// Start from the permissive value so a no-op implementation fails.
	spec := &oci.Spec{Process: &ocispec.Process{NoNewPrivileges: false}}
	applyOpts(t, spec, newPrivilegesOpts(false))

	if !spec.Process.NoNewPrivileges {
		t.Error("NoNewPrivileges = false with allow=false, want true — setuid escalation would stay available on a pool that opted out")
	}
}

func TestNewPrivilegesOpts_ToleratesNilProcess(t *testing.T) {
	for _, allow := range []bool{true, false} {
		spec := &oci.Spec{}
		applyOpts(t, spec, newPrivilegesOpts(allow))
		if spec.Process == nil {
			t.Fatalf("allow=%v: Process still nil after opts", allow)
		}
		if spec.Process.NoNewPrivileges == allow {
			t.Errorf("allow=%v: NoNewPrivileges = %v", allow, spec.Process.NoNewPrivileges)
		}
	}
}
