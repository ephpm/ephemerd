package runtime

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
)

// apparmorProfileName is the generated profile ephemerd confines job
// containers with. It mirrors what Docker installs as "docker-default".
//
// AppArmor here is defense in depth, not the only thing standing between a job
// and /proc or /sys. containerd's default spec (applied via
// oci.WithDefaultSpecForPlatform, see Create) already marks /proc/sys and
// /proc/sysrq-trigger read-only, masks /proc/kcore and friends, mounts /sys
// read-only, and installs a deny-all device cgroup; the seccomp profile blocks
// mount(2); and the capability set omits CAP_SYS_ADMIN. The profile adds a
// fourth, independent layer so that a bug or a misconfiguration in any one of
// those does not by itself hand a job write access to kernel tunables.
//
// It is not a blanket "/proc/sys and /sys are read-only" rule, and it should
// not be described as one: the generated template deliberately leaves
// /sys/fs/cgroup/** and /proc/sys/kernel/shm* writable (see the [^c]/[^g]/[^r]
// and shm carve-outs in containerd's contrib/apparmor/template.go), because
// containerized workloads legitimately need both.
const apparmorProfileName = "ephemerd-default"

// Paths the confinement decision reads. All are kernel-provided.
const (
	// apparmorEnabledPath holds "Y"/"N" for the apparmor module parameter.
	apparmorEnabledPath = "/sys/module/apparmor/parameters/enabled"
	// apparmorSecurityFSDir exists only when securityfs is mounted. Minimal
	// images (including the Linux VM images this repo builds) do not always
	// mount it, and without it the profile list below cannot be read at all.
	apparmorSecurityFSDir = "/sys/kernel/security/apparmor"
	// apparmorProfilesPath lists loaded profiles, one "<name> (<mode>)" per line.
	apparmorProfilesPath = apparmorSecurityFSDir + "/profiles"
)

// apparmorEnforceMode is the only profile mode that actually confines. A
// profile in "complain" mode logs violations and permits them, so treating it
// as loaded would leave jobs effectively unconfined while the spec claims
// otherwise.
const apparmorEnforceMode = "enforce"

// apparmorEnv is the set of host facts the confinement decision depends on.
// It is injected rather than called directly so the decision logic can be
// table-tested on any platform, including the Windows and macOS dev hosts
// where none of these paths exist.
type apparmorEnv struct {
	readFile func(name string) ([]byte, error)
	stat     func(name string) (fs.FileInfo, error)
	lookPath func(file string) (string, error)
	// load installs the generated profile into the kernel. On Linux this is
	// containerd's apparmor.LoadDefaultProfile, which shells out to
	// apparmor_parser; it is the only expensive step here.
	load func(name string) error
}

// apparmorResolver decides, per container create, whether job containers can
// be confined and with which profile.
//
// Deliberately not a sync.OnceValue. containerd's LoadDefaultProfile writes the
// profile to a temp file, loads it, and deletes the file — it is never
// persisted under /etc/apparmor.d. A `systemctl reload apparmor` or an apparmor
// package upgrade therefore drops the profile out of the kernel while the
// daemon is still running. A cached "yes, use ephemerd-default" answer would
// make runc fail every container create from that moment until the daemon is
// restarted, which is exactly the pool-down outcome the fail-open below exists
// to avoid. So the cheap check (one small read from securityfs) runs before
// every create, and the profile is reloaded if it has gone missing.
//
// The mutex serializes the whole decision, which keeps the expensive
// apparmor_parser exec from running concurrently when several jobs start at
// once. Reasons are remembered so a persistently unconfined host logs once
// rather than once per job.
type apparmorResolver struct {
	env apparmorEnv

	mu sync.Mutex
	// lastReason is the reason most recently reported to the log. "" means
	// confinement was available at the last check.
	lastReason string
	// reported is false until the first decision has been logged, so that the
	// initial "confinement is active" line is emitted too.
	reported bool
}

// newApparmorResolver builds a resolver over the given host facts.
func newApparmorResolver(env apparmorEnv) *apparmorResolver {
	return &apparmorResolver{env: env}
}

// profile returns the AppArmor profile name to confine a new container with,
// or "" when the host cannot enforce it.
//
// Fail-open is deliberate and matches Docker: a host without usable AppArmor
// still runs jobs under seccomp, a read-only /sys, a deny-all device cgroup and
// the restricted capability set. Hard-failing container creation would take a
// whole pool down on any host that happens not to ship AppArmor. Fail-open
// silently is a different matter, though — an operator needs to be able to
// answer "are my jobs actually confined?", so every transition is logged.
func (r *apparmorResolver) profile(log *slog.Logger) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	name, reason := r.resolveLocked()
	if r.reported && reason == r.lastReason {
		return name
	}
	r.reported = true
	r.lastReason = reason
	if log != nil {
		if reason == "" {
			log.Info("job containers are AppArmor confined", "profile", name, "mode", apparmorEnforceMode)
		} else {
			log.Warn("job containers are running WITHOUT AppArmor confinement; "+
				"seccomp, capability limits and the read-only /proc,/sys mounts still apply",
				"profile", apparmorProfileName, "reason", reason)
		}
	}
	return name
}

// resolveLocked returns the profile name to use, or "" plus a human-readable
// reason why confinement is unavailable. Caller must hold r.mu.
func (r *apparmorResolver) resolveLocked() (string, string) {
	if reason := apparmorHostSupport(r.env); reason != "" {
		return "", reason
	}

	mode, err := r.profileMode()
	if err != nil {
		return "", err.Error()
	}
	switch mode {
	case apparmorEnforceMode:
		return apparmorProfileName, ""
	case "":
		// Not loaded, or dropped by an apparmor reload since the last check.
		// Fall through and (re)load it.
	default:
		// complain/unconfined/kill/prompt. Notably, containerd's
		// LoadDefaultProfile short-circuits on "already loaded" without
		// looking at the mode, so asking it to reload would be a no-op —
		// only an operator running aa-enforce can fix this.
		return "", fmt.Sprintf("profile %q is loaded in %q mode, not %q; it would be applied but not enforced",
			apparmorProfileName, mode, apparmorEnforceMode)
	}

	if err := r.env.load(apparmorProfileName); err != nil {
		return "", fmt.Sprintf("loading profile %q failed: %v", apparmorProfileName, err)
	}
	mode, err = r.profileMode()
	if err != nil {
		return "", err.Error()
	}
	if mode != apparmorEnforceMode {
		if mode == "" {
			return "", fmt.Sprintf("profile %q is still not listed in %s after a successful load",
				apparmorProfileName, apparmorProfilesPath)
		}
		return "", fmt.Sprintf("profile %q loaded in %q mode, not %q", apparmorProfileName, mode, apparmorEnforceMode)
	}
	return apparmorProfileName, ""
}

// profileMode reports the mode the ephemerd profile is loaded in, or "" if it
// is not loaded at all.
func (r *apparmorResolver) profileMode() (string, error) {
	buf, err := r.env.readFile(apparmorProfilesPath)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", apparmorProfilesPath, err)
	}
	return parseApparmorMode(buf, apparmorProfileName), nil
}

// apparmorHostSupport reports why the host cannot enforce AppArmor, or "" if
// it can.
//
// These are not "the checks runc makes" — they are the checks this code's own
// dependencies need:
//
//   - the module parameter, because AppArmor may be compiled in but disabled at
//     boot (apparmor=0);
//   - securityfs, because the loaded-profile list lives there and every mode
//     check below reads it. runc also stats this directory before applying a
//     profile, so its absence means confinement would fail at container start
//     even if we claimed it was on;
//   - apparmor_parser, because containerd's LoadDefaultProfile execs it.
func apparmorHostSupport(env apparmorEnv) string {
	buf, err := env.readFile(apparmorEnabledPath)
	if err != nil {
		return "kernel AppArmor unavailable: cannot read " + apparmorEnabledPath
	}
	if len(buf) == 0 || (buf[0] != 'Y' && buf[0] != 'y') {
		return "AppArmor is disabled in the kernel (" + apparmorEnabledPath + " is not Y)"
	}
	if _, err := env.stat(apparmorSecurityFSDir); err != nil {
		return "securityfs is not mounted at " + apparmorSecurityFSDir
	}
	if _, err := env.lookPath("apparmor_parser"); err != nil {
		return "apparmor_parser is not installed or not in PATH"
	}
	return ""
}

// parseApparmorMode finds name in the contents of
// /sys/kernel/security/apparmor/profiles and returns the mode it is loaded in,
// or "" if it is absent.
//
// Lines are "<name> (<mode>)". Matching on a "<name> " prefix (as containerd's
// own isLoaded does) both accepts complain mode as loaded and would match a
// longer profile whose name merely starts with ours, so the name is compared
// exactly against everything left of the trailing parenthesized mode.
func parseApparmorMode(profiles []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(profiles))
	// Profile names can be long (paths); give the scanner room.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasSuffix(line, ")") {
			continue
		}
		open := strings.LastIndex(line, " (")
		if open < 0 {
			continue
		}
		if line[:open] != name {
			continue
		}
		return line[open+2 : len(line)-1]
	}
	return ""
}
