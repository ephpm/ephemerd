//go:build linux

package dind

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// TestSiblingHardenedDropCaps_DropsNetRaw is the security-critical assertion:
// CAP_NET_RAW must be in the drop list, because it is what lets an unprivileged
// sibling forge ARP replies and man-in-the-middle the plaintext package-cache
// proxies on the bridge gateway.
func TestSiblingHardenedDropCaps_DropsNetRaw(t *testing.T) {
	found := false
	for _, c := range siblingHardenedDropCaps {
		if c == "CAP_NET_RAW" {
			found = true
		}
	}
	if !found {
		t.Fatalf("siblingHardenedDropCaps must include CAP_NET_RAW; got %v", siblingHardenedDropCaps)
	}
}

// TestDropCaps_RemovesFromEverySet applies the same oci.WithDroppedCapabilities
// the production hardening uses and asserts CAP_NET_RAW is gone from every
// capability set (a residual in any one set would still grant the syscall).
func TestDropCaps_RemovesFromEverySet(t *testing.T) {
	defaults := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
		"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID",
		"CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
	dup := func() []string { out := make([]string, len(defaults)); copy(out, defaults); return out }
	// Ambient is left empty, as it is in containerd's default spec — the
	// runtime never grants ambient caps, and WithDroppedCapabilities operates
	// only on the bounding/effective/permitted/inheritable sets.
	s := &ocispec.Spec{
		Process: &ocispec.Process{
			Capabilities: &ocispec.LinuxCapabilities{
				Bounding:    dup(),
				Effective:   dup(),
				Permitted:   dup(),
				Inheritable: dup(),
			},
		},
		Linux: &ocispec.Linux{},
	}

	opt := oci.WithDroppedCapabilities(siblingHardenedDropCaps)
	if err := opt(context.Background(), nil, &containers.Container{}, s); err != nil {
		t.Fatalf("applying drop caps: %v", err)
	}

	sets := map[string][]string{
		"bounding":    s.Process.Capabilities.Bounding,
		"effective":   s.Process.Capabilities.Effective,
		"permitted":   s.Process.Capabilities.Permitted,
		"inheritable": s.Process.Capabilities.Inheritable,
	}
	for name, set := range sets {
		for _, c := range set {
			if c == "CAP_NET_RAW" {
				t.Errorf("CAP_NET_RAW still present in %s set after drop", name)
			}
		}
	}
	// A capability the runner keeps must survive the drop.
	kept := false
	for _, c := range s.Process.Capabilities.Bounding {
		if c == "CAP_CHOWN" {
			kept = true
		}
	}
	if !kept {
		t.Error("CAP_CHOWN was dropped; the drop list is too broad")
	}
}

// TestSeccompDefaultApplies confirms the default seccomp profile — part of the
// non-privileged hardening — installs a filter on the spec. seccomp's
// WithDefaultProfile is in-process (no apparmor_parser exec), so this is
// deterministic regardless of host privileges.
func TestSeccompDefaultApplies(t *testing.T) {
	s := &ocispec.Spec{
		Process: &ocispec.Process{
			Capabilities: &ocispec.LinuxCapabilities{
				Bounding: []string{"CAP_CHOWN"},
			},
		},
		Linux: &ocispec.Linux{},
	}
	if err := seccomp.WithDefaultProfile()(context.Background(), nil, &containers.Container{}, s); err != nil {
		t.Fatalf("applying default seccomp: %v", err)
	}
	if s.Linux.Seccomp == nil {
		t.Fatal("default seccomp profile was not applied to the spec")
	}
	if len(s.Linux.Seccomp.Syscalls) == 0 {
		t.Error("seccomp profile has no syscall rules")
	}
}
