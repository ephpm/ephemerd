package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// makeTestTarball writes a minimal Docker-format OCI tarball at path with the
// given OS and tag. Returns the digest.
func makeTestTarball(t *testing.T, path, osName, tag string) v1.Hash {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           osName,
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ref, err := name.NewTag(tag)
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := tarball.WriteToFile(path, ref, img); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d
}

func TestTarballImageOS_Linux(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "linux.tar")
	makeTestTarball(t, p, "linux", "myrepo/test:linux")

	got, err := tarballImageOS(p)
	if err != nil {
		t.Fatalf("tarballImageOS: %v", err)
	}
	if got != "linux" {
		t.Errorf("OS = %q, want linux", got)
	}
}

func TestTarballImageOS_Windows(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "windows.tar")
	makeTestTarball(t, p, "windows", "myrepo/test:windows")

	got, err := tarballImageOS(p)
	if err != nil {
		t.Fatalf("tarballImageOS: %v", err)
	}
	if got != "windows" {
		t.Errorf("OS = %q, want windows", got)
	}
}

func TestTarballImageOS_NonExistent(t *testing.T) {
	_, err := tarballImageOS(filepath.Join(t.TempDir(), "nope.tar"))
	if err == nil {
		t.Fatal("expected error for non-existent tarball")
	}
}

func TestTarballImageOS_NotATarball(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "garbage.tar")
	if err := os.WriteFile(p, []byte("this is not an OCI tarball"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := tarballImageOS(p)
	if err == nil {
		t.Fatal("expected error for non-tarball file")
	}
}

func TestTarballImageRef_ReturnsTag(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tagged.tar")
	makeTestTarball(t, p, "linux", "myrepo/test:v1.2.3")

	ref, err := tarballImageRef(p)
	if err != nil {
		t.Fatalf("tarballImageRef: %v", err)
	}
	// LoadManifest gives back the RepoTags entries verbatim.
	if ref == "" {
		t.Fatal("expected non-empty ref")
	}
	// The repo tag from a tarball will be the original tagged ref, possibly
	// with the docker.io/index normalization applied. Ensure it ends with
	// the test tag.
	want := "myrepo/test:v1.2.3"
	// Common formats: "index.docker.io/myrepo/test:v1.2.3" or "myrepo/test:v1.2.3"
	if ref != want && !endsWith(ref, want) {
		t.Errorf("ref = %q, want suffix %q", ref, want)
	}
}

func TestTarballImageRef_NonExistent(t *testing.T) {
	_, err := tarballImageRef(filepath.Join(t.TempDir(), "nope.tar"))
	if err == nil {
		t.Fatal("expected error for non-existent tarball")
	}
}

// TestImportImages_NoImagesDir verifies an empty/unset ImagesDir is a no-op.
func TestImportImages_NoImagesDir(t *testing.T) {
	rt := &Runtime{
		cfg: Config{
			ImagesDir: "",
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	deferred, err := rt.ImportImages(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want empty", deferred)
	}
}

// TestImportImages_MissingImagesDir verifies a non-existent ImagesDir is
// treated as a no-op (not an error).
func TestImportImages_MissingImagesDir(t *testing.T) {
	rt := &Runtime{
		cfg: Config{
			ImagesDir: filepath.Join(t.TempDir(), "does-not-exist"),
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	deferred, err := rt.ImportImages(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil for missing dir", err)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want empty", deferred)
	}
}

// TestImportImages_EmptyImagesDir verifies an empty (but existing) ImagesDir
// returns no deferred and no error.
func TestImportImages_EmptyImagesDir(t *testing.T) {
	dir := t.TempDir()
	rt := &Runtime{
		cfg: Config{
			ImagesDir: dir,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	deferred, err := rt.ImportImages(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want empty", deferred)
	}
}

// TestImportImages_SkipsNonTarFiles verifies non-.tar files in the images
// directory are ignored entirely (no deferred, no error).
func TestImportImages_SkipsNonTarFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{
		cfg: Config{
			ImagesDir: dir,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
	deferred, err := rt.ImportImages(context.Background())
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want empty", deferred)
	}
}

// endsWith is a tiny helper so we don't drag strings into the import set.
func endsWith(s, suf string) bool {
	if len(suf) > len(s) {
		return false
	}
	return s[len(s)-len(suf):] == suf
}
