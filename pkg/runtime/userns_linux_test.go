//go:build linux

package runtime

import (
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ephpm/ephemerd/pkg/config"
)

// Disabled is the default and must add nothing: no user namespace, no
// id maps. A job then shares the host user namespace exactly as before —
// this is what keeps the feature a true no-op until a pool opts in.
func TestUsernsSpecOpts_DisabledAddsNothing(t *testing.T) {
	if opts := usernsSpecOpts(config.RuntimeUserns{Enabled: false}); opts != nil {
		t.Fatalf("disabled userns returned %d opts, want none", len(opts))
	}
	// Even applied against a spec, an empty opt list leaves Linux untouched.
	spec := &oci.Spec{Linux: &ocispec.Linux{}}
	applyOpts(t, spec, usernsSpecOpts(config.RuntimeUserns{Enabled: false}))
	if len(spec.Linux.UIDMappings) != 0 || len(spec.Linux.GIDMappings) != 0 {
		t.Error("disabled userns left id mappings on the spec")
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == ocispec.UserNamespace {
			t.Error("disabled userns injected a user namespace")
		}
	}
}

// Enabled must map container id 0 -> host base for the full range, on both
// uid and gid, and insert a user namespace. If the maps are wrong the
// remap silently mis-maps ownership; if the namespace is missing there is
// no remap at all (container root stays host root).
func TestUsernsSpecOpts_EnabledMapsRootToBase(t *testing.T) {
	u := config.RuntimeUserns{Enabled: true, BaseUID: 1000000000, BaseGID: 1000000000, Size: 65536}
	spec := &oci.Spec{}
	applyOpts(t, spec, usernsSpecOpts(u))

	if spec.Linux == nil {
		t.Fatal("enabled userns did not initialize spec.Linux")
	}
	wantU := []ocispec.LinuxIDMapping{{ContainerID: 0, HostID: 1000000000, Size: 65536}}
	if got := spec.Linux.UIDMappings; !idMapsEqual(got, wantU) {
		t.Errorf("UIDMappings = %+v, want %+v", got, wantU)
	}
	wantG := []ocispec.LinuxIDMapping{{ContainerID: 0, HostID: 1000000000, Size: 65536}}
	if got := spec.Linux.GIDMappings; !idMapsEqual(got, wantG) {
		t.Errorf("GIDMappings = %+v, want %+v", got, wantG)
	}
	var hasUserNS bool
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == ocispec.UserNamespace {
			hasUserNS = true
		}
	}
	if !hasUserNS {
		t.Error("enabled userns did not add a user namespace to the spec")
	}
}

// A construction site (or test) that sets Enabled but leaves base/size zero
// must still get a real remap via Resolved(), not a degenerate map onto
// host uid 0 (which would be no remap at all — container root == host root).
func TestUsernsSpecOpts_EnabledWithoutBaseUsesResolvedDefaults(t *testing.T) {
	spec := &oci.Spec{}
	applyOpts(t, spec, usernsSpecOpts(config.RuntimeUserns{Enabled: true}))
	if spec.Linux == nil || len(spec.Linux.UIDMappings) != 1 {
		t.Fatal("expected one uid mapping")
	}
	m := spec.Linux.UIDMappings[0]
	if m.HostID == 0 {
		t.Fatal("resolved default mapped container root to host uid 0 — that is no remap")
	}
	if m.HostID != 1000000000 || m.Size != 65536 {
		t.Errorf("resolved default mapping = %+v, want host 1000000000 size 65536", m)
	}
}

// The snapshot remapper labels must (a) be absent when disabled and (b)
// encode the SAME base+size as the spec maps when enabled — a transposed
// or half-applied remap (spec says mapped, snapshot doesn't, or uid/gid
// swapped) would leave the rootfs and runc disagreeing on ownership.
func TestUsernsSnapshotOpts_LabelsMatchSpecAndDisabledEmpty(t *testing.T) {
	if opts := usernsSnapshotOpts(config.RuntimeUserns{Enabled: false}); opts != nil {
		t.Fatalf("disabled returned %d snapshot opts, want none", len(opts))
	}
	opts := usernsSnapshotOpts(config.RuntimeUserns{Enabled: true}) // resolved defaults
	if len(opts) != 1 {
		t.Fatalf("enabled returned %d snapshot opts, want 1", len(opts))
	}
	info := &snapshots.Info{}
	for _, o := range opts {
		if err := o(info); err != nil {
			t.Fatalf("apply snapshot opt: %v", err)
		}
	}
	uid := info.Labels[snapshots.LabelSnapshotUIDMapping]
	gid := info.Labels[snapshots.LabelSnapshotGIDMapping]
	if uid == "" || gid == "" {
		t.Fatalf("remapper labels not set: uid=%q gid=%q", uid, gid)
	}
	// uid and gid use the same base+size, so their labels are identical here;
	// divergence means the remap was built asymmetrically.
	if uid != gid {
		t.Errorf("uid mapping label %q != gid mapping label %q", uid, gid)
	}
	// Must encode the resolved default base, never host root.
	if !strings.Contains(uid, "1000000000") {
		t.Errorf("mapping label %q does not carry the resolved base 1000000000", uid)
	}
	if strings.HasPrefix(uid, "0:0:") {
		t.Errorf("mapping label %q maps container root to host uid 0 — no remap", uid)
	}
}

func idMapsEqual(a, b []ocispec.LinuxIDMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
