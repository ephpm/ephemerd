package runtime

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
}
