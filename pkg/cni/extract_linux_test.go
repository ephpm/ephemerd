//go:build linux

package cni

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is a simple description of a tar member used by the test
// helpers. Symlinks use Linkname; everything else uses Body.
type tarEntry struct {
	Name     string
	Typeflag byte
	Mode     int64
	Body     []byte
	Linkname string
}

// buildTar writes the given entries into a tar archive and returns the bytes.
func buildTar(t *testing.T, entries []tarEntry) []byte {
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

// gzipBytes wraps the input in gzip framing.
func gzipBytes(t *testing.T, in []byte) []byte {
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

// extract calls the package's extractTarGz which expects a gzip stream.
// Returns the destination root for assertions.
func extract(t *testing.T, archive []byte) (string, error) {
	t.Helper()
	dest := t.TempDir()
	err := extractTarGz(bytes.NewReader(archive), dest)
	return dest, err
}

func TestExtractTarGz_Basic(t *testing.T) {
	tarBytes := buildTar(t, []tarEntry{
		{Name: "bridge", Typeflag: tar.TypeReg, Mode: 0o755, Body: []byte("plugin-bridge\n")},
		{Name: "host-local", Typeflag: tar.TypeReg, Mode: 0o755, Body: []byte("plugin-host-local\n")},
	})
	dest, err := extract(t, gzipBytes(t, tarBytes))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	for _, name := range []string{"bridge", "host-local"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.HasPrefix(string(got), "plugin-") {
			t.Errorf("%s: body = %q, want plugin-*", name, string(got))
		}
	}
}

func TestExtractTarGz_DirectoryEntry(t *testing.T) {
	tarBytes := buildTar(t, []tarEntry{
		{Name: "subdir", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "subdir/inner", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("nested")},
	})
	dest, err := extract(t, gzipBytes(t, tarBytes))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "subdir"))
	if err != nil {
		t.Fatalf("stat subdir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("subdir is not a directory")
	}

	body, err := os.ReadFile(filepath.Join(dest, "subdir", "inner"))
	if err != nil {
		t.Fatalf("read subdir/inner: %v", err)
	}
	if string(body) != "nested" {
		t.Errorf("body = %q, want %q", string(body), "nested")
	}
}

func TestExtractTarGz_FallThrough_NoGzipMagic(t *testing.T) {
	// A plain (un-gzipped) tar should fail extractTarGz because the function
	// always wraps the reader in a gzip.NewReader. This test pins the current
	// behavior — extractTarGz REQUIRES gzip. (The fall-through-to-plain-tar
	// behavior lives in extractBuildContext, not here.)
	tarBytes := buildTar(t, []tarEntry{
		{Name: "x", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("plain")},
	})
	_, err := extract(t, tarBytes)
	if err == nil {
		t.Fatal("expected error on un-gzipped tar, got nil")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("err = %v, want gzip-related error", err)
	}
}

func TestExtractTarGz_GzipMagicDetection(t *testing.T) {
	// A valid empty gzip stream should succeed (no entries, no error).
	emptyTar := buildTar(t, nil)
	gzipped := gzipBytes(t, emptyTar)
	if len(gzipped) < 2 || gzipped[0] != 0x1f || gzipped[1] != 0x8b {
		t.Fatalf("gzip should start with 1f 8b, got %x %x", gzipped[0], gzipped[1])
	}
	if _, err := extract(t, gzipped); err != nil {
		t.Errorf("extractTarGz on empty gzip: %v", err)
	}
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name: "DotDotEscape",
			entries: []tarEntry{
				{Name: "../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "AbsolutePath",
			entries: []tarEntry{
				{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
		{
			name: "NestedDotDot",
			entries: []tarEntry{
				{Name: "good", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "good/../../bad", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("evil")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, err := extract(t, gzipBytes(t, buildTar(t, tc.entries)))
			if err == nil {
				t.Fatal("expected error for path traversal, got nil")
			}
			if !strings.Contains(err.Error(), "invalid path") {
				t.Errorf("err = %v, want 'invalid path' error", err)
			}
			// Ensure no escape file was created on the host.
			parent := filepath.Dir(dest)
			if _, statErr := os.Stat(filepath.Join(parent, "etc", "passwd")); statErr == nil {
				t.Errorf("traversal succeeded: file written outside dest")
			}
			if _, statErr := os.Stat(filepath.Join(parent, "bad")); statErr == nil {
				t.Errorf("traversal succeeded: nested escape wrote a file")
			}
		})
	}
}

func TestExtractTarGz_CorruptGzip(t *testing.T) {
	// Bytes that look like a gzip header but are otherwise corrupt.
	corrupt := []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef, 0xff, 0xff}
	dest := t.TempDir()
	err := extractTarGz(bytes.NewReader(corrupt), dest)
	if err == nil {
		t.Fatal("expected error on corrupt gzip, got nil")
	}
	if !strings.Contains(err.Error(), "gzip") &&
		!strings.Contains(err.Error(), "tar") {
		t.Errorf("err = %v, want gzip- or tar-related error", err)
	}
}

func TestExtractTarGz_TruncatedTar(t *testing.T) {
	// Truncate the tar header bytes within the gzip stream.
	tarBytes := buildTar(t, []tarEntry{
		{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Body: bytes.Repeat([]byte("x"), 4096)},
	})
	if len(tarBytes) < 100 {
		t.Fatalf("tar too short to truncate")
	}
	gz := gzipBytes(t, tarBytes[:len(tarBytes)-200])
	dest := t.TempDir()
	err := extractTarGz(bytes.NewReader(gz), dest)
	if err == nil {
		t.Fatal("expected error on truncated tar, got nil")
	}
}

func TestExtractTarGz_Symlink(t *testing.T) {
	// Symlink within the destination is allowed; the target string itself
	// is not interpreted at extraction time.
	tarBytes := buildTar(t, []tarEntry{
		{Name: "real", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("realbody")},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "real"},
	})
	dest, err := extract(t, gzipBytes(t, tarBytes))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "real" {
		t.Errorf("link target = %q, want %q", got, "real")
	}
}

func TestExtractTarGz_RejectsTarHeaderBeforeGzip(t *testing.T) {
	// A non-gzip stream should always be rejected — no fall-through.
	r := bytes.NewReader([]byte("not gzip not tar"))
	dest := t.TempDir()
	err := extractTarGz(r, dest)
	if err == nil {
		t.Fatal("expected error on non-gzip bytes, got nil")
	}
}

func TestExtractTarGz_ReadError(t *testing.T) {
	// A reader that returns a non-EOF error should propagate it.
	dest := t.TempDir()
	err := extractTarGz(errReader{err: errors.New("boom")}, dest)
	if err == nil {
		t.Fatal("expected error from underlying reader, got nil")
	}
}

type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

// Sanity: the dest dir must remain on disk for assertions.
func TestExtractTarGz_DestExistsAfterExtract(t *testing.T) {
	dest, err := extract(t, gzipBytes(t, buildTar(t, []tarEntry{
		{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("a")},
	})))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("dest is not a directory")
	}
}

// extractTarGz silently ignores types it doesn't understand (block/char
// devices, FIFOs, hardlinks) — make sure such entries don't fail extraction.
func TestExtractTarGz_UnknownTypesIgnored(t *testing.T) {
	tarBytes := buildTar(t, []tarEntry{
		{Name: "real", Typeflag: tar.TypeReg, Mode: 0o644, Body: []byte("data")},
		{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
		{Name: "char", Typeflag: tar.TypeChar, Mode: 0o644},
	})
	dest, err := extract(t, gzipBytes(t, tarBytes))
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "real")); err != nil {
		t.Errorf("real should exist: %v", err)
	}
	// fifo and char should NOT have been created.
	if _, err := os.Stat(filepath.Join(dest, "fifo")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("fifo should not exist, stat err = %v", err)
	}
}

// Compile-time guard so io.Reader and the helper types stay in scope even
// if a test referencing them is removed.
var _ io.Reader = (*bytes.Reader)(nil)
