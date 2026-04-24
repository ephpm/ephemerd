//go:build linux

package dind

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gzipWrap returns a gzipped copy of in.
func gzipWrap(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(in); err != nil {
		t.Fatalf("gz.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBuildContext_PlainTar(t *testing.T) {
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "Dockerfile", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("FROM scratch\n")},
		{Name: "src/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("package main\n")},
	})

	dst := t.TempDir()
	if err := extractBuildContext(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractBuildContext: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dst, "Dockerfile")); err != nil {
		t.Errorf("Dockerfile missing: %v", err)
	} else if !strings.HasPrefix(string(got), "FROM scratch") {
		t.Errorf("Dockerfile = %q", string(got))
	}

	if _, err := os.Stat(filepath.Join(dst, "src", "main.go")); err != nil {
		t.Errorf("src/main.go missing: %v", err)
	}
}

func TestExtractBuildContext_GzipTar(t *testing.T) {
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "Dockerfile", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("FROM alpine\n")},
	})
	gzipped := gzipWrap(t, tarBytes)

	if gzipped[0] != 0x1f || gzipped[1] != 0x8b {
		t.Fatalf("gzip should start with 1f 8b, got %x %x", gzipped[0], gzipped[1])
	}

	dst := t.TempDir()
	if err := extractBuildContext(bytes.NewReader(gzipped), dst); err != nil {
		t.Fatalf("extractBuildContext: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.HasPrefix(string(got), "FROM alpine") {
		t.Errorf("Dockerfile = %q", string(got))
	}
}

func TestExtractBuildContext_PathTraversalSilentlyDropped(t *testing.T) {
	// extractBuildContext silently drops traversal entries (returns nil),
	// matching extractTar's behavior.
	for _, tc := range pathTraversalCases() {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dst := filepath.Join(parent, "ctx")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			tarBytes := writeTar(t, tc.entries)
			if err := extractBuildContext(bytes.NewReader(tarBytes), dst); err != nil {
				t.Fatalf("extractBuildContext: %v", err)
			}

			// Walk parent and assert nothing leaked outside dst.
			if err := filepath.WalkDir(parent, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(parent, path)
				if path == parent {
					return nil
				}
				if rel == "ctx" || strings.HasPrefix(rel, "ctx"+string(os.PathSeparator)) {
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

func TestExtractBuildContext_PathTraversalGzipped(t *testing.T) {
	// Same traversal cases but inside a gzip wrapper — make sure the gzip
	// branch ALSO rejects escapes.
	parent := t.TempDir()
	dst := filepath.Join(parent, "ctx")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "../../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
		{Name: "Dockerfile", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("ok")},
	})
	gzipped := gzipWrap(t, tarBytes)

	if err := extractBuildContext(bytes.NewReader(gzipped), dst); err != nil {
		t.Fatalf("extractBuildContext: %v", err)
	}

	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("escape.txt should not exist: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "Dockerfile")); err != nil {
		t.Errorf("Dockerfile missing from dst: %v", err)
	}
}

func TestExtractBuildContext_CorruptGzip(t *testing.T) {
	// 0x1f 0x8b is the gzip magic; following bytes are nonsense.
	corrupt := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff}
	dst := t.TempDir()
	err := extractBuildContext(bytes.NewReader(corrupt), dst)
	if err == nil {
		t.Fatal("expected error on corrupt gzip, got nil")
	}
}

func TestExtractBuildContext_Empty(t *testing.T) {
	// Empty body — no error, no entries.
	dst := t.TempDir()
	err := extractBuildContext(bytes.NewReader(nil), dst)
	if err != nil {
		t.Errorf("extractBuildContext on empty body: %v", err)
	}
}

func TestExtractBuildContext_SingleByte(t *testing.T) {
	// Body shorter than 2 bytes: header read returns ErrUnexpectedEOF.
	// extractBuildContext tolerates this and treats as plain tar.
	dst := t.TempDir()
	err := extractBuildContext(bytes.NewReader([]byte{0x00}), dst)
	// Either no error (treated as empty plain tar) or a tar parse error
	// is acceptable — what we really care about is no panic and no escape.
	_ = err
}

func TestExtractBuildContext_DirectoryEntry(t *testing.T) {
	tarBytes := writeTar(t, []extractTarEntry{
		{Name: "subdir", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "subdir/file", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("body")},
	})
	dst := t.TempDir()
	if err := extractBuildContext(bytes.NewReader(tarBytes), dst); err != nil {
		t.Fatalf("extractBuildContext: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "subdir"))
	if err != nil || !info.IsDir() {
		t.Errorf("subdir not a directory: info=%v err=%v", info, err)
	}
}
