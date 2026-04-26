package dind

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dialServer returns an http.Client that connects to the given dind.Server.
// Linux/macOS: dials the unix socket at s.SocketPath().
// Windows: dials the TCP endpoint at s.Endpoint() (tcp://host:port).
// Tests issue requests against a placeholder "http://docker/..." URL; the
// DialContext hook ignores the URL and connects to the real transport.
func dialServer(s *Server) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				if runtime.GOOS == "windows" {
					return net.Dial("tcp", strings.TrimPrefix(s.Endpoint(), "tcp://"))
				}
				return net.Dial("unix", s.SocketPath())
			},
		},
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := New(Config{
		JobID:   "test-job-1",
		DataDir: dataDir,
		Client:  nil, // no containerd needed for health tests
		Log:     log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestPing(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("GET /_ping: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("body = %q, want OK", string(body))
	}
	if v := resp.Header.Get("API-Version"); v != "1.45" {
		t.Errorf("API-Version = %q, want 1.45", v)
	}
}

func TestVersion(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// Test both versioned and unversioned paths
	for _, path := range []string{"/version", "/v1.45/version"} {
		resp, err := client.Get("http://docker" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Logf("closing response body: %v", err)
			}
		}()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode)
		}

		var v map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if ver, ok := v["Version"].(string); !ok || ver != "27.0.0-ephemerd" {
			t.Errorf("%s: Version = %v, want 27.0.0-ephemerd", path, v["Version"])
		}
		if api, ok := v["ApiVersion"].(string); !ok || api != "1.45" {
			t.Errorf("%s: ApiVersion = %v, want 1.45", path, v["ApiVersion"])
		}
	}
}

func TestInfo(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/info")
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if name, ok := info["Name"].(string); !ok || name != "ephemerd-dind" {
		t.Errorf("Name = %v, want ephemerd-dind", info["Name"])
	}
}

func TestImageListEmpty(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/images/json")
	if err != nil {
		t.Fatalf("GET /images/json: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var images []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected empty image list, got %d", len(images))
	}
}

func TestContainerListEmpty(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/containers/json")
	if err != nil {
		t.Fatalf("GET /containers/json: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var containers []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("expected empty container list, got %d", len(containers))
	}
}

func TestContainerCreateNoClient(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Image": "alpine:latest"})
	resp, err := client.Post("http://docker/containers/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /containers/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (no containerd client)", resp.StatusCode)
	}
}

func TestContainerCreateNoImage(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Cmd": []string{"echo", "hello"}})
	resp, err := client.Post("http://docker/containers/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /containers/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	// No containerd client → 500 before image check.
	// This validates that the request parsing works.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestContainerInspectNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/containers/nonexistent/json")
	if err != nil {
		t.Fatalf("GET /containers/nonexistent/json: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestContainerStartNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Post("http://docker/containers/nonexistent/start", "", nil)
	if err != nil {
		t.Fatalf("POST /containers/nonexistent/start: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestContainerWaitNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Post("http://docker/containers/nonexistent/wait", "", nil)
	if err != nil {
		t.Fatalf("POST /containers/nonexistent/wait: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestContainerLogsNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/containers/nonexistent/logs")
	if err != nil {
		t.Fatalf("GET /containers/nonexistent/logs: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNotImplemented(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// networks endpoint is not implemented
	resp, err := client.Get("http://docker/networks")
	if err != nil {
		t.Fatalf("GET /networks: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestSocketCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket cleanup is unix-socket-specific; Windows uses a TCP endpoint with no filesystem artifact")
	}
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := New(Config{
		JobID:   "cleanup-test",
		DataDir: dataDir,
		Client:  nil,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sockPath := s.SocketPath()

	// Verify socket exists
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		t.Fatal("socket should exist after Start")
	}

	s.Stop()

	// Verify docker directory was cleaned up
	dockerDir := filepath.Dir(sockPath)
	if _, err := os.Stat(dockerDir); !os.IsNotExist(err) {
		t.Errorf("docker dir should be cleaned up after Stop: %s", dockerDir)
	}
}

func TestImagePullNoClient(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// Pull without a containerd client should return 500
	resp, err := client.Post("http://docker/images/create?fromImage=alpine&tag=latest", "", nil)
	if err != nil {
		t.Fatalf("POST /images/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestImagePullMissingFromImage(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Post("http://docker/images/create", "", nil)
	if err != nil {
		t.Fatalf("POST /images/create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExecCreateNoContainer(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	body, _ := json.Marshal(map[string]any{"Cmd": []string{"echo", "hi"}})
	resp, err := client.Post("http://docker/containers/nonexistent/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /containers/nonexistent/exec: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestExecStartNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Post("http://docker/exec/nonexistent/start", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /exec/nonexistent/start: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestExecInspectNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/exec/nonexistent/json")
	if err != nil {
		t.Fatalf("GET /exec/nonexistent/json: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCopyToNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	req, _ := http.NewRequest("PUT", "http://docker/containers/nonexistent/archive?path=/tmp", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT /containers/nonexistent/archive: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCopyFromNotFound(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	resp, err := client.Get("http://docker/containers/nonexistent/archive?path=/tmp")
	if err != nil {
		t.Fatalf("GET /containers/nonexistent/archive: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBuildRoute(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	// On Linux with no client: 500. On non-Linux: 501.
	resp, err := client.Post("http://docker/v1.45/build?t=myapp", "application/x-tar", nil)
	if err != nil {
		t.Fatalf("POST /v1.45/build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if runtime.GOOS == "linux" {
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 (no containerd client)", resp.StatusCode)
		}
	} else {
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501 (not supported on %s)", resp.StatusCode, runtime.GOOS)
		}
	}
}

// --- writeJSON tests ---

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"key": "val"})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["key"] != "val" {
		t.Errorf("key = %q, want val", body["key"])
	}
}

func TestWriteJSON_CustomStatus(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestWriteJSON_Array(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, []string{"a", "b"})

	var body []string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 2 || body[0] != "a" {
		t.Errorf("body = %v, want [a b]", body)
	}
}

func TestImageList(t *testing.T) {
	s := newTestServer(t)

	s.mu.Lock()
	s.images["alpine:latest"] = &imageEntry{ID: "sha256:abc123", Ref: "alpine:latest", Size: 5000000}
	s.images["ubuntu:22.04"] = &imageEntry{ID: "sha256:def456", Ref: "ubuntu:22.04", Size: 30000000}
	s.mu.Unlock()

	client := dialServer(s)
	resp, err := client.Get("http://docker/images/json")
	if err != nil {
		t.Fatalf("GET /images/json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var images []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}
	for _, img := range images {
		if _, ok := img["Id"]; !ok {
			t.Error("image missing Id field")
		}
		if tags, ok := img["RepoTags"]; !ok {
			t.Error("image missing RepoTags field")
		} else if tagList, ok := tags.([]any); !ok || len(tagList) == 0 {
			t.Error("RepoTags should be a non-empty array")
		}
	}
}

func TestInfoCount(t *testing.T) {
	s := newTestServer(t)

	s.mu.Lock()
	s.images["img1"] = &imageEntry{ID: "sha256:1", Ref: "img1", Size: 100}
	s.images["img2"] = &imageEntry{ID: "sha256:2", Ref: "img2", Size: 200}
	s.images["img3"] = &imageEntry{ID: "sha256:3", Ref: "img3", Size: 300}
	s.mu.Unlock()

	client := dialServer(s)
	resp, err := client.Get("http://docker/info")
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	imageCount, ok := info["Images"].(float64)
	if !ok {
		t.Fatalf("Images field missing or not a number: %v", info["Images"])
	}
	if imageCount != 3 {
		t.Errorf("Images = %v, want 3", imageCount)
	}
}

func TestIndexOf(t *testing.T) {
	tests := []struct {
		s    string
		b    byte
		want int
	}{
		{"hello", 'l', 2},
		{"hello", 'o', 4},
		{"hello", 'h', 0},
		{"hello", 'z', -1},
		{"", '/', -1},
		{"/version", '/', 0},
		{"1.45/version", '/', 4},
	}
	for _, tt := range tests {
		got := indexOf(tt.s, tt.b)
		if got != tt.want {
			t.Errorf("indexOf(%q, %q) = %d, want %d", tt.s, tt.b, got, tt.want)
		}
	}
}

func TestRouteVer(t *testing.T) {
	s := newTestServer(t)
	client := dialServer(s)

	paths := []struct {
		path       string
		wantStatus int
	}{
		{"/_ping", http.StatusOK},
		{"/v1.45/_ping", http.StatusOK},
		{"/v1.24/_ping", http.StatusOK},
		{"/version", http.StatusOK},
		{"/v1.45/version", http.StatusOK},
		{"/info", http.StatusOK},
		{"/v1.45/info", http.StatusOK},
		{"/images/json", http.StatusOK},
		{"/v1.45/images/json", http.StatusOK},
		{"/unknown/endpoint", http.StatusNotImplemented},
		{"/v1.45/unknown/endpoint", http.StatusNotImplemented},
	}
	for _, tt := range paths {
		resp, err := client.Get("http://docker" + tt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("GET %s: status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
		}
	}
}
