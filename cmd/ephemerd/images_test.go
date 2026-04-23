package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func writeTarball(t *testing.T, path string, size int) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing tarball %s: %v", path, err)
	}
}

func TestCopyTarballs_EmptySrcIsNoop(t *testing.T) {
	if err := copyTarballs("", t.TempDir(), discardLogger()); err != nil {
		t.Fatalf("empty src should be no-op, got: %v", err)
	}
}

func TestCopyTarballs_MissingSrcReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := copyTarballs(missing, t.TempDir(), discardLogger()); err == nil {
		t.Fatal("expected error for missing src")
	}
}

func TestCopyTarballs_SrcIsFileReturnsError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyTarballs(src, t.TempDir(), discardLogger()); err == nil {
		t.Fatal("expected error when src is a file")
	}
}

func TestCopyTarballs_CopiesOnlyTarFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeTarball(t, filepath.Join(src, "linux.tar"), 64)
	writeTarball(t, filepath.Join(src, "windows.tar"), 128)
	// Non-tar files must be ignored.
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "rootfs.tar.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Subdirectory with a .tar inside must be ignored (non-recursive).
	subdir := filepath.Join(src, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTarball(t, filepath.Join(subdir, "nested.tar"), 32)

	if err := copyTarballs(src, dst, discardLogger()); err != nil {
		t.Fatalf("copyTarballs: %v", err)
	}

	copied, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	for _, e := range copied {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = info.Size()
	}
	want := map[string]int64{"linux.tar": 64, "windows.tar": 128}
	if len(got) != len(want) {
		t.Fatalf("dst contents = %v, want %v", got, want)
	}
	for name, size := range want {
		if got[name] != size {
			t.Errorf("%s: got size %d, want %d", name, got[name], size)
		}
	}
}

func TestCopyTarballs_SkipsSameSize(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcPath := filepath.Join(src, "big.tar")
	dstPath := filepath.Join(dst, "big.tar")
	writeTarball(t, srcPath, 1024)
	// Pre-populate dst with a file of the same size but different bytes —
	// the copy should be skipped because sizes match.
	marker := make([]byte, 1024)
	for i := range marker {
		marker[i] = 0xAA
	}
	if err := os.WriteFile(dstPath, marker, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyTarballs(src, dst, discardLogger()); err != nil {
		t.Fatalf("copyTarballs: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0xAA {
		t.Error("dst was overwritten despite same size — skip logic did not trigger")
	}
}

func TestCopyTarballs_ReplacesDifferentSize(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	srcPath := filepath.Join(src, "img.tar")
	dstPath := filepath.Join(dst, "img.tar")
	writeTarball(t, srcPath, 2048)
	if err := os.WriteFile(dstPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyTarballs(src, dst, discardLogger()); err != nil {
		t.Fatalf("copyTarballs: %v", err)
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2048 {
		t.Errorf("dst size = %d, want 2048 (should have been replaced)", info.Size())
	}
}

func TestCopyTarballs_CreatesDstDir(t *testing.T) {
	src := t.TempDir()
	writeTarball(t, filepath.Join(src, "only.tar"), 16)

	base := t.TempDir()
	dst := filepath.Join(base, "data", "images") // does not exist

	if err := copyTarballs(src, dst, discardLogger()); err != nil {
		t.Fatalf("copyTarballs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "only.tar")); err != nil {
		t.Errorf("expected only.tar in dst, got %v", err)
	}
}
