package artifacts

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- ArtifactsDir tests ---

func TestArtifactsDir(t *testing.T) {
	dir := ArtifactsDir("/var/lib/ephemerd", "12345")
	want := filepath.Join("/var/lib/ephemerd", "artifacts", "12345")
	if dir != want {
		t.Errorf("ArtifactsDir() = %q, want %q", dir, want)
	}
}

func TestArtifactsDir_DifferentJobs(t *testing.T) {
	dir1 := ArtifactsDir("/data", "100")
	dir2 := ArtifactsDir("/data", "200")
	if dir1 == dir2 {
		t.Error("different job IDs should produce different directories")
	}
}

// --- Cleanup tests ---

func TestCleanup_RemovesDir(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "artifacts", "job1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	Cleanup(dir, testLogger())

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected directory to be removed, got err: %v", err)
	}
}

func TestCleanup_EmptyString(t *testing.T) {
	// Should be a no-op, not panic
	Cleanup("", testLogger())
}

func TestCleanup_NonexistentDir(t *testing.T) {
	// Should not error on missing directory
	Cleanup("/nonexistent/path/that/does/not/exist", testLogger())
}

// --- ListContents tests ---

func TestListContents(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "file1.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "subdir", "file2.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := ListContents(tmp)
	if err != nil {
		t.Fatalf("ListContents() error: %v", err)
	}

	if len(files) != 3 {
		t.Errorf("expected 3 entries (file1.txt, subdir, subdir/file2.txt), got %d: %v", len(files), files)
	}

	// Check that entries are present (order may vary by OS)
	found := make(map[string]bool)
	for _, f := range files {
		found[filepath.ToSlash(f)] = true
	}
	for _, want := range []string{"file1.txt", "subdir", "subdir/file2.txt"} {
		if !found[want] {
			t.Errorf("expected %q in listing, got %v", want, files)
		}
	}
}

func TestListContents_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	files, err := ListContents(tmp)
	if err != nil {
		t.Fatalf("ListContents() error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d: %v", len(files), files)
	}
}

func TestListContents_NonexistentDir(t *testing.T) {
	_, err := ListContents("/nonexistent/dir")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

// --- NewExtractor ---

func TestNewExtractor_NilClient(t *testing.T) {
	// NewExtractor should not panic with nil client
	// (it will fail on Extract(), but construction is fine)
	e := NewExtractor(nil, testLogger())
	if e == nil {
		t.Fatal("NewExtractor() returned nil")
	}
}
