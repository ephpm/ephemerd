//go:build linux

package dind

import (
	"log/slog"
	"os"
	"os/exec"

	"github.com/containerd/containerd/v2/contrib/apparmor"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// siblingHardenedDropCaps are the capabilities present in containerd's default
// OCI spec that a NON-privileged dind sibling has no need for and that widen the
// cross-job attack surface.
//
// CAP_NET_RAW is the load-bearing one. It permits crafting raw/ARP frames,
// which is the primitive behind ARP-spoofing the plaintext package-cache
// proxies that ephemerd serves on the bridge gateway (module/npm/pip/… caches):
// a sibling that answers for the gateway's MAC can man-in-the-middle another
// job's dependency fetches and turn that into code execution in the other job's
// build. The remaining entries mirror the capabilities the runner container
// itself drops (pkg/runtime keeps only a 9-cap allowlist), so a sibling is no
// more capable than the runner that launched it.
var siblingHardenedDropCaps = []string{
	"CAP_NET_RAW",
	"CAP_MKNOD",
	"CAP_AUDIT_WRITE",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
}

// siblingApparmorProfileName is the AppArmor profile non-privileged siblings are
// confined with. It reuses the name pkg/runtime loads for job containers so a
// host that already has the profile loaded needs no second parse.
const siblingApparmorProfileName = "ephemerd-default"

// nonPrivilegedHardeningOpts returns the OCI options that confine a
// non-privileged dind sibling to the same sandbox the runner container runs
// under: the unneeded capabilities are dropped, AppArmor is applied
// (best-effort — fail open on hosts that cannot enforce it), and the default
// seccomp profile is applied.
//
// Ordering is deliberate: capabilities are dropped before seccomp, because
// containerd's default seccomp profile is capability-aware and its
// WithDefaultProfile is documented as "must follow the setting of process
// capabilities". Callers apply these on the base spec, before any HostConfig
// CapAdd, so that on a host with the privileged gate OPEN an explicit
// --cap-add can still re-grant a capability (matching Docker's precedence)
// while the default posture stays hardened.
func nonPrivilegedHardeningOpts(log *slog.Logger) []oci.SpecOpts {
	opts := []oci.SpecOpts{
		oci.WithDroppedCapabilities(siblingHardenedDropCaps),
	}
	if apparmorHostCanEnforce() {
		opts = append(opts, apparmor.WithDefaultProfile(siblingApparmorProfileName))
	} else if log != nil {
		log.Debug("dind sibling running WITHOUT AppArmor (host cannot enforce it); " +
			"seccomp and capability limits still apply")
	}
	// seccomp last — after every capability change above.
	opts = append(opts, seccomp.WithDefaultProfile())
	return opts
}

// apparmorHostCanEnforce reports whether the host can load and enforce an
// AppArmor profile. It mirrors the host-support checks pkg/runtime makes;
// duplicated here because pkg/dind cannot import pkg/runtime (that package
// imports this one, so the dependency only runs the other way).
//
// Fail-open is deliberate and matches Docker and the runner path: a host
// without usable AppArmor still runs siblings under seccomp, the dropped
// capabilities, a read-only /proc,/sys and a deny-all device cgroup. Hard
// failing here would take a whole pool down on any host that does not ship
// AppArmor.
func apparmorHostCanEnforce() bool {
	buf, err := os.ReadFile("/sys/module/apparmor/parameters/enabled")
	if err != nil || len(buf) == 0 || (buf[0] != 'Y' && buf[0] != 'y') {
		return false
	}
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err != nil {
		return false
	}
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		return false
	}
	return true
}
