//go:build linux

package dind

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// errBindPathTraversal is returned for a bind source that contains a ".."
// component after cleaning (which can only come from a symlink target).
var errBindPathTraversal = errors.New(`bind source contains a ".." component, which is not permitted`)

// maxPinSymlinks bounds symlink expansion in the openat2-less fallback walk.
// Matches the kernel's own MAXSYMLINKS.
const maxPinSymlinks = 40

// openat2Unsupported latches once the kernel is known not to support openat2,
// so the fallback does not pay for a failing syscall on every bind. Atomic
// because sibling container creates are served concurrently.
var openat2Unsupported atomic.Bool

// closePinFd drops a pinned descriptor. Close errors on a descriptor being
// discarded are not actionable.
func closePinFd(fd int) { _ = unix.Close(fd) }

// pinBindSource resolves rel underneath root and returns a handle pinned to
// the resolved inode. rel is a POSIX path relative to root (a leading "/" is
// ignored); an empty rel pins root itself.
//
// Containment is enforced by the KERNEL during resolution, not by a string
// comparison afterwards:
//
//   - openat2(2) with RESOLVE_IN_ROOT treats root as if it were the process
//     root for this one resolution. Symlinks are still followed — the runner's
//     rootfs legitimately contains plenty — but a symlink (or a chain of them,
//     or a "..") that would leave root cannot: an absolute symlink is
//     re-anchored at root, and ".." at root is a no-op. This is exactly the
//     view the runner itself has of its own filesystem, which is the view the
//     `-v` source was written against. It is also why this is openat2 and not
//     Go's os.Root, which rejects absolute symlinks outright and would break
//     every bind traversing merged-usr's /bin -> /usr/bin.
//   - RESOLVE_NO_MAGICLINKS forbids traversing /proc/<pid>/fd style magic
//     links, which would otherwise be a way back out through a descriptor the
//     resolution never inspected.
//
// The result is a descriptor, so there is nothing left for the job to
// re-point. Turning it into something runc can mount is the stager's job.
//
// autoCreate mirrors Docker's behaviour for a `-v` source that does not exist
// yet (the GHA runner emits binds for directories a later step creates). The
// creation is done with mkdirat(2) relative to a pinned parent descriptor and
// each new component is re-opened with O_NOFOLLOW, so a job racing to plant a
// symlink where we are about to mkdir loses: mkdirat fails EEXIST and the
// O_NOFOLLOW|O_DIRECTORY open then fails ENOTDIR/ELOOP rather than following
// it out.
func pinBindSource(root, rel string, autoCreate bool) (*bindPin, error) {
	comps, err := pathComponents(rel)
	if err != nil {
		return nil, err
	}
	logical := logicalPath(root, comps)

	if len(comps) == 0 {
		// Pin root itself. root is a path ephemerd chose (the runner rootfs,
		// or a host source out of its own bind table), never one the job
		// supplied, so following symlinks in it is not attacker-controlled.
		fd, err := unix.Open(root, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("opening bind root %s: %w", root, err)
		}
		return newBindPin(fd, logical)
	}

	rootFd, err := openPathDir(root)
	if err != nil {
		return nil, fmt.Errorf("opening bind root %s: %w", root, err)
	}
	defer closePinFd(rootFd)

	fd, err := resolveBeneath(rootFd, comps)
	if err != nil {
		if !autoCreate || !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		fd, err = createBeneath(rootFd, comps)
		if err != nil {
			return nil, err
		}
	}
	return newBindPin(fd, logical)
}

// openPathDir opens a directory as an O_PATH anchor. O_PATH means the
// descriptor can only be used to name things relative to it — it grants no
// read or write access to the directory's contents, which is all a resolution
// anchor needs.
func openPathDir(p string) (int, error) {
	return unix.Open(p, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// newBindPin wraps an already-open O_PATH descriptor. The mode comes from
// fstat on that same descriptor, so the type check the caller performs is
// about the pinned inode and not about whatever the path names by then.
func newBindPin(fd int, logical string) (*bindPin, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		closePinFd(fd)
		return nil, fmt.Errorf("stat pinned bind source %s: %w", logical, err)
	}
	return &bindPin{
		logical: logical,
		mode:    fileModeFromStat(st.Mode),
		fd:      fd,
	}, nil
}

// fileModeFromStat converts a raw st_mode into the os.FileMode bits the
// callers care about (IsDir / IsRegular).
func fileModeFromStat(m uint32) os.FileMode {
	mode := os.FileMode(m & 0o777)
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	}
	return mode
}

// resolveBeneath opens comps relative to rootFd without ever leaving rootFd,
// returning an O_PATH descriptor for the result.
func resolveBeneath(rootFd int, comps []string) (int, error) {
	if len(comps) == 0 {
		return unix.Dup(rootFd)
	}
	if !openat2Known() {
		return resolveBeneathWalk(rootFd, comps)
	}
	how := &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFd, strings.Join(comps, "/"), how)
	if err == nil {
		return fd, nil
	}
	if isOpenat2Unavailable(err) {
		openat2Unsupported.Store(true)
		return resolveBeneathWalk(rootFd, comps)
	}
	return -1, err
}

// openat2Known reports whether openat2 is worth trying. The probe is the first
// real call; after that the answer is latched.
func openat2Known() bool { return !openat2Unsupported.Load() }

// isOpenat2Unavailable distinguishes "this kernel/sandbox has no openat2" from
// a genuine resolution failure. ENOSYS is a pre-5.6 kernel; EPERM is a seccomp
// filter that does not know the syscall; E2BIG is an OpenHow the kernel does
// not understand.
//
// EACCES and EINVAL are deliberately NOT in this list even though the syscall
// can return them for an unsupported flag set: both are also ordinary
// resolution outcomes (EACCES on a directory we may not search, EINVAL from
// RESOLVE_IN_ROOT rejecting an escape on some kernels), and treating a real
// rejection as "kernel too old" would silently downgrade every subsequent bind
// on the node to the fallback walk.
func isOpenat2Unavailable(err error) bool {
	return errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.E2BIG)
}

// resolveBeneathWalk is the openat2-less fallback: a manual component-by-
// component walk that reproduces RESOLVE_IN_ROOT semantics.
//
// It is race-free for the same reason openat2 is: every step is an openat(2)
// relative to a descriptor we already hold, with O_NOFOLLOW, so a component
// swapped after we have opened it cannot redirect the walk. Symlinks are read
// from the descriptor (readlinkat with an empty path), never re-opened by
// name, and an absolute target restarts the walk at rootFd instead of at the
// node's real root.
//
// This exists so a node on a pre-5.6 kernel degrades to "slower but equally
// contained" rather than to "unprotected" or "dind is broken".
func resolveBeneathWalk(rootFd int, comps []string) (int, error) {
	cur, err := unix.Dup(rootFd)
	if err != nil {
		return -1, fmt.Errorf("dup bind root: %w", err)
	}
	remaining := append([]string(nil), comps...)
	links := 0

	for len(remaining) > 0 {
		name := remaining[0]
		remaining = remaining[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// RESOLVE_IN_ROOT makes ".." at the root a no-op; anywhere else
			// it would need parent tracking. Refusing is stricter and, for
			// a lexically cleaned bind source, unreachable except through a
			// symlink target.
			closePinFd(cur)
			return -1, errBindPathTraversal
		}

		next, err := unix.Openat(cur, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			closePinFd(cur)
			return -1, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil {
			closePinFd(next)
			closePinFd(cur)
			return -1, fmt.Errorf("stat %s during bind resolution: %w", name, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFLNK {
			closePinFd(cur)
			cur = next
			continue
		}

		// Symlink: expand it in place rather than following it by name.
		links++
		if links > maxPinSymlinks {
			closePinFd(next)
			closePinFd(cur)
			return -1, unix.ELOOP
		}
		target, err := readlinkFd(next)
		closePinFd(next)
		if err != nil {
			closePinFd(cur)
			return -1, err
		}
		if strings.HasPrefix(target, "/") {
			// Absolute target: re-anchor at the bind root, which is what
			// RESOLVE_IN_ROOT does and what the path means from inside the
			// runner's own namespace.
			closePinFd(cur)
			if cur, err = unix.Dup(rootFd); err != nil {
				return -1, fmt.Errorf("dup bind root: %w", err)
			}
		}
		remaining = append(strings.Split(target, "/"), remaining...)
	}
	return cur, nil
}

// readlinkFd reads the target of the symlink an O_PATH|O_NOFOLLOW descriptor
// refers to. readlinkat with an empty pathname operates on dirfd itself.
func readlinkFd(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(fd, "", buf)
	if err != nil {
		return "", fmt.Errorf("reading symlink during bind resolution: %w", err)
	}
	if n <= 0 || n >= len(buf) {
		return "", fmt.Errorf("symlink target during bind resolution is empty or too long (%d bytes)", n)
	}
	return string(buf[:n]), nil
}

// createBeneath materializes a bind source that does not exist yet, then pins
// it. Only reached when the caller asked for Docker's auto-mkdir behaviour.
//
// The deepest existing prefix is resolved with the same contained resolver;
// only the missing tail is created, each component with mkdirat(2) against a
// held descriptor and re-opened with O_NOFOLLOW|O_DIRECTORY. New directories
// inherit uid/gid from that closest existing ancestor, which is what lets the
// GHA runner (uid 1001) write into a directory we created on its behalf.
func createBeneath(rootFd int, comps []string) (int, error) {
	for i := len(comps) - 1; i >= 0; i-- {
		parentFd, err := resolveBeneath(rootFd, comps[:i])
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return -1, err
		}
		return createUnder(parentFd, comps[i:])
	}
	return -1, fmt.Errorf("no existing ancestor found for bind source %q beneath the bind root", strings.Join(comps, "/"))
}

// createUnder creates missing under parentFd and returns a pinned descriptor
// for the last component. Takes ownership of parentFd.
func createUnder(parentFd int, missing []string) (int, error) {
	var pst unix.Stat_t
	if err := unix.Fstat(parentFd, &pst); err != nil {
		closePinFd(parentFd)
		return -1, fmt.Errorf("stat bind source ancestor: %w", err)
	}

	cur := parentFd
	for _, name := range missing {
		if err := unix.Mkdirat(cur, name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			closePinFd(cur)
			return -1, fmt.Errorf("creating bind source component %q: %w", name, err)
		}
		// O_NOFOLLOW|O_DIRECTORY: if the mkdirat lost a race to a symlink
		// planted by the job, this fails rather than following it.
		next, err := unix.Openat(cur, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			closePinFd(cur)
			// The common way to get here is that mkdirat returned EEXIST
			// because a symlink already occupies the name — either planted
			// ahead of us or swapped in during the race. O_NOFOLLOW turns
			// that into ELOOP/ENOTDIR instead of an escape, which is the
			// entire point, so say what it means.
			return -1, fmt.Errorf("bind source component %q could not be opened as a directory after creation; something (most likely a symlink whose target escapes the bind root) already occupies that name: %w", name, err)
		}
		// Inherit ownership from the closest pre-existing ancestor so the
		// runner user can populate what we made for it. Done through the
		// descriptor, so it cannot be redirected either.
		if err := unix.Fchownat(next, "", int(pst.Uid), int(pst.Gid), unix.AT_EMPTY_PATH); err != nil {
			closePinFd(next)
			closePinFd(cur)
			return -1, fmt.Errorf("chown auto-created bind source component %q to %d:%d: %w", name, pst.Uid, pst.Gid, err)
		}
		closePinFd(cur)
		cur = next
	}
	return cur, nil
}
