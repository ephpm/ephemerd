package dind

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// extractTarEntry models one member in an in-memory tar archive.
type extractTarEntry struct {
	Name     string
	Typeflag byte
	Mode     int64
	Body     []byte
	Linkname string
}

func writeTar(t *testing.T, entries []extractTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Typeflag: e.Typeflag,
			Mode:     e.Mode,
			Linkname: e.Linkname,
		}
		if e.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.Name, err)
		}
		if e.Typeflag == tar.TypeReg && len(e.Body) > 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("Write(%q): %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	return buf.Bytes()
}

// pathTraversalCases returns tar archives that try to write outside the
// destination directory in various ways.
type traversalCase struct {
	name    string
	entries []extractTarEntry
}

func pathTraversalCases() []traversalCase {
	return []traversalCase{
		{
			name: "DotDotDirect",
			entries: []extractTarEntry{
				{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "DotDotDeep",
			entries: []extractTarEntry{
				{Name: "../../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "AbsolutePath",
			entries: []extractTarEntry{
				{Name: "/etc/passwd-evil", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "MidPathEscape",
			entries: []extractTarEntry{
				{Name: "good/../../bad.txt", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "DirEscape",
			entries: []extractTarEntry{
				{Name: "../escape-dir", Typeflag: tar.TypeDir, Mode: 0o755},
			},
		},
	}
}

func TestExtractTar_PathTraversalRejected(t *testing.T) {
	for _, tc := range pathTraversalCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Build dst inside a parent that we then check for escape.
			parent := t.TempDir()
			dst := filepath.Join(parent, "dst")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			tarBytes := writeTar(t, tc.entries)
			// extractTar silently skips traversal entries (returns nil).
			if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
				t.Fatalf("extractTar: unexpected error %v", err)
			}

			// The parent of dst must not contain any of the would-be escape
			// files. Walk the parent tree and assert nothing was written
			// outside dst.
			if err := filepath.WalkDir(parent, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(parent, path)
				// Only "dst" and its descendants are allowed.
				if path == parent {
					return nil
				}
				if rel == "dst" || strings.HasPrefix(rel, "dst"+string(os.PathSeparator)) {
					return nil
				}
				t.Errorf("file written outside dst: %s", rel)
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
		})
	}
}

func TestExtractTar_SymlinkOutsideDestRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows symlink creation requires elevated privileges and behaves
		// differently — skip on Windows.
		t.Skip("symlink creation not reliable without admin on Windows")
	}
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Symlink path outside dst. extractTar's traversal check rejects this
	// before the symlink is created.
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "../evil-link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	})

	if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	// No symlink should exist in parent.
	if _, err := os.Lstat(filepath.Join(parent, "evil-link")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("symlink outside dst was created (Lstat err = %v)", err)
	}
}

func TestExtractTar_SymlinkInsideDestAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation not reliable without admin on Windows")
	}
	dst := t.TempDir()

	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "real", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("hello")},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "real"},
	})

	if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "real" {
		t.Errorf("link target = %q, want %q", got, "real")
	}
}

func TestExtractTar_RegularFile(t *testing.T) {
	dst := t.TempDir()
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "ok.txt", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("contents")},
	})
	if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "ok.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "contents" {
		t.Errorf("body = %q", string(got))
	}
}

func TestExtractTar_DirectoryEntry(t *testing.T) {
	dst := t.TempDir()
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "d", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "d/inner", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("x")},
	})
	if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "d"))
	if err != nil {
		t.Fatalf("stat d: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("d is not a directory")
	}
	if _, err := os.Stat(filepath.Join(dst, "d", "inner")); err != nil {
		t.Errorf("d/inner missing: %v", err)
	}
}

func TestExtractTar_TruncatedTar(t *testing.T) {
	dst := t.TempDir()
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Body: bytes.Repeat([]byte("x"), 1024)},
	})
	if len(tarBytes) < 50 {
		t.Fatal("tar too short")
	}
	// Truncate after the header.
	err := extractTar(bytes.NewReader(tarBytes[:200]), dst)
	if err == nil {
		t.Fatal("expected error on truncated tar, got nil")
	}
}

func TestExtractTar_Empty(t *testing.T) {
	dst := t.TempDir()
	tarBytes := writeTar(t, nil)
	if err := extractTar(bytes.NewReader(tarBytes), dst); err != nil {
		t.Errorf("extractTar on empty tar: %v", err)
	}
}

// Sanity: io.Reader interface is satisfied by *bytes.Reader so we don't
// silently break the test setup.
var _ io.Reader = (*bytes.Reader)(nil)
