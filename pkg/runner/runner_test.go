package runner

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- New / Dir / Entrypoint / ContainerDir ---

func TestNew(t *testing.T) {
	m := New("/data", testLogger())
	if m == nil {
		t.Fatal("New() returned nil")
	}
	if m.dataDir != "/data" {
		t.Errorf("dataDir = %q, want %q", m.dataDir, "/data")
	}
}

func TestDir(t *testing.T) {
	m := New("/data", testLogger())
	dir := m.Dir()
	if dir == "" {
		t.Fatal("Dir() returned empty string")
	}
	if !strings.Contains(dir, "runners") {
		t.Errorf("Dir() = %q, expected to contain 'runners'", dir)
	}
	if !strings.Contains(dir, Version) {
		t.Errorf("Dir() = %q, expected to contain version %q", dir, Version)
	}
}

func TestEntrypoint(t *testing.T) {
	m := New("/data", testLogger())
	ep := m.Entrypoint()
	if ep == "" {
		t.Fatal("Entrypoint() returned empty string")
	}

	switch goruntime.GOOS {
	case "windows":
		if !strings.HasSuffix(ep, "run.cmd") {
			t.Errorf("Entrypoint() = %q, expected to end with run.cmd on Windows", ep)
		}
	default:
		if !strings.HasSuffix(ep, "run.sh") {
			t.Errorf("Entrypoint() = %q, expected to end with run.sh", ep)
		}
	}
}

func TestContainerDir(t *testing.T) {
	m := New("/data", testLogger())
	cd := m.ContainerDir()
	if cd == "" {
		t.Fatal("ContainerDir() returned empty string")
	}

	switch goruntime.GOOS {
	case "windows":
		if cd != `C:\actions-runner` {
			t.Errorf("ContainerDir() = %q, want %q", cd, `C:\actions-runner`)
		}
	default:
		if cd != "/actions-runner" {
			t.Errorf("ContainerDir() = %q, want %q", cd, "/actions-runner")
		}
	}
}

// --- Extract caching ---

func TestExtract_CachesWithMarker(t *testing.T) {
	tmp := t.TempDir()
	m := New(tmp, testLogger())

	dir := m.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".extracted"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Extract(); err != nil {
		t.Errorf("Extract() with marker should be no-op, got error: %v", err)
	}
}

// --- extractTarGz tests ---

func createTestTarGz(t *testing.T, files map[string]string) *os.File {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}

	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// writeTarGz is a helper that creates a tar.gz file with the given entries,
// closes the writers, seeks to the beginning, and returns the file.
// Each entry is a tar header + optional content bytes.
type tarEntry struct {
	header  *tar.Header
	content []byte
}

func writeTarGz(t *testing.T, entries []tarEntry) *os.File {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}

	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)

	for _, e := range entries {
		if err := tw.WriteHeader(e.header); err != nil {
			t.Fatal(err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestExtractTarGz_BasicFiles(t *testing.T) {
	dest := t.TempDir()
	f := createTestTarGz(t, map[string]string{
		"file1.txt": "hello",
		"file2.txt": "world",
	})
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	if err := extractTarGz(f, dest); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "file1.txt"))
	if err != nil {
		t.Fatalf("reading file1.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file1.txt = %q, want %q", string(data), "hello")
	}

	data, err = os.ReadFile(filepath.Join(dest, "file2.txt"))
	if err != nil {
		t.Fatalf("reading file2.txt: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("file2.txt = %q, want %q", string(data), "world")
	}
}

func TestExtractTarGz_NestedDirs(t *testing.T) {
	dest := t.TempDir()
	content := "nested content"

	f := writeTarGz(t, []tarEntry{
		{header: &tar.Header{Name: "subdir/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{header: &tar.Header{Name: "subdir/nested.txt", Mode: 0o644, Size: int64(len(content))}, content: []byte(content)},
	})
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	if err := extractTarGz(f, dest); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	if err != nil {
		t.Fatalf("reading nested.txt: %v", err)
	}
	if string(data) != content {
		t.Errorf("nested.txt = %q, want %q", string(data), content)
	}
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	dest := t.TempDir()

	f := writeTarGz(t, []tarEntry{
		{header: &tar.Header{Name: "../../../etc/passwd", Mode: 0o644, Size: 5}, content: []byte("pwned")},
	})
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	err := extractTarGz(f, dest)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("expected 'invalid path' error, got: %v", err)
	}
}

func TestExtractTarGz_EmptyArchive(t *testing.T) {
	dest := t.TempDir()

	f := writeTarGz(t, nil)
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	if err := extractTarGz(f, dest); err != nil {
		t.Errorf("extractTarGz() empty archive error: %v", err)
	}
}

// --- extractZipFromReader tests ---

func writeTestZip(t *testing.T, files map[string]string) *os.File {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "test-*.zip")
	if err != nil {
		t.Fatal(err)
	}

	zw := zip.NewWriter(tmp)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestExtractZipFromReader_BasicFile(t *testing.T) {
	dest := t.TempDir()

	f := writeTestZip(t, map[string]string{"hello.txt": "hello zip"})
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	if err := extractZipFromReader(f, dest); err != nil {
		t.Fatalf("extractZipFromReader() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("reading hello.txt: %v", err)
	}
	if string(data) != "hello zip" {
		t.Errorf("hello.txt = %q, want %q", string(data), "hello zip")
	}
}

func TestExtractZipFromReader_SafeFile(t *testing.T) {
	dest := t.TempDir()

	f := writeTestZip(t, map[string]string{"safe.txt": "safe"})
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("closing test file: %v", err)
		}
	}()

	if err := extractZipFromReader(f, dest); err != nil {
		t.Fatalf("extractZipFromReader() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "safe.txt")); err != nil {
		t.Errorf("safe.txt not extracted: %v", err)
	}
}

// --- findTarball tests ---

func TestFindTarball(t *testing.T) {
	m := New("/data", testLogger())
	name, err := m.findTarball()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	if name == "" {
		t.Fatal("findTarball() returned empty string")
	}
	if !strings.HasPrefix(name, "embed/") {
		t.Errorf("findTarball() = %q, expected embed/ prefix", name)
	}
}

func TestFindTarball_MatchesPlatform(t *testing.T) {
	m := New("/data", testLogger())
	name, err := m.findTarball()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	switch goruntime.GOOS {
	case "windows":
		if !strings.Contains(name, "win") {
			t.Errorf("findTarball() = %q, expected 'win' on Windows", name)
		}
	case "darwin":
		if !strings.Contains(name, "osx") {
			t.Errorf("findTarball() = %q, expected 'osx' on macOS", name)
		}
	default:
		if !strings.Contains(name, "linux") {
			t.Errorf("findTarball() = %q, expected 'linux' on Linux", name)
		}
	}
}
