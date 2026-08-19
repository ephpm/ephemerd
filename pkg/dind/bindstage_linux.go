//go:build linux

package dind

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// newBindStager builds the Linux bind stager for one job. dataDir is the
// ephemerd data directory; the job's staging mounts land under
// <dataDir>/dind-binds/<jobID>/.
func newBindStager(dataDir, jobID string, log *slog.Logger) bindStager {
	return &mountStager{dir: jobStagingDir(dataDir, jobID), log: log}
}

// mountStager materializes pinned bind sources as bind mounts under a
// per-job directory that only root can reach. See bindStager for why the
// mount is necessary and why handing runc a /proc/<pid>/fd path is not an
// option.
type mountStager struct {
	dir string
	log *slog.Logger

	// mu serialises staging against teardown. It is held across the whole of
	// stage() — including the mkdir and the mount — rather than just the
	// bookkeeping, because teardown removes the directory tree and a mount
	// appearing between its mount-check and its os.RemoveAll would have
	// RemoveAll delete the files visible through that mount, i.e. the
	// runner's rootfs. Staging a handful of binds per container create is not
	// a contended path; correctness here is worth more than the concurrency.
	mu sync.Mutex
	// ready is set once the staging directory exists and has been proven
	// safe. closed is set by teardown and never cleared: a stager that has
	// been torn down must refuse to stage again rather than silently
	// recreating the directory the caller just swept, which would leak a
	// mount pinning the runner's rootfs until the next daemon startup.
	ready  bool
	closed bool
	seq    uint64
}

// stage binds the pinned inode to <dir>/<n> and returns that path.
//
// The name is a bare counter. Nothing derived from the job's requested source
// goes into it: the staging path is the one part of this whole mechanism the
// job must have no influence over, down to the characters in it.
func (m *mountStager) stage(p *bindPin) (string, error) {
	if p == nil || p.fd < 0 {
		return "", fmt.Errorf("bind source was not pinned; refusing to stage an unpinned source")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return "", fmt.Errorf("this job's bind staging directory has already been torn down; refusing to stage %s (the job is shutting down)", p.logical)
	}
	if err := m.ensureDirLocked(); err != nil {
		return "", err
	}
	m.seq++
	target := filepath.Join(m.dir, strconv.FormatUint(m.seq, 10))

	// The mountpoint has to match the source's type: a directory for a
	// directory, an empty regular file for anything else (bind mounts of
	// files are how /etc/hosts-shaped sources work).
	if p.mode.IsDir() {
		if err := os.Mkdir(target, 0o700); err != nil {
			return "", fmt.Errorf("creating bind staging mountpoint %s: %w", target, err)
		}
	} else {
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", fmt.Errorf("creating bind staging mountpoint %s: %w", target, err)
		}
		_ = f.Close()
	}

	// /proc/self/fd/<n> is resolvable here because this IS the process that
	// opened it and this IS its mount namespace — the two conditions runc
	// cannot satisfy. MS_REC mirrors the "rbind" the spec asks for, so a
	// source that has submounts under it carries them across.
	src := "/proc/self/fd/" + strconv.Itoa(p.fd)
	if err := unix.Mount(src, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		_ = os.Remove(target)
		return "", fmt.Errorf("staging bind source %s at %s: %w "+
			"(ephemerd binds every job-supplied -v source into %s before handing it to the container runtime, "+
			"so the source cannot be swapped between validation and mount; "+
			"this requires ephemerd to run as root with CAP_SYS_ADMIN on a writable data directory)",
			p.logical, target, err, m.dir)
	}

	p.staged = target
	// Releasing one pin takes the same lock, so a pin close can never land
	// its unmount inside teardown's check-then-remove window either.
	p.unstage = func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		return unmountAndRemove(target)
	}
	return target, nil
}

// ensureDirLocked creates the per-job staging directory on first use and
// proves it is somewhere a job cannot reach.
//
// The whole point of staging is that runc's second walk of the path is safe,
// and that is only true if every component of it belongs to root and is not
// writable by anyone else. Checking it is cheap (once per job) and it is the
// assumption the entire fix rests on, so it is checked rather than assumed.
func (m *mountStager) ensureDirLocked() error {
	if m.ready {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("creating bind staging dir %s: %w", m.dir, err)
	}
	// MkdirAll honours the umask and skips existing dirs, so set the mode
	// explicitly on both the job dir and its parent.
	//
	// The symlink check comes BEFORE the chmod, not after: os.Chmod follows
	// symlinks, so chmodding first would apply 0700 to whatever a planted
	// <data>/dind-binds symlink pointed at — modifying something outside the
	// staging tree on the way to refusing to use it. ensureTrustedAncestry
	// below repeats the check as part of the full ancestry walk; this is the
	// narrower one that has to happen first.
	for _, d := range []string{stagingRootParent(m.dir), m.dir} {
		info, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("stat bind staging dir %s: %w", d, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bind staging dir %s is a symlink; every component of the staging path must be a real directory", d)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("securing bind staging dir %s: %w", d, err)
		}
	}
	if err := ensureTrustedAncestry(m.dir); err != nil {
		return fmt.Errorf("bind staging dir %s is not safe to mount into: %w", m.dir, err)
	}

	// Best-effort: make the job's staging directory its own private mount so
	// the binds published inside it do not propagate into every other mount
	// namespace on the node. Not a security boundary — what gets staged is
	// the runner's own rootfs, which the runner can already see — so a
	// failure here is logged and tolerated rather than failing the job.
	if err := unix.Mount(m.dir, m.dir, "", unix.MS_BIND, ""); err != nil {
		m.logf("could not self-bind the bind staging dir; staged mounts will propagate normally", "path", m.dir, "error", err)
	} else if err := unix.Mount("", m.dir, "", unix.MS_PRIVATE, ""); err != nil {
		m.logf("could not make the bind staging dir private; staged mounts will propagate normally", "path", m.dir, "error", err)
	}

	m.ready = true
	return nil
}

// teardown removes every mount and directory this job staged. Idempotent, and
// final: nothing can be staged afterwards.
//
// The lock is held across the unmount-and-remove, not just around the flags.
// unmountTreeAndRemove checks that no mounts remain and then calls
// os.RemoveAll; a stage() landing a mount between those two steps would have
// RemoveAll delete through it, destroying the runner's rootfs contents before
// failing with EBUSY on the mountpoint.
func (m *mountStager) teardown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	ready := m.ready
	m.ready = false
	m.closed = true
	if !ready {
		// Nothing was ever staged; the directory may not even exist. Still
		// try the removal, since a create that failed halfway can leave it.
		if _, err := os.Stat(m.dir); err != nil {
			return
		}
	}
	if err := unmountTreeAndRemove(m.dir); err != nil {
		m.logf("failed to clean up bind staging dir", "path", m.dir, "error", err)
	}
}

func (m *mountStager) logf(msg string, args ...any) {
	if m.log != nil {
		m.log.Warn(msg, args...)
	}
}

// stagingRootParent returns the parent of a per-job staging dir, i.e. the
// <data>/dind-binds root.
func stagingRootParent(jobDir string) string { return filepath.Dir(jobDir) }

// sweepStagedBinds is the startup half of the lifecycle: it unmounts and
// removes staging directories left by a previous ephemerd process. See
// SweepStagedBinds for why leaked staging mounts are worse than untidy.
func sweepStagedBinds(root string, log *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // never created, or already gone
	}
	swept := 0
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if err := unmountTreeAndRemove(dir); err != nil {
			if log != nil {
				log.Warn("failed to sweep leaked dind bind staging dir", "path", dir, "error", err)
			}
			continue
		}
		swept++
	}
	if swept > 0 && log != nil {
		log.Info("swept leaked dind bind staging dirs", "count", swept, "root", root)
	}
}

// unmountTreeAndRemove detaches every mount at or below dir, deepest first,
// and then removes the (now mount-free) directory.
//
// The removal is guarded on the unmount actually having worked. os.RemoveAll
// over a live bind mount deletes the files visible THROUGH the mount — here
// that would be the runner's own rootfs — so "could not unmount" must mean
// "leave it alone and complain", never "delete anyway".
func unmountTreeAndRemove(dir string) error {
	return unmountTreeAndRemoveWith(dir, mountPointsUnder, detachMount)
}

// unmountTreeAndRemoveWith is unmountTreeAndRemove with its two syscall-backed
// steps injectable. The seam exists because the "still mounted → do not
// remove" branch is the most consequential line in this file and there is no
// way to provoke it for real: as root, MNT_DETACH does not fail, so a test
// that tries to leave a mount behind on purpose cannot.
func unmountTreeAndRemoveWith(dir string, list func(string) ([]string, error), detach func(string)) error {
	mounts, err := list(dir)
	if err != nil {
		return err
	}
	// Deepest first: a parent cannot be unmounted while a child mount sits
	// inside it (and MNT_DETACH on the parent would hide, not release, the
	// children).
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	for _, mp := range mounts {
		detach(mp)
	}
	remaining, err := list(dir)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("%d bind staging mount(s) still present under %s (first: %s); not removing the directory, because deleting through a live bind mount would delete the source", len(remaining), dir, remaining[0])
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing bind staging dir %s: %w", dir, err)
	}
	return nil
}

// unmountAndRemove releases one staged bind source.
func unmountAndRemove(target string) error {
	detachMount(target)
	mounts, err := mountPointsUnder(target)
	if err != nil {
		return err
	}
	if len(mounts) > 0 {
		return fmt.Errorf("bind staging mount %s could not be detached; leaving the mountpoint in place rather than deleting through it", target)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing bind staging mountpoint %s: %w", target, err)
	}
	return nil
}

// detachMount lazily unmounts mp, repeating for stacked mounts at the same
// point. MNT_DETACH so a descriptor someone still holds cannot turn teardown
// into EBUSY; the mount goes away once the last reference does.
func detachMount(mp string) {
	const maxStacked = 16
	for i := 0; i < maxStacked; i++ {
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
			return // EINVAL once mp is no longer a mount point
		}
	}
}

// mountPointsUnder returns every mount point in this process's mount namespace
// that is dir or lives beneath it.
func mountPointsUnder(dir string) ([]string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("reading mount table: %w", err)
	}
	defer f.Close()

	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo: id parent major:minor root mountpoint options...
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountPath(fields[4])
		if mp == dir || strings.HasPrefix(mp, prefix) {
			out = append(out, mp)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading mount table: %w", err)
	}
	return out, nil
}

// unescapeMountPath undoes the octal escaping the kernel applies to mountinfo
// paths (space, tab, newline and backslash).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ensureTrustedAncestry verifies that dir and every directory above it is
// owned by root (or by us, if ephemerd is somehow not root) and is not
// writable by anyone else. A world-writable directory is accepted only when it
// carries the sticky bit, which is what makes /tmp safe: entries in a sticky
// directory can only be renamed or removed by their owner.
//
// This is the precondition that makes runc's re-walk of the staging path safe.
// If it does not hold, staging provides no protection and the bind must fail
// rather than pretend.
func ensureTrustedAncestry(dir string) error {
	euid := uint32(os.Geteuid())
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("%s is a symlink; every component of the staging path must be a real directory", p)
		}
		if st.Uid != 0 && st.Uid != euid {
			return fmt.Errorf("%s is owned by uid %d, which is neither root nor ephemerd's uid %d", p, st.Uid, euid)
		}
		const otherWrite = 0o022
		if st.Mode&otherWrite != 0 && st.Mode&unix.S_ISVTX == 0 {
			return fmt.Errorf("%s is writable by group or other (mode %04o) without the sticky bit, so a job with any foothold there could swap it", p, st.Mode&0o7777)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}
