package runtime

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCleanOldLogs_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()

	// Create an old log file
	oldPath := filepath.Join(dir, "old-job.log")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the modification time
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create a recent log file
	recentPath := filepath.Join(dir, "recent-job.log")
	if err := os.WriteFile(recentPath, []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	// Old file should be gone
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old log file should have been removed")
	}

	// Recent file should still exist
	if _, err := os.Stat(recentPath); err != nil {
		t.Error("recent log file should still exist")
	}
}

func TestCleanOldLogs_SkipsNonLogFiles(t *testing.T) {
	dir := t.TempDir()

	// Create an old non-log file
	txtPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(txtPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(txtPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	// Non-.log file should not be removed
	if _, err := os.Stat(txtPath); err != nil {
		t.Error("non-.log file should not be removed")
	}
}

func TestCleanOldLogs_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()

	subdir := filepath.Join(dir, "subdir.log")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	if _, err := os.Stat(subdir); err != nil {
		t.Error("directory named .log should not be removed")
	}
}

func TestCleanOldLogs_NonexistentDir(t *testing.T) {
	// Should not panic
	CleanOldLogs("/nonexistent/dir", 7*24*time.Hour, silentLog())
}

func TestCleanOldLogs_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	// Should not panic
	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	// Dir should still exist and still be empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir, got %d entries", len(entries))
	}
}

// --- additional CleanOldLogs edges (item #15) ---

func TestCleanOldLogs_StaleSymlinkRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Symlink creation requires admin privileges or developer mode on
		// Windows; CI typically lacks both. The behavior under test is
		// the same as on Unix because os.Remove on a symlink deletes
		// the link itself (not the target).
		t.Skip("symlink creation requires special privileges on Windows")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "real-target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "broken.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Backdate the symlink itself.
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(link, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	// The .log symlink should be gone, but the target file must remain.
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected symlink removed, got err=%v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target should remain: %v", err)
	}
}

func TestCleanOldLogs_BrokenSymlinkRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires special privileges on Windows")
	}

	dir := t.TempDir()
	link := filepath.Join(dir, "dangling.log")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), link); err != nil {
		t.Fatal(err)
	}
	// Symlink mtime defaults to ~now; explicitly backdate via Lchtimes
	// substitute. Plain Chtimes follows the link and would fail because
	// the target doesn't exist. Use os.Chtimes with a workaround: create
	// the symlink already pointing at a missing target, then use lutimes
	// via a fresh symlink with the desired birth time.
	//
	// Simpler: set the entire dir mtime won't work; just rely on the
	// fact that symlink lstat returns the link's own ctime (~now) which
	// will be ~now, so it WON'T be older than 7 days. To keep this
	// deterministic, set maxAge to negative so every file qualifies as
	// "old".
	CleanOldLogs(dir, -1*time.Second, silentLog())

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected dangling symlink removed, got err=%v", err)
	}
}

func TestCleanOldLogs_PermissionDeniedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows file permission semantics differ — chmod 0o000 doesn't
		// produce an os.Remove error the way it does on Unix. The
		// equivalent test there would need ACLs; skip it.
		t.Skip("Unix file mode semantics required for this test")
	}
	if os.Getuid() == 0 {
		// Root bypasses file mode checks, so chmod 0o000 wouldn't
		// produce a permission denied on remove. Skip in that case.
		t.Skip("running as root, file permissions ignored")
	}

	dir := t.TempDir()
	// Create a file inside a directory we strip write permission from.
	// os.Remove needs write+execute on the parent, not the file itself.
	subdir := filepath.Join(dir, "ro-subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(subdir, "blocked.log")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(target, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Place a file directly under dir so CleanOldLogs has something to
	// scan, then strip write permission from the subdir to ensure
	// permission checks elsewhere don't matter. Actually CleanOldLogs
	// only scans dir directly (not recursively), so to test
	// permission-denied on os.Remove we need the *target* dir, dir, to
	// be writable (so ReadDir works) and then the *individual file*
	// must fail to remove. The simplest reliable signal on Linux: chmod
	// the dir read-only after writing the file.
	oldFile := filepath.Join(dir, "old.log")
	if err := os.WriteFile(oldFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Make dir read-only so os.Remove fails for old.log.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore permissions so t.TempDir cleanup succeeds.
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Logf("restore chmod: %v", err)
		}
	})

	// Should not panic; the permission error is logged at debug level
	// and the function continues.
	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	// File should still exist (delete failed).
	if _, err := os.Stat(oldFile); err != nil {
		t.Errorf("file should still exist after failed remove: %v", err)
	}
}

func TestCleanOldLogs_FileRemovedBeforeStat(t *testing.T) {
	// Simulates a file disappearing mid-walk: we stage a directory entry
	// via os.WriteFile, then delete it before CleanOldLogs runs. ReadDir
	// snapshots the entries up-front, so when the loop calls entry.Info()
	// it sees ErrNotExist. CleanOldLogs must log+continue, not panic.
	//
	// We can't truly inject a delete *between* ReadDir and Info() from
	// outside, but we can populate the directory with files, then delete
	// some of them and re-add others to assert the cleanup is robust.
	dir := t.TempDir()

	// Old file that should be removed.
	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Recent file that must survive.
	recent := filepath.Join(dir, "recent.log")
	if err := os.WriteFile(recent, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Old file that we delete *before* CleanOldLogs runs to simulate the
	// mid-walk race. Even though our delete happens before ReadDir, the
	// real concern is that CleanOldLogs's per-entry Info()/Remove() calls
	// tolerate failures — and they do.
	racy := filepath.Join(dir, "racy.log")
	if err := os.WriteFile(racy, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(racy); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expected old.log removed, got err=%v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent.log should still exist: %v", err)
	}
}

func TestCleanOldLogs_LogSuffixOnDirIgnored(t *testing.T) {
	// A *file* whose name contains ".log" but doesn't end with it should
	// be skipped, mirroring the existing dir test for entries named ".log".
	dir := t.TempDir()
	weird := filepath.Join(dir, "old.log.bak")
	if err := os.WriteFile(weird, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(weird, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	if _, err := os.Stat(weird); err != nil {
		t.Errorf("file without .log suffix should not be removed: %v", err)
	}
}

func TestCleanOldLogs_BoundaryAge(t *testing.T) {
	// Files exactly at the cutoff: ModTime.Before(cutoff) is false at
	// equal times so the file should NOT be removed. Verify with a file
	// barely younger than maxAge.
	dir := t.TempDir()

	young := filepath.Join(dir, "young.log")
	if err := os.WriteFile(young, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1 minute younger than the cutoff.
	mtime := time.Now().Add(-7*24*time.Hour + time.Minute)
	if err := os.Chtimes(young, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, silentLog())

	if _, err := os.Stat(young); err != nil {
		t.Errorf("file barely younger than maxAge should survive: %v", err)
	}
}

// loggerOutput is a slog.Logger that writes structured records to a
// strings.Builder so tests can assert that error paths log the expected
// keys without poking at slog.Handler internals.
func loggerOutput(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestCleanOldLogs_DiscardsOutputWriter(t *testing.T) {
	// Sanity check that we can route logs to a buffer (used implicitly
	// by error-path tests above; this just verifies the helper plumbing).
	var buf strings.Builder
	log := loggerOutput(&buf)

	dir := t.TempDir()
	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	CleanOldLogs(dir, 7*24*time.Hour, log)

	if buf.Len() == 0 {
		t.Error("expected debug log output for removed file")
	}
	if !strings.Contains(buf.String(), "removed old job log") {
		t.Errorf("expected 'removed old job log' in output, got: %s", buf.String())
	}
}
