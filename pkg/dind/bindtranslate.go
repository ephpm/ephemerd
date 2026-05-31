package dind

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
		switch info, err := os.Stat(candidate); {
		case err == nil:
			if info.IsDir() || info.Mode().IsRegular() {
				return bindResolution{HostPath: candidate}, nil
			}
			return bindResolution{}, fmt.Errorf("bind source %q resolves to %s, which is not a regular file or directory (mode %s)", src, candidate, info.Mode())
		case errors.Is(err, os.ErrNotExist):
			// Mirror Docker's auto-mkdir-on-missing-source semantic. The
			// GHA runner emits -v entries for paths the runner creates
			// lazily inside a step (e.g. /home/runner/_work/_actions
			// only exists once actions/checkout downloads its handler).
			// Real Docker creates the missing dir at create time and
			// the workflow proceeds. Our dind has to do the same or
			// every container: job 400s on the first lazy bind source.
			if mkErr := ensureBindSourceDir(candidate); mkErr != nil {
				return bindResolution{}, fmt.Errorf("bind source %q could not be auto-created at %s: %w", src, candidate, mkErr)
			}
			return bindResolution{HostPath: candidate}, nil
		default:
			return bindResolution{}, fmt.Errorf("bind source %q could not be stat'd at %s: %w", src, candidate, err)
		}
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

// ensureBindSourceDir creates target (and any missing intermediate dirs
// between it and the closest existing ancestor) so a bind for a path the
// runner hasn't materialized yet can still proceed. Mirrors Docker's
// behavior for missing -v sources.
//
// Newly-created directories inherit ownership from the closest existing
// ancestor (Linux only, no-op elsewhere). This matters for the GHA
// `container:` flow: the closest ancestor is typically /home/runner/_work
// owned by uid 1001 (the runner user), so children we create are also
// uid 1001 — the runner can write into them once a step downloads an
// action or stages a file. Without the ownership flow, the new dir is
// root-owned and the runner gets EACCES the first time it tries to
// populate it.
func ensureBindSourceDir(target string) error {
	ancestor := target
	var newDirs []string
	for {
		info, err := os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("ancestor %s is not a directory", ancestor)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat ancestor %s: %w", ancestor, err)
		}
		newDirs = append(newDirs, ancestor)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("walked past root without finding existing ancestor of %s", target)
		}
		ancestor = parent
	}
	if len(newDirs) == 0 {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", target, err)
	}
	return chownNewDirsLikeAncestor(newDirs, ancestor)
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
