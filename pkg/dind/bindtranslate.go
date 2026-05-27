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
// upperdir / lowerdirs come from the runner snapshot's overlayfs mount
// options. upperdir is the mutable layer where the runner's writes land;
// lowerdirs are the shared, read-only image layers.
//
// Resolution order:
//  1. Longest-prefix match against runnerBinds — /var/run/docker.sock and
//     anything under a known bind destination is translated via the bind
//     table, never re-resolved against the rootfs.
//  2. upperdir match → returned rw. This is the common GHA `container:`
//     case: the runner writes /home/runner/_work/_temp/<uuid>.sh which
//     lives in the runner's upperdir, and the sibling needs to read it
//     (and the next step's wrapper script likewise needs to land back
//     in the same _temp directory, so the mount has to stay rw).
//  3. lowerdir match → returned ro. Image-layer files (e.g.
//     /home/runner/externals) are shared across every container using the
//     same base image, so a rw mount on top of one would corrupt the
//     cache.
//  4. No match → error. The pre-fix shim silently dropped these, which
//     surfaced downstream as "cannot open /__w/_temp/<uuid>.sh".
func translateBindSource(src string, runnerBinds map[string]string, upperdir string, lowerdirs []string) (bindResolution, error) {
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
