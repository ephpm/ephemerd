package runtime

import (
	"runtime"
	"slices"
	"testing"
)

// dangerousCapabilities are the capabilities that, granted to a CI job running
// as root in a container that shares the host user namespace, hand an attacker
// a documented escape. They must never appear in containerCapabilities.
//
//	CAP_SYS_ADMIN   — mount(2), the single broadest escape primitive
//	CAP_SYS_MODULE  — load a kernel module; game over immediately
//	CAP_SYS_RAWIO   — raw port/memory access
//	CAP_SYS_PTRACE  — attach to processes outside the container's cgroup
//	CAP_NET_ADMIN   — reconfigure the host's networking
//	CAP_DAC_READ_SEARCH — open_by_handle_at, the "shocker" escape
//	CAP_MKNOD       — create a block device for the host disk and read it raw
//	CAP_SYS_BOOT / CAP_SYS_TIME — affect the host, not just the container
var dangerousCapabilities = []string{
	"CAP_SYS_ADMIN",
	"CAP_SYS_MODULE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_PTRACE",
	"CAP_NET_ADMIN",
	"CAP_DAC_READ_SEARCH",
	"CAP_MKNOD",
	"CAP_SYS_BOOT",
	"CAP_SYS_TIME",
}

// TestContainerCapabilities_NoDangerousCaps guards the capability allowlist.
// Adding any of these to make one workflow happy would silently remove a
// containment layer for every job on the fleet.
func TestContainerCapabilities_NoDangerousCaps(t *testing.T) {
	for _, bad := range dangerousCapabilities {
		if slices.Contains(containerCapabilities, bad) {
			t.Errorf("containerCapabilities grants %s; see the comment on the var before re-adding it", bad)
		}
	}
}

// TestContainerCapabilities_KeepsCIEssentials is the other half of the guard:
// trimming the list too far breaks apt-get/dpkg/sudo in every job, so the
// minimum working set is pinned here too.
func TestContainerCapabilities_KeepsCIEssentials(t *testing.T) {
	essential := []string{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FOWNER",
		"CAP_SETGID",
		"CAP_SETUID",
	}
	for _, want := range essential {
		if !slices.Contains(containerCapabilities, want) {
			t.Errorf("containerCapabilities is missing %s, which dpkg/sudo need", want)
		}
	}
}

// TestApparmorOpts mirrors TestSeccompOpts: the profile is applied on Linux
// and is a no-op elsewhere. On a Linux host without AppArmor installed the
// result is also nil (fail-open), so this only asserts the platform split.
func TestApparmorOpts(t *testing.T) {
	opts := apparmorOpts()
	if runtime.GOOS != "linux" {
		if opts != nil {
			t.Errorf("apparmorOpts() on %s = %v, want nil", runtime.GOOS, opts)
		}
		return
	}
	// On Linux the result depends on whether the host has AppArmor; both
	// outcomes are valid, so just assert it does not panic and is well-formed.
	for i, o := range opts {
		if o == nil {
			t.Errorf("apparmorOpts()[%d] is nil", i)
		}
	}
}
