package dind

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// bindResolution is the outcome of translating a sibling container's -v
// source from the runner container's mount namespace to a real path on the
// dind daemon's filesystem.
type bindResolution struct {
	// HostPath is the path the dind daemon will hand to containerd as the
	// OCI bind source. It is always on the dind daemon's filesystem.
	HostPath string
	// ForceReadOnly is set when the source resolved to a shared image
	// layer (lowerdir). Writes through that mount would corrupt the
	// cached image for every other job using the same base, so the bind
	// is downgraded to ro regardless of what the client requested.
	ForceReadOnly bool
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
// non-empty, rootfs sources resolve via "<runnerRootfsPath>/<src>" — a
// regular path in the host's mount namespace that points at the same
// merged view the runner sees from inside.
//
// The previous draft of this fix tried "/proc/<pid>/root/<src>" as the
// bind source. That path readlinks correctly, but the kernel refuses it
// at mount(2) because resolving it crosses into the runner's mount
// namespace — bind sources have to be paths in the *calling* process's
// mount namespace. The bundle's rootfs mount is in the host namespace
// so the kernel walks it normally.
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
func translateBindSource(src string, runnerBinds map[string]string, runnerRootfsPath string, upperdir string, lowerdirs []string) (bindResolution, error) {
	// Sources are POSIX paths from the runner's Linux mount namespace;
	// use path (not filepath) so this evaluates consistently on Windows
	// build hosts during testing. Host-side joins below use filepath
	// because the dind daemon's filesystem is native.
	if !path.IsAbs(src) {
		return bindResolution{}, fmt.Errorf("bind source %q must be absolute", src)
	}
	cleaned := path.Clean(src)

	if host, suffix, ok := matchBindPrefix(cleaned, runnerBinds); ok {
		return bindResolution{HostPath: path.Join(host, suffix)}, nil
	}

	if runnerRootfsPath != "" {
		candidate := path.Join(runnerRootfsPath, cleaned)
		if _, err := os.Stat(candidate); err == nil {
			return bindResolution{HostPath: candidate}, nil
		}
		return bindResolution{}, fmt.Errorf("bind source %q is not visible in the runner's rootfs (%s does not exist)", src, candidate)
	}

	if upperdir != "" {
		candidate := path.Join(upperdir, cleaned)
		if _, err := os.Stat(candidate); err == nil {
			return bindResolution{HostPath: candidate}, nil
		}
	}

	for _, lower := range lowerdirs {
		if lower == "" {
			continue
		}
		candidate := path.Join(lower, cleaned)
		if _, err := os.Stat(candidate); err == nil {
			return bindResolution{HostPath: candidate, ForceReadOnly: true}, nil
		}
	}

	return bindResolution{}, fmt.Errorf("bind source %q is not visible to ephemerd dind (not in runner rootfs or known bind table)", src)
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
