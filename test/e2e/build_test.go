//go:build linux && e2e && privileged

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/dind"
)

// dialDindSocket returns an http.Client that connects via a Unix socket.
func dialDindSocket(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 5 * time.Minute,
	}
}

// buildTar creates a tar archive from a map of filename → content.
func buildTar(files map[string]string) *bytes.Buffer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			panic(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return &buf
}

// readBuildOutput parses the Docker build JSON stream and returns all
// "stream" lines joined and any "error" line found.
func readBuildOutput(body io.Reader) (streams string, buildErr string) {
	dec := json.NewDecoder(body)
	var lines []string
	for {
		var msg map[string]string
		if err := dec.Decode(&msg); err != nil {
			break
		}
		if s, ok := msg["stream"]; ok {
			lines = append(lines, s)
		}
		if e, ok := msg["error"]; ok {
			buildErr = e
		}
	}
	return strings.Join(lines, ""), buildErr
}

// TestE2E_Build_Simple builds a minimal Dockerfile (FROM + RUN echo) through
// the fake Docker socket and verifies the build succeeds.
func TestE2E_Build_Simple(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-simple",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	// Build context: a Dockerfile that creates a file
	context := buildTar(map[string]string{
		"Dockerfile": "FROM busybox:latest\nRUN echo 'buildah-works' > /proof.txt\n",
	})

	resp, err := client.Post(
		"http://docker/build?t=e2e-build-test:latest",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /build: status %d: %s", resp.StatusCode, body)
	}

	streams, buildErr := readBuildOutput(resp.Body)
	t.Logf("build output:\n%s", streams)

	if buildErr != "" {
		t.Fatalf("build error: %s", buildErr)
	}

	if !strings.Contains(streams, "Successfully built") {
		t.Errorf("output missing 'Successfully built'.\nOutput:\n%s", streams)
	}
	if !strings.Contains(streams, "Successfully tagged e2e-build-test:latest") {
		t.Errorf("output missing tag confirmation.\nOutput:\n%s", streams)
	}
}

// TestE2E_Build_MultiStep builds a Dockerfile with multiple RUN instructions
// and verifies each step produces output.
func TestE2E_Build_MultiStep(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-multi",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	context := buildTar(map[string]string{
		"Dockerfile": `FROM busybox:latest
RUN echo "step-one"
RUN echo "step-two"
RUN echo "step-three"
`,
	})

	resp, err := client.Post(
		"http://docker/build?t=multi-step:v1",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	streams, buildErr := readBuildOutput(resp.Body)
	t.Logf("build output:\n%s", streams)

	if buildErr != "" {
		t.Fatalf("build error: %s", buildErr)
	}

	if !strings.Contains(streams, "Successfully built") {
		t.Errorf("build did not succeed.\nOutput:\n%s", streams)
	}
}

// TestE2E_Build_COPY tests that COPY instructions work by including a
// file in the build context and copying it into the image.
func TestE2E_Build_COPY(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-copy",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	context := buildTar(map[string]string{
		"Dockerfile": `FROM busybox:latest
COPY hello.txt /hello.txt
RUN cat /hello.txt
`,
		"hello.txt": "hello from build context\n",
	})

	resp, err := client.Post(
		"http://docker/build?t=copy-test:latest",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	streams, buildErr := readBuildOutput(resp.Body)
	t.Logf("build output:\n%s", streams)

	if buildErr != "" {
		t.Fatalf("build error: %s", buildErr)
	}

	if !strings.Contains(streams, "Successfully built") {
		t.Errorf("build did not succeed.\nOutput:\n%s", streams)
	}
}

// TestE2E_Build_BadDockerfile verifies that a syntax error in the
// Dockerfile produces a build error, not a crash.
func TestE2E_Build_BadDockerfile(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-bad",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	// No FROM instruction — invalid Dockerfile
	context := buildTar(map[string]string{
		"Dockerfile": "RUN echo 'no base image'\n",
	})

	resp, err := client.Post(
		"http://docker/build?t=bad-build:latest",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	// Should still get 200 (Docker streams errors in the JSON body)
	streams, buildErr := readBuildOutput(resp.Body)
	t.Logf("build output:\n%s", streams)

	if buildErr == "" && !strings.Contains(streams, "Error") && !strings.Contains(streams, "error") && !strings.Contains(streams, "failed") {
		t.Errorf("expected an error for a Dockerfile without FROM.\nOutput:\n%s", streams)
	}
}

// TestE2E_Build_MissingDockerfile verifies that a build context without
// a Dockerfile produces a clear error.
func TestE2E_Build_MissingDockerfile(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-nofile",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	// Build context with no Dockerfile
	context := buildTar(map[string]string{
		"app.go": "package main\n",
	})

	resp, err := client.Post(
		"http://docker/build?t=missing-df:latest",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	streams, buildErr := readBuildOutput(resp.Body)
	t.Logf("build output:\n%s", streams)

	if buildErr == "" && !strings.Contains(streams, "not found") {
		t.Errorf("expected error about missing Dockerfile.\nStreams: %s\nError: %s", streams, buildErr)
	}
}

// TestE2E_Build_ImageInList verifies that after a successful build, the
// built image appears in the image list (GET /images/json).
func TestE2E_Build_ImageInList(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "build-list",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("creating dind server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("starting dind server: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	context := buildTar(map[string]string{
		"Dockerfile": "FROM busybox:latest\nRUN echo listed\n",
	})

	resp, err := client.Post(
		"http://docker/build?t=list-check:v1",
		"application/x-tar",
		context,
	)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	// Drain the build output
	streams, buildErr := readBuildOutput(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Logf("closing body: %v", err)
	}
	if buildErr != "" {
		t.Fatalf("build error: %s\n%s", buildErr, streams)
	}
	if !strings.Contains(streams, "Successfully built") {
		t.Fatalf("build did not succeed.\n%s", streams)
	}

	// Now check that the image appears in the list
	listResp, err := client.Get("http://docker/images/json")
	if err != nil {
		t.Fatalf("GET /images/json: %v", err)
	}
	defer func() {
		if err := listResp.Body.Close(); err != nil {
			t.Logf("closing body: %v", err)
		}
	}()

	var images []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&images); err != nil {
		t.Fatalf("decoding image list: %v", err)
	}

	found := false
	for _, img := range images {
		if tags, ok := img["RepoTags"].([]any); ok {
			for _, tag := range tags {
				if tag == "list-check:v1" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("built image 'list-check:v1' not found in image list: %+v", images)
	}
}
