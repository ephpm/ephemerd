//go:build linux

package runtime

import (
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ephpm/ephemerd/pkg/config"
)

// usernsSpecOpts returns the OCI spec option that places the runner
// container in a REMAPPED user namespace when userns is enabled: container
// uid/gid 0 map to host base, and the whole [0, Size) id range maps
// contiguously onto [base, base+Size). Empty when disabled — the container
// then shares the host user namespace and container root is real host root,
// the behavior ephemerd has always had.
//
// The maps here MUST match usernsSnapshotOpts (same base + size) so runc and
// the snapshotter agree on which host uids own the rootfs. See issue #126.
func usernsSpecOpts(u config.RuntimeUserns) []oci.SpecOpts {
	if !u.Enabled {
		return nil
	}
	u = u.Resolved()
	idMap := func(host uint32) []specs.LinuxIDMapping {
		return []specs.LinuxIDMapping{{ContainerID: 0, HostID: host, Size: u.Size}}
	}
	return []oci.SpecOpts{oci.WithUserNamespace(idMap(u.BaseUID), idMap(u.BaseGID))}
}

// usernsSnapshotOpts returns the snapshot options that make the container
// rootfs snapshot present mapped ownership to the remapped container — via an
// idmapped mount where the host overlay snapshotter supports it. Empty when
// disabled.
//
// FAIL-CLOSED: if the host snapshotter cannot honor the remap (no
// idmapped-mount support and no slow_chown configured), containerd's
// resolveSnapshotOptions fails the snapshot prepare, so NewContainer errors
// and the job fails visibly rather than silently running UNMAPPED with
// container root == host root. That is the intended behavior for a security
// control: a userns pool that can't actually remap must not fall back to the
// weaker posture without anyone noticing.
func usernsSnapshotOpts(u config.RuntimeUserns) []snapshots.Opt {
	if !u.Enabled {
		return nil
	}
	u = u.Resolved()
	// WithRemapperLabels(ctrUID, hostUID, ctrGID, hostGID, length): stamp the
	// containerd.io/snapshot/uidmapping+gidmapping labels the overlay
	// snapshotter reads to remap the rootfs.
	return []snapshots.Opt{client.WithRemapperLabels(0, u.BaseUID, 0, u.BaseGID, u.Size)}
}
