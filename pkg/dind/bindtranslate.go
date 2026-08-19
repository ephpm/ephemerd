package dind

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// bindResolution is the outcome of translating a sibling container's -v
// source from the runner container's mount namespace to a real path on the
// dind daemon's filesystem.
type bindResolution struct {
	// ResolvedPath is the path the source resolved to, before symlink
	// resolution. It is the OCI bind source ONLY for sources that carry no
	// job-controlled component (Pin == nil). For everything else it is
	// diagnostics: mounting it is the second path walk issue #125 is about.
	ResolvedPath string
	// ForceReadOnly is set when the source resolved to a shared image
	// layer (lowerdir). Writes through that mount would corrupt the
	// cached image for every other job using the same base, so the bind
	// is downgraded to ro regardless of what the client requested.
	ForceReadOnly bool
	// Pin holds the resolved inode open. Non-nil whenever any part of the
	// source came from the job. The caller must stage it (bindStager) to
	// obtain the path for the OCI spec, and must Close it when the
	// container goes away. On error nothing is left open.
	Pin *bindPin
}

// translateBindSource maps a bind source path the sibling container received
// (which the runner specified relative to its own mount namespace) to a real
// path on the dind daemon's filesystem.
//
// runnerBinds is a map of (runner mount destination → host source) covering
// non-rootfs mounts ephemerd installed into the runner (/var/run/docker.sock,
// /etc/hosts, /etc/resolv.conf, the embedded runner directory, etc.).
//
// runnerRootfsPath is the host-namespace path where the runner container's
// merged overlay is mounted by runc (typically
// "/run/containerd/io.containerd.runtime.v2.task/<ns>/<id>/rootfs"). When
// non-empty, rootfs sources resolve beneath it — a regular path in the host's
// mount namespace that points at the same merged view the runner sees from
// inside.
//
// An earlier draft used "/proc/<pid>/root/<src>" as the bind source. That path
// readlinks correctly, but the kernel refuses it at mount(2) because resolving
// it crosses into the runner's mount namespace — bind sources have to be paths
// in the *calling* process's mount namespace. The bundle's rootfs mount is in
// the host namespace so the kernel walks it normally.
//
// upperdir / lowerdirs are the explicit layer paths for the test path —
// real production calls always pass runnerRootfsPath != "".
//
// Resolution order:
//  1. Longest-prefix match against runnerBinds.
//  2. <runnerRootfsPath>/<src> when the rootfs path is registered. The
//     directory at that path is the merged overlay, so files split
//     across image layers (e.g. /home/runner/externals/node20/bin/node)
//     are reachable. Returned rw; writes copy-up into the runner's
//     own upperdir, which is the runner's own writable layer.
//  3. Upperdir match (fallback for tests with no rootfs path).
//  4. Lowerdir match (fallback for tests; forced ro).
//  5. No match → error. Loud failure replaces the pre-fix silent drop.
//
// SECURITY (issue #125): every branch whose path contains anything the job
// chose resolves through pinBindSource, which contains the resolution inside
// that branch's root and returns a HELD DESCRIPTOR rather than a string.
//
//   - Containment matters because a job owns its own rootfs and can plant a
//     symlink to "/" anywhere in it; the per-job runner directory (which
//     appears in runnerBinds) is likewise bind-mounted into the runner and
//     therefore job-writable, so the bind-table branch is just as
//     attacker-controlled as the rootfs branch — and it previously had no
//     containment check at all.
//   - The descriptor matters because a containment check on a path that is
//     then handed onward as a string is a check on an object that no longer
//     has to be the object that gets mounted.
//
// On success the caller owns bindResolution.Pin: it must stage it and close it.
func translateBindSource(src string, runnerBinds map[string]string, runnerRootfsPath string, upperdir string, lowerdirs []string) (bindResolution, error) {
	// Sources are POSIX paths from the runner's Linux mount namespace;
	// use path (not filepath) so this evaluates consistently on Windows
	// build hosts during testing.
	if !path.IsAbs(src) {
		return bindResolution{}, fmt.Errorf("bind source %q must be absolute", src)
	}
	cleaned := path.Clean(src)

	if host, suffix, ok := matchBindPrefix(cleaned, runnerBinds); ok {
		if suffix == "" {
			// The bind point itself (e.g. /var/run/docker.sock → the per-job
			// dind socket, /etc/hosts → the per-job hosts file, the runner
			// mount → the per-job runner directory). Every component of the
			// host path was chosen by ephemerd and lives in a directory the
			// job has no handle on, so there is nothing here for a symlink
			// swap to act on: no pin, no staging, passed through exactly as
			// before. This is also why /var/run/docker.sock keeps working —
			// it is a socket, which is not a valid pin target.
			return bindResolution{ResolvedPath: host}, nil
		}
		// A non-empty suffix is attacker-supplied. The important case is the
		// runner directory: it is bind-mounted INTO the runner and is
		// therefore fully job-writable, so `-v <runner-mount>/evil/x:/y` with
		// `evil` a planted symlink was a straight escape. Resolve it strictly
		// beneath the host source and pin the result.
		pin, err := pinBindSource(host, suffix, true)
		if err != nil {
			return bindResolution{}, fmt.Errorf("bind source %q rejected: %w", src, err)
		}
		if err := rejectUnbindableType(src, pin); err != nil {
			return bindResolution{}, err
		}
		return bindResolution{ResolvedPath: pin.Logical(), Pin: pin}, nil
	}

	if runnerRootfsPath != "" {
		// Mirror Docker's auto-mkdir-on-missing-source semantic. The GHA
		// runner emits -v entries for paths it creates lazily inside a step
		// (e.g. /home/runner/_work/_actions only exists once actions/checkout
		// downloads its handler). Real Docker creates the missing dir at
		// create time and the workflow proceeds; our dind has to do the same
		// or every `container:` job 400s on the first lazy bind source. The
		// creation itself is contained and symlink-safe — see pinBindSource.
		pin, err := pinBindSource(runnerRootfsPath, cleaned, true)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return bindResolution{}, fmt.Errorf("bind source %q could not be resolved under the runner rootfs; a path component may be a symlink whose target escapes it, and contained resolution never follows one out: %w", src, err)
			}
			return bindResolution{}, fmt.Errorf("bind source %q rejected: %w", src, err)
		}
		if err := rejectUnbindableType(src, pin); err != nil {
			return bindResolution{}, err
		}
		return bindResolution{ResolvedPath: pin.Logical(), Pin: pin}, nil
	}

	if upperdir != "" {
		if pin, err := pinBindSource(upperdir, cleaned, false); err == nil {
			return bindResolution{ResolvedPath: pin.Logical(), Pin: pin}, nil
		}
	}

	for _, lower := range lowerdirs {
		if lower == "" {
			continue
		}
		if pin, err := pinBindSource(lower, cleaned, false); err == nil {
			return bindResolution{ResolvedPath: pin.Logical(), ForceReadOnly: true, Pin: pin}, nil
		}
	}

	return bindResolution{}, fmt.Errorf("bind source %q is not visible to ephemerd dind (not in runner rootfs or known bind table)", src)
}

// rejectUnbindableType refuses a pinned source that is neither a directory nor
// a regular file, closing the pin on the way out.
//
// Applied to every pinned branch, not just the rootfs one. The runner
// directory reachable through the bind table is job-writable, so a job can put
// a FIFO or a unix socket under it and ask for it by name; the mountpoint the
// stager would create for it is a plain file, and binding a FIFO onto one is
// at best confusing and at worst a container that blocks forever on open.
// There is no escalation either way — this is about the two pinned branches
// answering the same question the same way.
//
// The exact-match bind-table entries are deliberately not subject to this:
// /var/run/docker.sock IS a socket, it is ephemerd's own, and it is passed
// through unpinned.
func rejectUnbindableType(src string, pin *bindPin) error {
	mode := pin.Mode()
	if mode.IsDir() || mode.IsRegular() {
		return nil
	}
	_ = pin.Close()
	return fmt.Errorf("bind source %q resolves to something that is not a regular file or directory (mode %s)", src, mode)
}

// matchBindPrefix returns the host source for the longest runnerBinds key
// that contains src, along with the leftover suffix within that bind.
// Longest-prefix wins so a child mount (e.g. /etc/hosts) is preferred over
// a parent (/etc) when both are registered.
func matchBindPrefix(src string, binds map[string]string) (host string, suffix string, ok bool) {
	if len(binds) == 0 {
		return "", "", false
	}
	keys := make([]string, 0, len(binds))
	for k := range binds {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if src == k {
			return binds[k], "", true
		}
		if strings.HasPrefix(src, k+"/") {
			return binds[k], strings.TrimPrefix(src, k+"/"), true
		}
	}
	return "", "", false
}
