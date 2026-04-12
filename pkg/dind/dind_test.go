package dind

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// dialSocket returns an http.Client that connects via the given Unix socket.
func dialSocket(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
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
	client := dialSocket(s.SocketPath())

	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("GET /_ping: %v", err)
	}
	defer resp.Body.Close()

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
	client := dialSocket(s.SocketPath())

	// Test both versioned and unversioned paths
	for _, path := range []string{"/version", "/v1.45/version"} {
		resp, err := client.Get("http://docker" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()

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
	client := dialSocket(s.SocketPath())

	resp, err := client.Get("http://docker/info")
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	defer resp.Body.Close()

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
	client := dialSocket(s.SocketPath())

	resp, err := client.Get("http://docker/images/json")
	if err != nil {
		t.Fatalf("GET /images/json: %v", err)
	}
	defer resp.Body.Close()

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

func TestNotImplemented(t *testing.T) {
	s := newTestServer(t)
	client := dialSocket(s.SocketPath())

	resp, err := client.Get("http://docker/containers/json")
	if err != nil {
		t.Fatalf("GET /containers/json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestSocketCleanup(t *testing.T) {
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
	client := dialSocket(s.SocketPath())

	// Pull without a containerd client should return 500
	resp, err := client.Post("http://docker/images/create?fromImage=alpine&tag=latest", "", nil)
	if err != nil {
		t.Fatalf("POST /images/create: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestImagePullMissingFromImage(t *testing.T) {
	s := newTestServer(t)
	client := dialSocket(s.SocketPath())

	resp, err := client.Post("http://docker/images/create", "", nil)
	if err != nil {
		t.Fatalf("POST /images/create: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
