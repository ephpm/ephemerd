//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"sync"

	"github.com/containerd/containerd/v2/contrib/apparmor"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// apparmorProfileName is the generated profile ephemerd confines job
// containers with. It mirrors what Docker installs as "docker-default":
// deny writes to /proc/sys and most of /sys, deny mount, deny ptrace of
// processes outside the profile — the class of things seccomp alone does not
// cover because they are file operations rather than syscalls.
const apparmorProfileName = "ephemerd-default"

// apparmorOpts returns the AppArmor confinement for Linux job containers, or
// nil when the host cannot enforce it (AppArmor absent or disabled — e.g. a
// non-Ubuntu host, or the Linux VM on darwin).
//
// Fail-open is deliberate and matches Docker: a host without AppArmor still
// runs jobs under seccomp + the restricted capability set. The alternative
// (hard-failing container creation) would take the whole pool down on any host
// that happens not to ship AppArmor.
func apparmorOpts() []oci.SpecOpts {
	name := loadedApparmorProfile()
	if name == "" {
		return nil
	}
	return []oci.SpecOpts{apparmor.WithProfile(name)}
}

// loadedApparmorProfile loads the default profile once per process and returns
// its name, or "" if AppArmor is unavailable or the load failed. Loading is
// done here rather than via apparmor.WithDefaultProfile so that a load failure
// degrades to "unconfined" instead of erroring out of every spec build.
var loadedApparmorProfile = sync.OnceValue(func() string {
	if !hostSupportsApparmor() {
		return ""
	}
	if err := apparmor.LoadDefaultProfile(apparmorProfileName); err != nil {
		return ""
	}
	return apparmorProfileName
})

// hostSupportsApparmor reports whether the kernel has AppArmor enabled and the
// userspace parser is present — the same pair of checks runc makes.
func hostSupportsApparmor() bool {
	buf, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil || len(buf) == 0 || (buf[0] != 'Y' && buf[0] != 'y') {
		return false
	}
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		return false
	}
	return true
}
