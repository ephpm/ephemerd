package dind

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerBuildOptsToSolveOpt_MinimalInput(t *testing.T) {
	req := httptest.NewRequest("POST", "/build?t=alpine:local&dockerfile=Dockerfile", nil)
	opt, err := dockerBuildOptsToSolveOpt(req, "/tmp/ctx", "test-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opt.Frontend != "dockerfile.v0" {
		t.Errorf("frontend = %q, want dockerfile.v0", opt.Frontend)
	}
	if opt.FrontendAttrs["filename"] != "Dockerfile" {
		t.Errorf("filename = %q, want Dockerfile", opt.FrontendAttrs["filename"])
	}
	if len(opt.Exports) != 1 {
		t.Fatalf("want 1 export, got %d", len(opt.Exports))
	}
	if opt.Exports[0].Type != "image" {
		t.Errorf("export type = %q, want image", opt.Exports[0].Type)
	}
	if got, want := opt.Exports[0].Attrs["name"], "build.ephemerd.local/test-job/alpine:local"; got != want {
		t.Errorf("export name = %q, want %q", got, want)
	}
	if opt.LocalDirs["context"] != "/tmp/ctx" {
		t.Errorf("LocalDirs[context] = %q, want /tmp/ctx", opt.LocalDirs["context"])
	}
}

func TestDockerBuildOptsToSolveOpt_AllOptions(t *testing.T) {
	q := "t=foo:bar" +
		"&dockerfile=Dockerfile.test" +
		"&target=prod" +
		"&nocache=1" +
		"&pull=1" +
		"&platform=linux/amd64,linux/arm64" +
		`&buildargs={"VERSION":"1.0","DEBUG":"true"}` +
		`&labels={"org.ephpm.test":"yes"}`
	req := httptest.NewRequest("POST", "/build?"+q, nil)
	opt, err := dockerBuildOptsToSolveOpt(req, "/ctx", "test-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"filename":           "Dockerfile.test",
		"target":             "prod",
		"no-cache":           "",
		"image-resolve-mode": "pull",
		"platform":           "linux/amd64,linux/arm64",
		"build-arg:VERSION":  "1.0",
		"build-arg:DEBUG":    "true",
		"label:org.ephpm.test": "yes",
	}
	for k, v := range want {
		if got := opt.FrontendAttrs[k]; got != v {
			t.Errorf("FrontendAttrs[%q] = %q, want %q", k, got, v)
		}
	}
}

func TestDockerBuildOptsToSolveOpt_InvalidBuildargsJSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/build?buildargs=not-json", nil)
	_, err := dockerBuildOptsToSolveOpt(req, "/ctx", "test-job")
	if err == nil {
		t.Fatal("want error for malformed buildargs JSON, got nil")
	}
	if !strings.Contains(err.Error(), "buildargs") {
		t.Errorf("error should mention buildargs, got: %v", err)
	}
}

func TestExtractBuildContextToTempDir_PlainTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "Dockerfile", "FROM alpine\nRUN echo hi\n")
	writeTarFile(t, tw, "src/app.go", "package main\n")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	dir, err := extractBuildContextToTempDir(&buf)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	assertFileContains(t, filepath.Join(dir, "Dockerfile"), "FROM alpine")
	assertFileContains(t, filepath.Join(dir, "src/app.go"), "package main")
}

func TestExtractBuildContextToTempDir_GzippedTar(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeTarFile(t, tw, "Dockerfile", "FROM scratch\n")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	dir, err := extractBuildContextToTempDir(&buf)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	assertFileContains(t, filepath.Join(dir, "Dockerfile"), "FROM scratch")
}

func TestExtractBuildContextToTempDir_RejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Deliberately traversal-y path.
	hdr := &tar.Header{
		Name:     "../../etc/evil",
		Mode:     0o644,
		Size:     int64(len("bad")),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("bad")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	_, err := extractBuildContextToTempDir(&buf)
	if err == nil {
		t.Fatal("want error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "invalid tar entry") {
		t.Errorf("error should mention invalid tar entry, got: %v", err)
	}
}

// writeTarFile is a small helper to add a regular file to a tar.Writer.
func writeTarFile(t *testing.T, tw *tar.Writer, name, body string) {
	t.Helper()
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header %q: %v", name, err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatalf("write tar body %q: %v", name, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("file %s does not contain %q\ncontents:\n%s", path, want, data)
	}
}
