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

// openat2Unsupported latches once the kernel is known not to have openat2, so
// the fallback does not pay for a failing syscall on every bind. Atomic
// because sibling container creates are served concurrently.
//
// The latch is process-global and permanent, which is why only errors that
// genuinely mean "this kernel does not implement the syscall" set it — see
// openat2Missing. Anything conditional (a seccomp filter, a transient EPERM)
// falls back for that one call instead, so a single odd error cannot silently
// convert the node's resolver for the rest of the daemon's uptime.
var openat2Unsupported atomic.Bool

// openat2FallbackNote carries the reason the resolver fell back, so it reaches
// the operator's log instead of nothing at all. Set at most once per process;
// delivered at most once, by the first bind that looks (see
// openat2FallbackNotice), because pinBindSource has no logger of its own and
// threading one through every call site would be a lot of churn for a
// once-per-process event.
var (
	openat2Noted     atomic.Bool
	openat2FallbackR atomic.Pointer[string]
)

func noteOpenat2Fallback(reason string) {
	if openat2Noted.CompareAndSwap(false, true) {
		openat2FallbackR.Store(&reason)
	}
}

// openat2FallbackNotice returns the fallback reason exactly once, then "".
// Callers with a logger (buildBindMounts) surface it.
func openat2FallbackNotice() string {
	if r := openat2FallbackR.Swap(nil); r != nil {
		return *r
	}
	return ""
}

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
	if openat2Missing(err) {
		openat2Unsupported.Store(true)
		noteOpenat2Fallback(fmt.Sprintf("openat2(2) is not implemented on this kernel (%v); dind bind sources will be resolved by the equivalent O_PATH|O_NOFOLLOW walk for the rest of this process", err))
		return resolveBeneathWalk(rootFd, comps)
	}
	if openat2Blocked(err) {
		// Conditional, so it does not latch: a seccomp filter that rejects
		// the syscall will reject it again next time and we will fall back
		// again, at the cost of one failing syscall per bind. That is cheap,
		// and it means a one-off EPERM cannot permanently downgrade the node.
		noteOpenat2Fallback(fmt.Sprintf("openat2(2) was refused (%v), most likely by a seccomp filter; dind bind sources are being resolved by the equivalent O_PATH|O_NOFOLLOW walk instead", err))
		return resolveBeneathWalk(rootFd, comps)
	}
	return -1, err
}

// openat2Known reports whether openat2 is worth trying. The probe is the first
// real call; after that the answer is latched.
func openat2Known() bool { return !openat2Unsupported.Load() }

// openat2Missing reports errors that can only mean the kernel does not
// implement openat2 at all: ENOSYS on pre-5.6, and E2BIG for an OpenHow the
// kernel cannot parse. These latch.
func openat2Missing(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.E2BIG)
}

// openat2Blocked reports errors that mean something is refusing the syscall
// rather than lacking it. These fall back without latching.
func openat2Blocked(err error) bool {
	return errors.Is(err, unix.EPERM)
}

// EACCES and EINVAL are deliberately in NEITHER list.
//
// EACCES is an ordinary resolution outcome — one directory along the path we
// may not search — and treating it as "no openat2" would downgrade the whole
// node's resolver because of a single unreadable directory.
//
// EINVAL means a malformed OpenHow (bad flags, or a reserved field set). It is
// unreachable for a correct call: RESOLVE_IN_ROOT shipped in the same release
// as openat2 itself, so there is no kernel that has one without the other. If
// it ever does happen, the bind fails closed with the real error rather than
// quietly switching resolvers. (Note for anyone reading the git history: an
// earlier version of this comment justified excluding EINVAL by claiming
// RESOLVE_IN_ROOT returns it when a path escapes. It does not — it clamps the
// escape instead. EXDEV is RESOLVE_BENEATH's escape error, and we do not use
// RESOLVE_BENEATH. Right call, wrong reason.)

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
// ".." is handled by holding every directory on the way down open and stepping
// back to the one above — never by re-opening a parent by name, which would be
// a second walk of the sort this whole mechanism exists to remove. At the root
// it is a no-op, exactly as RESOLVE_IN_ROOT clamps it.
//
// An earlier version refused ".." outright, reasoning that a lexically cleaned
// bind source cannot contain one. That is true of the source, but not of a
// SYMLINK TARGET, which is spliced into the walk here — and relative "../"
// targets are everywhere in real images (/etc/alternatives/*, Debian
// multiarch, tool caches). It failed closed rather than unsafely, but it made
// this path meaningfully stricter than openat2 while the comment claimed
// equivalence, which would have surfaced as legitimate binds 400ing on any
// node that ever fell back.
//
// This exists so a node on a pre-5.6 kernel degrades to "slower but equally
// contained" rather than to "unprotected" or "dind is broken".
func resolveBeneathWalk(rootFd int, comps []string) (int, error) {
	root, err := unix.Dup(rootFd)
	if err != nil {
		return -1, fmt.Errorf("dup bind root: %w", err)
	}
	// stack[0] is always the bind root; the last element is the current
	// directory. Everything in between is held open so ".." can step back
	// without naming anything.
	stack := []int{root}
	closeStack := func() {
		for _, fd := range stack {
			closePinFd(fd)
		}
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
			if len(stack) > 1 {
				closePinFd(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
			}
			// At the root, ".." is a no-op: there is no "above" to reach.
			continue
		}

		cur := stack[len(stack)-1]
		next, err := unix.Openat(cur, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			closeStack()
			return -1, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(next, &st); err != nil {
			closePinFd(next)
			closeStack()
			return -1, fmt.Errorf("stat %s during bind resolution: %w", name, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFLNK {
			stack = append(stack, next)
			continue
		}

		// Symlink: expand it in place rather than following it by name. The
		// link itself is not descended into, so the stack does not grow.
		links++
		if links > maxPinSymlinks {
			closePinFd(next)
			closeStack()
			return -1, unix.ELOOP
		}
		target, err := readlinkFd(next)
		closePinFd(next)
		if err != nil {
			closeStack()
			return -1, err
		}
		if strings.HasPrefix(target, "/") {
			// Absolute target: re-anchor at the bind root, which is what
			// RESOLVE_IN_ROOT does and what the path means from inside the
			// runner's own namespace.
			for _, fd := range stack[1:] {
				closePinFd(fd)
			}
			stack = stack[:1]
		}
		remaining = append(strings.Split(target, "/"), remaining...)
	}

	result := stack[len(stack)-1]
	for _, fd := range stack[:len(stack)-1] {
		closePinFd(fd)
	}
	return result, nil
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
