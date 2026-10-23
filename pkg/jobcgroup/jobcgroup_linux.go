//go:build linux

package jobcgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// removeDir removes one cgroup directory. On cgroupfs rmdir succeeds on a
// directory that still lists its interface files (they are virtual) and
// fails with EBUSY while it holds processes or child cgroups. Tests swap it
// for os.RemoveAll, since a temp dir standing in for cgroupfs has real files.
var removeDir = os.Remove

// Ensure creates the job parent cgroup under root if it is missing and writes
// its memory budget. Idempotent, and called before every container of the job
// is created, so a parent that SweepStale removed between two of the job's
// containers comes back with its limits rather than without them.
func Ensure(root, jobID string, l Limits) error {
	if !l.Enabled() {
		return nil
	}
	if err := enableMemory(root); err != nil {
		return err
	}
	dir := filepath.Join(root, Dir(jobID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating job cgroup %s: %w", dir, err)
	}
	// max before high: high above max is allowed, a max below the current
	// usage is not refused either (the kernel reclaims), so this order never
	// fails on a resize in either direction.
	if err := writeFile(dir, "memory.max", strconv.FormatInt(l.MaxBytes, 10)); err != nil {
		return err
	}
	high := "max"
	if l.HighBytes > 0 {
		high = strconv.FormatInt(l.HighBytes, 10)
	}
	return writeFile(dir, "memory.high", high)
}

// enableMemory makes sure the memory controller is delegated to root's
// children, without which the job parent has no memory.max to write. On a
// systemd host it already is; writing it again is a no-op.
func enableMemory(root string) error {
	ctl := filepath.Join(root, "cgroup.subtree_control")
	cur, err := os.ReadFile(ctl)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ctl, err)
	}
	for _, c := range strings.Fields(string(cur)) {
		if c == "memory" {
			return nil
		}
	}
	if err := os.WriteFile(ctl, []byte("+memory"), 0o644); err != nil {
		return fmt.Errorf("enabling the memory controller in %s: %w", ctl, err)
	}
	return nil
}

func writeFile(dir, name, value string) error {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(value), 0o644); err != nil {
		return fmt.Errorf("writing %s=%s: %w", p, value, err)
	}
	return nil
}

// Remove deletes the job parent cgroup once its containers are gone: any
// leftover empty child first, then the parent. Missing is success. A cgroup
// that still holds processes is left alone (rmdir refuses it) and reported;
// SweepStale retries it later.
func Remove(root, jobID string) error {
	dir := filepath.Join(root, Dir(jobID))
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// The kernel frees a cgroup a beat after its last task exits;
			// runc's own delete of a leaf can still be in flight.
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading job cgroup %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				_ = removeDir(filepath.Join(dir, e.Name()))
			}
		}
		if lastErr = removeDir(dir); lastErr == nil || errors.Is(lastErr, fs.ErrNotExist) {
			return nil
		}
	}
	return fmt.Errorf("removing job cgroup %s: %w", dir, lastErr)
}

// SweepStale removes job parent cgroups under root that hold no processes and
// no child cgroups and have not changed for grace. It never removes one with
// either: a live job's parent always holds its containers' leaves, and with a
// job memory limit the runner's leaf is in it for the job's whole life.
// Without a limit, a live job's parent can be briefly empty between two
// sibling containers; removing it then is harmless because runc recreates it,
// and the grace period makes it unlikely in the first place.
//
// Returns how many it removed.
func SweepStale(root string, grace time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", root, err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), Prefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !idle(dir) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < grace {
			continue
		}
		if removeDir(dir) == nil {
			removed++
		}
	}
	return removed, nil
}

// idle reports whether a cgroup has no processes and no child cgroups.
// Anything unreadable counts as busy: the sweep only removes what it can
// prove is empty.
func idle(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return false
		}
	}
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && strings.TrimSpace(string(procs)) == ""
}

// Sweep is SweepStale on the real cgroupfs, logged. A sweep failure is never
// fatal — the leak it cleans up is slow and harmless in the short term.
func Sweep(grace time.Duration, log *slog.Logger) {
	n, err := SweepStale(Root, grace, time.Now())
	if err != nil {
		log.Debug("job cgroup sweep failed", "error", err)
		return
	}
	if n > 0 {
		log.Info("removed stale job cgroups", "count", n)
	}
}
