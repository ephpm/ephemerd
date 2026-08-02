package runtime

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// newPrivilegesOpts returns the spec option that controls whether a
// process in the runner container can gain privileges through execve.
//
// allow=true emits NoNewPrivileges=false, which is what lets `sudo` and
// other setuid binaries work. That is the default and the behavior
// ephemerd has always had; see config.RuntimeConfig.AllowNewPrivileges
// for why an operator might turn it off.
//
// Both branches set the field explicitly rather than letting the false
// branch fall through to whatever oci.WithDefaultSpecForPlatform emits.
// The default happens to be NoNewPrivileges=true today, but relying on
// that would make the hardened path depend on a containerd default that
// could change without this package noticing.
//
// Not build-tagged: NoNewPrivileges is a Linux-only enforcement, but the
// field lives in the portable part of the OCI spec and setting it on
// Windows is inert (HCS ignores it), which matches how
// oci.WithNewPrivileges was applied before this became configurable.
func newPrivilegesOpts(allow bool) []oci.SpecOpts {
	if allow {
		return []oci.SpecOpts{oci.WithNewPrivileges}
	}
	return []oci.SpecOpts{withNoNewPrivileges}
}

// withNoNewPrivileges is the inverse of oci.WithNewPrivileges, which
// containerd does not ship.
func withNoNewPrivileges(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
	if s.Process == nil {
		s.Process = &ocispec.Process{}
	}
	s.Process.NoNewPrivileges = true
	return nil
}
