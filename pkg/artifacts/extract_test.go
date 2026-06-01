package artifacts

import (
	"archive/tar"
	"bytes"
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

func TestNewExtractor(t *testing.T) {
	e := NewExtractor(testLogger())
	if e == nil {
		t.Fatal("NewExtractor() returned nil")
	}
}

// --- applyTar tests ---

// buildTar creates an in-memory tar archive from the given entries.
func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing tar body for %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return &buf
}

type tarEntry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
}

func TestApplyTar_RegularFiles(t *testing.T) {
	dest := t.TempDir()

	entries := []tarEntry{
		{name: "hello.txt", body: "hello world", mode: 0o644, typeflag: tar.TypeReg},
		{name: "subdir/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "subdir/nested.txt", body: "nested content", mode: 0o644, typeflag: tar.TypeReg},
	}

	buf := buildTar(t, entries)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() error: %v", err)
	}

	// Verify hello.txt
	data, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("reading hello.txt: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("hello.txt content = %q, want %q", string(data), "hello world")
	}

	// Verify subdir/nested.txt
	data, err = os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("reading subdir/nested.txt: %v", err)
	}
	if string(data) != "nested content" {
		t.Errorf("subdir/nested.txt content = %q, want %q", string(data), "nested content")
	}
}

func TestApplyTar_Symlink(t *testing.T) {
	dest := t.TempDir()

	entries := []tarEntry{
		{name: "target.txt", body: "target", mode: 0o644, typeflag: tar.TypeReg},
		{name: "link.txt", typeflag: tar.TypeSymlink, linkname: "target.txt"},
	}

	buf := buildTar(t, entries)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() error: %v", err)
	}

	linkDest, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if linkDest != "target.txt" {
		t.Errorf("symlink target = %q, want %q", linkDest, "target.txt")
	}
}

func TestApplyTar_HardLink(t *testing.T) {
	dest := t.TempDir()

	entries := []tarEntry{
		{name: "original.txt", body: "shared content", mode: 0o644, typeflag: tar.TypeReg},
		{name: "hardlink.txt", typeflag: tar.TypeLink, linkname: "original.txt"},
	}

	buf := buildTar(t, entries)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() error: %v", err)
	}

	// Both files should have the same content.
	data1, err := os.ReadFile(filepath.Join(dest, "original.txt"))
	if err != nil {
		t.Fatalf("reading original.txt: %v", err)
	}
	data2, err := os.ReadFile(filepath.Join(dest, "hardlink.txt"))
	if err != nil {
		t.Fatalf("reading hardlink.txt: %v", err)
	}
	if string(data1) != string(data2) {
		t.Errorf("hard link content mismatch: %q vs %q", string(data1), string(data2))
	}
}

func TestApplyTar_Whiteout(t *testing.T) {
	dest := t.TempDir()

	// First, create a file that should be whited out.
	if err := os.WriteFile(filepath.Join(dest, "removeme.txt"), []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply a layer with a whiteout for removeme.txt.
	entries := []tarEntry{
		{name: ".wh.removeme.txt", typeflag: tar.TypeReg, mode: 0o644},
	}

	buf := buildTar(t, entries)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "removeme.txt")); !os.IsNotExist(err) {
		t.Error("expected removeme.txt to be deleted by whiteout")
	}
}

func TestApplyTar_DirectoryTraversal(t *testing.T) {
	dest := t.TempDir()

	// An entry with ../ should be skipped.
	entries := []tarEntry{
		{name: "../escape.txt", body: "bad", mode: 0o644, typeflag: tar.TypeReg},
		{name: "good.txt", body: "good", mode: 0o644, typeflag: tar.TypeReg},
	}

	buf := buildTar(t, entries)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() error: %v", err)
	}

	// The escape file should NOT exist.
	if _, err := os.Stat(filepath.Join(dest, "..", "escape.txt")); err == nil {
		t.Error("directory traversal entry should have been skipped")
	}

	// The good file should exist.
	if _, err := os.Stat(filepath.Join(dest, "good.txt")); err != nil {
		t.Error("good.txt should exist")
	}
}

func TestApplyTar_OverwriteFile(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: create a file.
	entries1 := []tarEntry{
		{name: "file.txt", body: "version 1", mode: 0o644, typeflag: tar.TypeReg},
	}
	buf1 := buildTar(t, entries1)
	if err := applyTar(buf1, dest); err != nil {
		t.Fatalf("applyTar() layer 1 error: %v", err)
	}

	// Layer 2: overwrite the same file.
	entries2 := []tarEntry{
		{name: "file.txt", body: "version 2", mode: 0o644, typeflag: tar.TypeReg},
	}
	buf2 := buildTar(t, entries2)
	if err := applyTar(buf2, dest); err != nil {
		t.Fatalf("applyTar() layer 2 error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil {
		t.Fatalf("reading file.txt: %v", err)
	}
	if string(data) != "version 2" {
		t.Errorf("file.txt content = %q, want %q", string(data), "version 2")
	}
}

func TestApplyTar_EmptyArchive(t *testing.T) {
	dest := t.TempDir()
	buf := buildTar(t, nil)
	if err := applyTar(buf, dest); err != nil {
		t.Fatalf("applyTar() with empty archive should succeed, got: %v", err)
	}
}
