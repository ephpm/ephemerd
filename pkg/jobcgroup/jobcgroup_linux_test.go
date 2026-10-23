//go:build linux

package jobcgroup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCgroupfs stands in for /sys/fs/cgroup: a temp dir whose root carries a
// cgroup.subtree_control, and a removeDir that behaves like rmdir on
// cgroupfs — refusing a cgroup with processes or child cgroups, removing one
// that only holds interface files.
func fakeCgroupfs(t *testing.T, subtree string) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.subtree_control"), subtree)
	old := removeDir
	removeDir = func(dir string) error {
		if !idle(dir) {
			return &os.PathError{Op: "rmdir", Path: dir, Err: syscall.EBUSY}
		}
		return os.RemoveAll(dir)
	}
	t.Cleanup(func() { removeDir = old })
	return root
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnsure_WritesTheJobBudget(t *testing.T) {
	root := fakeCgroupfs(t, "cpu memory pids")
	if err := Ensure(root, "job-1", Limits{MaxBytes: 16 << 30, HighBytes: 14 << 30}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := filepath.Join(root, Dir("job-1"))
	if got := read(t, filepath.Join(dir, "memory.max")); got != "17179869184" {
		t.Errorf("memory.max = %q, want 16 GiB", got)
	}
	if got := read(t, filepath.Join(dir, "memory.high")); got != "15032385536" {
		t.Errorf("memory.high = %q, want 14 GiB", got)
	}
	// Idempotent, and a later resize rewrites the values.
	if err := Ensure(root, "job-1", Limits{MaxBytes: 8 << 30}); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := read(t, filepath.Join(dir, "memory.high")); got != "max" {
		t.Errorf("memory.high with no soft limit = %q, want max", got)
	}
}

func TestEnsure_NoBudgetCreatesNothing(t *testing.T) {
	root := fakeCgroupfs(t, "memory")
	if err := Ensure(root, "job-1", Limits{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, Dir("job-1"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a job with no memory budget got a parent cgroup (stat err %v)", err)
	}
}

func TestEnsure_EnablesTheMemoryController(t *testing.T) {
	root := fakeCgroupfs(t, "cpu pids")
	if err := Ensure(root, "job-1", Limits{MaxBytes: 1 << 30}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); !strings.Contains(got, "+memory") {
		t.Errorf("subtree_control = %q, want the memory controller enabled", got)
	}
}

func TestRemove_DropsTheParentAndEmptyLeaves(t *testing.T) {
	root := fakeCgroupfs(t, "memory")
	dir := filepath.Join(root, Dir("job-1"))
	write(t, filepath.Join(dir, "memory.max"), "max")
	write(t, filepath.Join(dir, "deadbeef", "cgroup.procs"), "")
	if err := Remove(root, "job-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("job cgroup still present after Remove (stat err %v)", err)
	}
	// Removing what is already gone is success.
	if err := Remove(root, "job-1"); err != nil {
		t.Errorf("Remove of a missing cgroup: %v", err)
	}
}

func TestRemove_LeavesABusyCgroup(t *testing.T) {
	root := fakeCgroupfs(t, "memory")
	dir := filepath.Join(root, Dir("job-1"))
	write(t, filepath.Join(dir, RunnerLeaf, "cgroup.procs"), "4242\n")
	if err := Remove(root, "job-1"); err == nil {
		t.Fatal("Remove succeeded on a job whose runner still has processes")
	}
	if _, err := os.Stat(filepath.Join(dir, RunnerLeaf)); err != nil {
		t.Errorf("busy leaf removed: %v", err)
	}
}

func TestSweepStale(t *testing.T) {
	root := fakeCgroupfs(t, "memory")
	old := time.Now().Add(-time.Hour)
	mk := func(name string, files map[string]string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, v := range files {
			write(t, filepath.Join(dir, f), v)
		}
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	leaked := mk(Prefix+"done-1", map[string]string{"cgroup.procs": "", "memory.max": "max"})
	leaked2 := mk(Prefix+"done-2", nil)
	busy := mk(Prefix+"running", map[string]string{"cgroup.procs": "77\n"})
	parent := mk(Prefix+"with-child", map[string]string{"cid/cgroup.procs": ""})
	if err := os.Chtimes(parent, old, old); err != nil { // the child write bumped it
		t.Fatal(err)
	}
	other := mk("system.slice", nil)
	recent := filepath.Join(root, Prefix+"just-finished")
	if err := os.MkdirAll(recent, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live job between two sibling containers: empty and old, but the
	// daemon is still running it.
	liveJob := mk(Prefix+"live-job", map[string]string{"cgroup.procs": ""})

	n, err := SweepStale(root, 10*time.Minute, time.Now(), map[string]bool{"live-job": true})
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	for _, gone := range []string{leaked, leaked2} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: idle stale job cgroup not removed", filepath.Base(gone))
		}
	}
	for _, kept := range []string{busy, parent, other, recent, liveJob} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s: must be kept (procs, children, not ours, inside the grace period, or live): %v", filepath.Base(kept), err)
		}
	}

	// Once the job is no longer live, its empty parent goes.
	if n, err := SweepStale(root, 10*time.Minute, time.Now(), nil); err != nil || n != 1 {
		t.Errorf("SweepStale with no live jobs = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := os.Stat(liveJob); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("finished job's empty parent not removed")
	}
}

func TestPaths(t *testing.T) {
	if got := Dir("ephemerd-github-ephpm-x"); got != "/ephemerd-dind-ephemerd-github-ephpm-x" {
		t.Errorf("Dir = %q", got)
	}
	if got := RunnerPath("j"); got != "/ephemerd-dind-j/runner" {
		t.Errorf("RunnerPath = %q", got)
	}
}
