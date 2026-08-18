//go:build !linux

package dind

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errBindPathTraversal is returned for a bind source that contains a ".."
// component after cleaning (which can only come from a symlink target).
var errBindPathTraversal = errors.New(`bind source contains a ".." component, which is not permitted`)

// closePinFd is a no-op off Linux: nothing here holds a descriptor.
func closePinFd(int) {}

// pinBindSource is the non-Linux implementation. It exists so the translation
// logic and its tests build and run on a Windows or macOS dev host.
//
// IT IS NOT RACE-FREE, AND IT IS NOT A PRODUCTION PATH. There is no dind
// daemon off Linux: sibling containers are created by the linux build of
// ephemerd, either on a Linux node or inside the managed Linux VM on a
// Windows/macOS node, and bind translation is not wired into the Windows or
// macOS container paths at all (see runtime.go and containers.go). This
// implementation resolves symlinks and then compares strings — the same
// check-then-use shape bindPin exists to eliminate — and it returns a plain
// path rather than a descriptor, because there is no /proc/<pid>/fd here and
// nothing to hand it to.
//
// Keeping it deliberately simple (rather than reimplementing a contained walk
// on Windows semantics) means the cross-platform tests exercise the policy —
// which sources are accepted, which are rejected, what gets auto-created —
// while the Linux file carries the security property.
func pinBindSource(root, rel string, autoCreate bool) (*bindPin, error) {
	comps, err := pathComponents(rel)
	if err != nil {
		return nil, err
	}
	// logicalPath (POSIX join) is what callers compare against; the native
	// filesystem calls below are happy with forward slashes on Windows too.
	target := logicalPath(root, comps)

	info, err := os.Stat(target)
	switch {
	case err == nil:
		if cerr := containedAfterResolve(root, target); cerr != nil {
			return nil, cerr
		}
		return &bindPin{logical: target, mode: info.Mode(), fd: -1}, nil
	case errors.Is(err, fs.ErrNotExist) && autoCreate:
		if cerr := closestExistingAncestorContained(root, target); cerr != nil {
			return nil, cerr
		}
		if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
			return nil, fmt.Errorf("creating bind source %s: %w", target, mkErr)
		}
		if cerr := containedAfterResolve(root, target); cerr != nil {
			return nil, cerr
		}
		info, serr := os.Stat(target)
		if serr != nil {
			return nil, serr
		}
		return &bindPin{logical: target, mode: info.Mode(), fd: -1}, nil
	default:
		return nil, err
	}
}

// containedAfterResolve reports whether target, fully symlink-resolved, is
// root or lives underneath it.
func containedAfterResolve(root, target string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving bind root %s: %w", root, err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolving bind candidate %s: %w", target, err)
	}
	if !isWithin(realRoot, realTarget) {
		return fmt.Errorf("resolved path %s escapes bind root %s", realTarget, realRoot)
	}
	return nil
}

// closestExistingAncestorContained walks up from a path that does not exist
// yet to the first component that does, and checks containment there — so
// auto-mkdir cannot create a directory outside the root through a symlinked
// intermediate.
func closestExistingAncestorContained(root, target string) error {
	ancestor := target
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			return containedAfterResolve(root, ancestor)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("resolving ancestor %s: %w", ancestor, err)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("walked past root resolving ancestor of %s", target)
		}
		ancestor = parent
	}
}

// isWithin reports whether target is root itself or lives underneath root.
// Both arguments are expected to be cleaned, symlink-resolved absolute paths.
// The separator-terminated prefix check prevents "/a/rootfs-evil" from
// matching root "/a/rootfs".
func isWithin(root, target string) bool {
	if target == root {
		return true
	}
	rootWithSep := root
	if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
		rootWithSep += string(filepath.Separator)
	}
	return strings.HasPrefix(target, rootWithSep)
}
