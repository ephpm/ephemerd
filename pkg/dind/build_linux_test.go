//go:build linux

package dind

import (
	"archive/tar"
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

	containerdclient "github.com/containerd/containerd/v2/client"
)

// newSentinelClient returns a non-nil *client.Client whose only purpose is
// to make the early "s.client == nil" check in handleImageBuild pass. The
// downstream paths under test (invalid tar context, missing Dockerfile)
// fail before any client method is invoked, so the zero value is safe.
func newSentinelClient() *containerdclient.Client {
	return &containerdclient.Client{}
}

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

// newBuildTestServer builds a Server with no containerd client. The /build
// handler returns a synchronous 500 in that case, which is what we want to
// drive the early error paths.
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

// writeBuildTar packages the given files into a tar archive and returns the
// bytes. Each entry has mode 0o644.
func writeBuildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("Write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	return buf.Bytes()
}

// readNDJSON reads newline-delimited JSON objects from a body.
func readNDJSON(t *testing.T, body io.Reader) []map[string]string {
	t.Helper()
	out := []map[string]string{}
	dec := json.NewDecoder(body)
	for {
		var m map[string]string
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return out
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, m)
	}
}

func TestHandleImageBuild_NoClient(t *testing.T) {
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

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (no containerd client)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "containerd client not available") {
		t.Errorf("body = %q, want containerd-client error", string(body))
	}
}

// fakeStorageServer is a minimal Server that has a non-nil client field set
// via reflection-like manipulation. Since the containerd client type is
// concrete, we instead drive the request body error paths without needing
// a real client. The "client is nil" early-return covers the no-client
// case; for the rest we need a non-nil client.
//
// Approach: allocate an unstarted *client.Client value via the zero struct
// pointer so the early nil-check passes. This is a hack — but the code
// then proceeds to extractBuildContext (via r.Body) which fails BEFORE any
// real containerd call when the body isn't a valid tar.

func newServerWithFakeClient(t *testing.T) *Server {
	t.Helper()
	s := newBuildTestServer(t)
	// Inject a non-nil sentinel pointer so the early-return doesn't fire.
	// We never actually call methods on it because the build context fails
	// first.
	s.client = newSentinelClient()
	return s
}

func TestHandleImageBuild_InvalidTarContext(t *testing.T) {
	s := newServerWithFakeClient(t)
	cli := dialUnix(s.SocketPath())

	// A body that is neither tar nor gzip — extractBuildContext should fail
	// with a tar parse error. The handler streams the error to the client
	// (status is still 200 from the streaming header).
	resp, err := cli.Post(
		"http://docker/build",
		"application/x-tar",
		bytes.NewReader([]byte("definitely not a tar file at all, completely invalid")),
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (handler streams errors in body)", resp.StatusCode)
	}

	msgs := readNDJSON(t, resp.Body)
	// Look for an "error" message about extracting build context.
	var foundErr bool
	for _, m := range msgs {
		if errMsg, ok := m["error"]; ok && strings.Contains(errMsg, "extracting build context") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("expected error message about build context, got messages: %v", msgs)
	}
}

func TestHandleImageBuild_MissingDockerfile(t *testing.T) {
	s := newServerWithFakeClient(t)
	cli := dialUnix(s.SocketPath())

	// A valid tar that doesn't contain "Dockerfile" — handler should report
	// "dockerfile not found in build context".
	tarBytes := writeBuildTar(t, map[string]string{
		"main.go": "package main\n",
	})

	resp, err := cli.Post(
		"http://docker/build",
		"application/x-tar",
		bytes.NewReader(tarBytes),
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	msgs := readNDJSON(t, resp.Body)
	var foundErr bool
	for _, m := range msgs {
		if errMsg, ok := m["error"]; ok && strings.Contains(errMsg, "not found in build context") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("expected dockerfile-not-found error, got messages: %v", msgs)
	}
}

func TestHandleImageBuild_CustomDockerfileNameMissing(t *testing.T) {
	s := newServerWithFakeClient(t)
	cli := dialUnix(s.SocketPath())

	// Custom Dockerfile name in query, body has only "Dockerfile".
	tarBytes := writeBuildTar(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
	})

	resp, err := cli.Post(
		"http://docker/build?dockerfile=custom.Dockerfile",
		"application/x-tar",
		bytes.NewReader(tarBytes),
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	msgs := readNDJSON(t, resp.Body)
	var foundErr bool
	for _, m := range msgs {
		if errMsg, ok := m["error"]; ok && strings.Contains(errMsg, "custom.Dockerfile") && strings.Contains(errMsg, "not found") {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("expected custom.Dockerfile-not-found error, got messages: %v", msgs)
	}
}

func TestHandleImageBuild_VersionedRoute(t *testing.T) {
	// /v1.45/build should also reach the handler.
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

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}
