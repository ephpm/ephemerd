package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func mkJobFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRemoveJobWorkdir_RemovesWholeDir proves the primary per-job fix: the
// entire <data>/jobs/<id>/ tree is removed on completion, not just docker/.
func TestRemoveJobWorkdir_RemovesWholeDir(t *testing.T) {
	data := t.TempDir()
	const jobID = "ephemerd-github-ephpm-fast_shannon"
	// dind's docker/ subdir plus a runner _work tree with an extracted php-sdk.
	mkJobFile(t, filepath.Join(data, "jobs", jobID, "docker", "d.sock"))
	mkJobFile(t, filepath.Join(data, "jobs", jobID, "_work", "php-sdk", "libphp.a"))

	removeJobWorkdir(data, jobID, slog.Default())

	if _, err := os.Stat(filepath.Join(data, "jobs", jobID)); !os.IsNotExist(err) {
		t.Errorf("job workdir should be fully removed, stat err = %v", err)
	}
}

// TestRemoveJobWorkdir_MissingIsNoop ensures a job that never created a workdir
// (dind disabled) doesn't error on completion.
func TestRemoveJobWorkdir_MissingIsNoop(t *testing.T) {
	data := t.TempDir()
	removeJobWorkdir(data, "never-ran", slog.Default())
	// No panic / no error path to assert; reaching here is success.
}

// TestRemoveJobWorkdir_EmptyArgsNoop guards the nil-safety of the helper.
func TestRemoveJobWorkdir_EmptyArgsNoop(t *testing.T) {
	removeJobWorkdir("", "id", slog.Default())
	removeJobWorkdir(t.TempDir(), "", slog.Default())
}

// TestCleanOrphanJobDirs_SweepsLeftoversKeepsRunning is the startup-sweep +
// running-job-guard test: orphan job dirs are removed, but a dir whose name is
// in the keep set (a currently-running job) is preserved.
func TestCleanOrphanJobDirs_SweepsLeftoversKeepsRunning(t *testing.T) {
	data := t.TempDir()
	const running = "ephemerd-github-ephpm-live_curie"
	const orphanA = "ephemerd-github-ephpm-stale_galileo"
	const orphanB = "ephemerd-github-ephpm-stale_newton"
	mkJobFile(t, filepath.Join(data, "jobs", running, "_work", "keep.txt"))
	mkJobFile(t, filepath.Join(data, "jobs", orphanA, "docker", "d.sock"))
	mkJobFile(t, filepath.Join(data, "jobs", orphanB, "_work", "php-sdk", "libphp.a"))

	CleanOrphanJobDirs(data, map[string]struct{}{running: {}}, slog.Default())

	if _, err := os.Stat(filepath.Join(data, "jobs", running)); err != nil {
		t.Errorf("running job dir must be preserved: %v", err)
	}
	for _, orphan := range []string{orphanA, orphanB} {
		if _, err := os.Stat(filepath.Join(data, "jobs", orphan)); !os.IsNotExist(err) {
			t.Errorf("orphan job dir %q should have been swept, stat err = %v", orphan, err)
		}
	}
}

// TestCleanOrphanJobDirs_MissingJobsDirNoop ensures a fresh data dir (no jobs/
// yet) doesn't error.
func TestCleanOrphanJobDirs_MissingJobsDirNoop(t *testing.T) {
	CleanOrphanJobDirs(t.TempDir(), nil, slog.Default())
}

// TestCleanOrphanJobDirs_IgnoresFiles ensures a stray file directly under jobs/
// (not a job dir) is left alone - the sweep only touches directories.
func TestCleanOrphanJobDirs_IgnoresFiles(t *testing.T) {
	data := t.TempDir()
	mkJobFile(t, filepath.Join(data, "jobs", "stray.txt"))
	CleanOrphanJobDirs(data, nil, slog.Default())
	if _, err := os.Stat(filepath.Join(data, "jobs", "stray.txt")); err != nil {
		t.Errorf("stray file under jobs/ should be left alone: %v", err)
	}
}
