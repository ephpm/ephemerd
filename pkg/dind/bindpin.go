package dind

import (
	"os"
	"path"
	"strings"
	"sync"
)

// bindPin is a bind source that has been resolved ONCE, under containment, and
// is held so that what eventually gets mounted is the object that was checked.
//
// THE BUG THIS EXISTS TO KILL (issue #125, TOCTOU / check-then-use):
//
// Bind translation maps a sibling container's `-v` source (a path in the
// runner's mount namespace) onto a real path on the dind daemon's filesystem,
// and has to prove the result stays inside the runner's rootfs — otherwise a
// job can ask for `-v /esc/x:/y` where `/esc` is a symlink it planted pointing
// at `/`, and the sibling container receives the node's filesystem.
//
// The pre-fix implementation proved containment by resolving symlinks
// (filepath.EvalSymlinks) and then returned the ORIGINAL, unresolved joined
// string. Everything between that check and runc's mount(2) — the rest of
// translation, the containerd container create, the whole Docker
// create-then-start round trip — was a window in which the job (which owns
// every byte of its own rootfs) could replace a validated directory component
// with a symlink out of the rootfs. runc then walked the string again and
// mounted whatever it pointed at by then. Reproduced against real runc 1.3.4:
// the container saw the swapped target, exit status 0, no error anywhere.
//
// A bindPin removes the second walk. The path is resolved once under
// kernel-enforced containment (openat2 with RESOLVE_IN_ROOT) and the resulting
// inode is held open as an O_PATH descriptor. Renaming or replacing a path
// component afterwards cannot change where the descriptor points.
//
// The descriptor is NOT what goes into the OCI spec. See bindStager: a
// /proc/<pid>/fd/<n> reference is rejected by mount(2) when the mounter is in
// a different mount namespace, which runc always is. The descriptor is instead
// materialized as a bind mount at a path ephemerd owns, and that path is what
// the spec carries.
type bindPin struct {
	// logical is the human-readable path the pin was resolved from
	// (root + relative components, before symlink resolution). Diagnostics,
	// logging and tests — and, off Linux, the source itself, because there
	// is no dind daemon there to defend. Never used as the mount source on
	// Linux: mounting a path is exactly the re-resolution this type exists
	// to prevent.
	logical string
	// mode is the file type/permission bits of the pinned object, captured
	// through the descriptor (not from a second stat of the path). The
	// staging mountpoint is created to match: a directory for a directory,
	// an empty file for a file.
	mode os.FileMode
	// fd is the O_PATH descriptor holding the resolved inode. -1 on
	// platforms with no descriptor pinning (see bindpin_other.go), where
	// there is also no bind translation in production.
	fd int
	// staged is the ephemerd-owned path the pin was published at, once a
	// bindStager has done so. This is what the OCI spec carries.
	staged string
	// unstage tears down whatever the stager created. Set by the stager.
	unstage func() error
	// once makes Close exactly-once. A pin is released from two places that
	// can legitimately both run (cleanupContainer on teardown, and the
	// error paths out of container create), and a double close would free a
	// descriptor number the process may have already handed to something
	// else — a far worse bug than the leak it would be preventing.
	once sync.Once
}

// Logical is the path the pin was resolved from. Diagnostics only on Linux.
func (p *bindPin) Logical() string {
	if p == nil {
		return ""
	}
	return p.logical
}

// Mode is the file mode of the pinned object.
func (p *bindPin) Mode() os.FileMode {
	if p == nil {
		return 0
	}
	return p.mode
}

// Staged is the ephemerd-owned path this pin was published at, or "" if it has
// not been staged yet.
func (p *bindPin) Staged() string {
	if p == nil {
		return ""
	}
	return p.staged
}

// Close releases the staging mount (if any) and the pinned descriptor. Safe on
// nil, safe to call twice, and safe to call concurrently.
func (p *bindPin) Close() error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() {
		if p.unstage != nil {
			err = p.unstage()
		}
		if p.fd >= 0 {
			closePinFd(p.fd)
			p.fd = -1
		}
	})
	return err
}

// closeBindPins releases a batch of pins. Used on every path out of container
// create/start once the mount has happened or been abandoned, and again at
// container cleanup.
func closeBindPins(pins []*bindPin) {
	for _, p := range pins {
		_ = p.Close()
	}
}

// pathComponents splits a POSIX path into its meaningful components, dropping
// "" and "." and rejecting "..".
//
// ".." is refused rather than resolved: the callers pass paths that have
// already been lexically cleaned, so a remaining ".." can only have come from
// a symlink target, and "resolve it and re-check" is exactly the pattern that
// produced the TOCTOU in the first place. Refusing matches what the kernel
// does for us under RESOLVE_IN_ROOT.
func pathComponents(rel string) ([]string, error) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return nil, nil
	}
	parts := strings.Split(rel, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			return nil, errBindPathTraversal
		}
		out = append(out, p)
	}
	return out, nil
}

// logicalPath is the human-readable join of a bind root and its resolved
// components. Deliberately path.Join (POSIX) rather than filepath.Join: the
// components come from a Linux mount namespace, and keeping the join stable
// across build hosts is what lets the cross-platform translation tests assert
// exact strings.
func logicalPath(root string, comps []string) string {
	if len(comps) == 0 {
		return path.Clean(root)
	}
	return path.Join(root, strings.Join(comps, "/"))
}
