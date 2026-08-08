package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	tests := []struct {
		version, goos, goarch string
		want                  string
	}{
		{"v0.1.7", "linux", "amd64", "ephemerd_v0.1.7_linux_amd64.tar.gz"},
		{"v0.1.7", "linux", "arm64", "ephemerd_v0.1.7_linux_arm64.tar.gz"},
		{"v0.1.7", "darwin", "arm64", "ephemerd_v0.1.7_darwin_arm64.tar.gz"},
		{"v0.1.7", "windows", "amd64", "ephemerd_v0.1.7_windows_amd64.zip"},
		// unnormalized version gets a leading v
		{"0.1.7", "windows", "amd64", "ephemerd_v0.1.7_windows_amd64.zip"},
	}
	for _, tt := range tests {
		if got := AssetName(tt.version, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("AssetName(%q,%q,%q) = %q, want %q", tt.version, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestBaseURL(t *testing.T) {
	if got := baseURL("v0.1.7", ""); got != "https://github.com/ephpm/ephemerd/releases/download/v0.1.7" {
		t.Errorf("default baseURL = %q", got)
	}
	if got := baseURL("v0.1.7", "http://mirror.local/dl/"); got != "http://mirror.local/dl" {
		t.Errorf("override baseURL = %q (trailing slash not trimmed?)", got)
	}
}

func TestNormalizeAndValidVersion(t *testing.T) {
	tests := []struct {
		in    string
		norm  string
		valid bool
	}{
		{"v0.1.7", "v0.1.7", true},
		{"0.1.7", "v0.1.7", true},
		{"  v0.1.7 ", "v0.1.7", true},
		{"v0.0.0-rc1", "v0.0.0-rc1", true},
		{"v1.2", "v1.2", false},
		{"dev", "vdev", false},
		{"", "", false},
		{"latest", "vlatest", false},
	}
	for _, tt := range tests {
		if got := NormalizeVersion(tt.in); got != tt.norm {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", tt.in, got, tt.norm)
		}
		if got := ValidVersion(tt.in); got != tt.valid {
			t.Errorf("ValidVersion(%q) = %v, want %v", tt.in, got, tt.valid)
		}
	}
}

func TestSameVersion(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v0.1.7", "v0.1.7", true},
		{"0.1.7", "v0.1.7", true},
		{"v0.1.7", "v0.1.6", false},
		{"dev", "v0.1.7", false}, // an unstamped build is never "up to date"
		{"", "v0.1.7", false},
		{"v0.1.7", "", false},
	}
	for _, tt := range tests {
		if got := SameVersion(tt.a, tt.b); got != tt.want {
			t.Errorf("SameVersion(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	in := `dca8e8f36903b08a9102fe89661fd687e5dbb452f10dc87123a099a2d1bf94e7  ephemerd_v0.1.6_darwin_arm64.tar.gz
4baf43a22416d27bbb5c23db309f6c7dd0d5a0b06b8ed07c20ae07d5778a4345  ephemerd_v0.1.6_linux_amd64.tar.gz

e3252a7833ac6088aaa9d2f5d917fc8b9c228579b96b76c8ba48a9db29eb8ffa  *ephemerd_v0.1.6_windows_amd64.zip
garbage line with too many fields here nope
`
	sums, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if got := sums["ephemerd_v0.1.6_linux_amd64.tar.gz"]; got != "4baf43a22416d27bbb5c23db309f6c7dd0d5a0b06b8ed07c20ae07d5778a4345" {
		t.Errorf("linux amd64 sum = %q", got)
	}
	// binary-mode '*' prefix on the filename is tolerated
	if got := sums["ephemerd_v0.1.6_windows_amd64.zip"]; got != "e3252a7833ac6088aaa9d2f5d917fc8b9c228579b96b76c8ba48a9db29eb8ffa" {
		t.Errorf("windows sum = %q (star-prefixed name not handled?)", got)
	}
	if len(sums) != 3 {
		t.Errorf("parsed %d entries, want 3 (blank + malformed lines should be skipped)", len(sums))
	}
}

func TestParseChecksums_EmptyIsError(t *testing.T) {
	if _, err := ParseChecksums(strings.NewReader("\n   \n")); err == nil {
		t.Fatal("expected error for checksums with no entries")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.bin")
	content := []byte("the quick brown fox")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	good := hex.EncodeToString(sum[:])

	if err := verifyChecksum(path, good); err != nil {
		t.Errorf("verifyChecksum matching: %v", err)
	}
	// case-insensitive
	if err := verifyChecksum(path, strings.ToUpper(good)); err != nil {
		t.Errorf("verifyChecksum uppercase: %v", err)
	}
	if err := verifyChecksum(path, "deadbeef"); err == nil {
		t.Error("verifyChecksum mismatch: expected error")
	}
}

func TestParseVersionOutput(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"ephemerd version v0.1.7", "v0.1.7"},
		{"ephemerd version v0.1.7\n", "v0.1.7"},
		{"v0.1.7", "v0.1.7"},
		{"weird 0.1.7 format", "v0.1.7"},
	}
	for _, tt := range tests {
		if got := parseVersionOutput(tt.in); got != tt.want {
			t.Errorf("parseVersionOutput(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- archive extraction ---

func makeTarGz(t *testing.T, entryName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: entryName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entryName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary_TarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	content := []byte("linux-binary-bytes")
	if err := os.WriteFile(archive, makeTarGz(t, "ephemerd", content), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractBinary(archive, "linux", dest); err != nil {
		t.Fatalf("extractBinary tar.gz: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content) {
		t.Errorf("extracted content = %q, want %q", got, content)
	}
}

func TestExtractBinary_Zip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	content := []byte("windows-binary-bytes")
	if err := os.WriteFile(archive, makeZip(t, "ephemerd.exe", content), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.exe")
	if err := extractBinary(archive, "windows", dest); err != nil {
		t.Fatalf("extractBinary zip: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content) {
		t.Errorf("extracted content = %q, want %q", got, content)
	}
}

func TestExtractBinary_MissingEntry(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, "something-else", []byte("x")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(archive, "linux", filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected error when archive lacks the ephemerd entry")
	}
}
