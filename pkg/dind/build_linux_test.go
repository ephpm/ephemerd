//go:build linux

package dind

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

// dialUnix returns an http.Client that connects via the given Unix socket.
func dialUnix(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

// newBuildTestServer builds a Server with no BuildKit solver. The /build
// handler returns 501 in that case.
func newBuildTestServer(t *testing.T) *Server {
	t.Helper()
	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(Config{
		JobID:   "build-test",
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
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestHandleImageBuild_NoBuildKit(t *testing.T) {
	s := newBuildTestServer(t)
	cli := dialUnix(s.SocketPath())
	resp, err := cli.Post("http://docker/build", "application/x-tar", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (no BuildKit)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "BuildKit") {
		t.Errorf("body = %q, want BuildKit error message", string(body))
	}
}

func TestHandleImageBuild_NoBuildKit_JSONResponse(t *testing.T) {
	s := newBuildTestServer(t)
	cli := dialUnix(s.SocketPath())
	resp, err := cli.Post("http://docker/build", "application/x-tar", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	var msg map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if _, ok := msg["message"]; !ok {
		t.Errorf("response missing 'message' key: %v", msg)
	}
}

func TestHandleImageBuild_VersionedRoute(t *testing.T) {
	s := newBuildTestServer(t)
	cli := dialUnix(s.SocketPath())

	resp, err := cli.Post("http://docker/v1.45/build", "application/x-tar", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("POST /v1.45/build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
