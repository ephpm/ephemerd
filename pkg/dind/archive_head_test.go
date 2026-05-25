package dind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestArchiveHEAD_NotFound is the lightweight routing test. A HEAD against
// /containers/{id}/archive for a container the daemon doesn't know about
// must respond 404, matching the existing PUT and GET handlers in
// TestCopyToNotFound / TestCopyFromNotFound. Before this fix the route
// fell through to handleNotImplemented (501) because no MethodHead case
// existed in routeContainer — that's exactly the routing gap that makes
// the Docker CLI's `docker cp` produce "unable to decode container path
// stat header: EOF" on stopped containers.
func TestArchiveHEAD_NotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	req, err := http.NewRequest("HEAD", "http://docker/containers/nonexistent/archive?path=/tmp", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD /containers/nonexistent/archive: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (matching PUT and GET archive handlers)", resp.StatusCode)
	}
}

// TestArchiveHEAD_ReturnsStatHeader plants a fake containerEntry pointing at
// a tempdir with a known file, then issues a HEAD and asserts the
// X-Docker-Container-Path-Stat header decodes into a Docker-shaped struct
// with the expected name/size/mode. This is the precise contract the Docker
// CLI's StatPath uses — anything less than this and `docker cp` returns
// "unable to decode container path stat header: EOF" even on a 200 response.
func TestArchiveHEAD_ReturnsStatHeader(t *testing.T) {
	rootfs := t.TempDir()
	const wantName = "output.tar"
	const wantBody = "hello-from-stopped-container\n"
	if err := os.WriteFile(filepath.Join(rootfs, wantName), []byte(wantBody), 0o644); err != nil {
		t.Fatalf("seeding rootfs file: %v", err)
	}

	s := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		containers: map[string]*containerEntry{},
	}
	const cid = "abc123"
	s.containers[cid] = &containerEntry{ID: cid, Status: "exited"}
	// Test override: hand the HEAD handler our planted rootfs without
	// going through containerd's snapshotter.
	s.rootfsSearchDirsFn = func(_ context.Context, _ string) ([]string, error) {
		return []string{rootfs}, nil
	}

	req, err := http.NewRequest("HEAD", "http://docker/containers/"+cid+"/archive?path=/"+wantName, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	s.routeContainer(rec, req, "/containers/"+cid+"/archive")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	hdr := rec.Header().Get("X-Docker-Container-Path-Stat")
	if hdr == "" {
		t.Fatal("X-Docker-Container-Path-Stat header missing — CLI will fail with 'unable to decode container path stat header: EOF'")
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("header not valid base64: %v", err)
	}
	// Docker's containerPathStat shape (engine-api types/container/file.go).
	var got struct {
		Name       string      `json:"name"`
		Size       int64       `json:"size"`
		Mode       os.FileMode `json:"mode"`
		Mtime      time.Time   `json:"mtime"`
		LinkTarget string      `json:"linkTarget"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("header JSON: %v", err)
	}
	if got.Name != wantName {
		t.Errorf("Name = %q, want %q", got.Name, wantName)
	}
	if got.Size != int64(len(wantBody)) {
		t.Errorf("Size = %d, want %d", got.Size, len(wantBody))
	}
	if got.Mode.IsDir() {
		t.Errorf("Mode = %v, want regular file", got.Mode)
	}
}

// TestArchiveGET_ReturnsStatHeader asserts the same X-Docker-Container-Path-Stat
// header is set on the GET response too. Some Docker clients skip the HEAD
// pre-flight and rely on the header on the GET. Regression guard for the
// shared header-emit code path.
func TestArchiveGET_ReturnsStatHeader(t *testing.T) {
	rootfs := t.TempDir()
	const wantName = "data.bin"
	if err := os.WriteFile(filepath.Join(rootfs, wantName), []byte("xyz"), 0o644); err != nil {
		t.Fatalf("seeding rootfs file: %v", err)
	}
	s := &Server{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		containers: map[string]*containerEntry{},
	}
	const cid = "def456"
	s.containers[cid] = &containerEntry{ID: cid, Status: "exited"}
	s.rootfsSearchDirsFn = func(_ context.Context, _ string) ([]string, error) {
		return []string{rootfs}, nil
	}

	req, err := http.NewRequest("GET", "http://docker/containers/"+cid+"/archive?path=/"+wantName, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	s.routeContainer(rec, req, "/containers/"+cid+"/archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Docker-Container-Path-Stat") == "" {
		t.Error("GET response missing X-Docker-Container-Path-Stat header")
	}
}
