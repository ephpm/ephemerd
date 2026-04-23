//go:build linux && e2e && privileged

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/dind"
)

// TestE2E_Dind_ExecHijack verifies that POST /exec/start with the Docker
// HTTP-upgrade handshake returns 101 + multiplexed-stream framing. This is
// exactly the protocol path that buildx's docker-container driver uses, and
// the path that was returning 200 before the hijack implementation landed.
//
// We speak the Docker API by hand (raw HTTP over a unix socket) so the test
// doesn't pull in the docker/docker SDK as a dep.
func TestE2E_Dind_ExecHijack(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "exec-hijack",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("dind.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("dind.Start: %v", err)
	}
	defer srv.Stop()

	client := dialDindSocket(srv.SocketPath())

	// Create + start a long-running container so we can exec into it.
	createBody, _ := json.Marshal(map[string]any{
		"Image": "busybox:latest",
		"Cmd":   []string{"sleep", "60"},
	})
	cresp, err := client.Post("http://docker/containers/create", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cresp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(cresp.Body)
		_ = cresp.Body.Close()
		t.Fatalf("create status=%d body=%s", cresp.StatusCode, body)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(cresp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	_ = cresp.Body.Close()
	t.Logf("container created id=%s", created.ID)
	defer removeContainer(t, client, created.ID)

	sresp, err := client.Post("http://docker/containers/"+created.ID+"/start", "application/json", nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = sresp.Body.Close()
	if sresp.StatusCode != http.StatusNoContent {
		t.Fatalf("start status=%d", sresp.StatusCode)
	}

	// /exec/create
	execBody, _ := json.Marshal(map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          []string{"sh", "-c", "echo out; echo err 1>&2; exit 3"},
	})
	eresp, err := client.Post("http://docker/containers/"+created.ID+"/exec", "application/json", bytes.NewReader(execBody))
	if err != nil {
		t.Fatalf("exec create: %v", err)
	}
	if eresp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(eresp.Body)
		_ = eresp.Body.Close()
		t.Fatalf("exec create status=%d body=%s", eresp.StatusCode, body)
	}
	var execRes struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(eresp.Body).Decode(&execRes); err != nil {
		t.Fatalf("decode exec: %v", err)
	}
	_ = eresp.Body.Close()
	t.Logf("exec created id=%s", execRes.ID)

	// /exec/start with Upgrade — this is where the bug was.
	conn, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer conn.Close()

	body := `{"Detach":false,"Tty":false}`
	req := "POST /exec/" + execRes.ID + "/start HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Upgrade: tcp\r\n" +
		"Connection: Upgrade\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) +
		body
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		t.Fatalf("want 101 UPGRADED, got %q", statusLine)
	}
	// Drain headers.
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimRight(l, "\r\n") == "" {
			break
		}
	}

	// Demux stdout/stderr frames until EOF. Collect until 5s timeout or EOF.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	var stdout, stderr bytes.Buffer
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			t.Fatalf("read frame header: %v", err)
		}
		size := int(binary.BigEndian.Uint32(hdr[4:]))
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatalf("read frame payload: %v", err)
		}
		switch hdr[0] {
		case 1:
			stdout.Write(payload)
		case 2:
			stderr.Write(payload)
		default:
			t.Fatalf("unknown stream id: %d", hdr[0])
		}
	}

	if strings.TrimSpace(stdout.String()) != "out" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "out\n")
	}
	if strings.TrimSpace(stderr.String()) != "err" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "err\n")
	}

	// Exit code via /exec/{id}/json inspect.
	iresp, err := client.Get("http://docker/exec/" + execRes.ID + "/json")
	if err != nil {
		t.Fatalf("exec inspect: %v", err)
	}
	defer iresp.Body.Close()
	var inspect struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := json.NewDecoder(iresp.Body).Decode(&inspect); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if inspect.Running {
		t.Errorf("inspect.Running=true, want false after exec exit")
	}
	if inspect.ExitCode != 3 {
		t.Errorf("inspect.ExitCode=%d, want 3", inspect.ExitCode)
	}
}

// TestE2E_Dind_DockerCLI covers the happy-path docker CLI commands that use
// hijacked endpoints. Skipped if `docker` isn't on PATH — this makes the
// test opt-in for contributors who have Docker installed (same as the
// existing dind_test.go e2e).
func TestE2E_Dind_DockerCLI(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH — skipping")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "docker-cli",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("dind.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("dind.Start: %v", err)
	}
	defer srv.Stop()

	env := append(os.Environ(),
		"DOCKER_HOST=unix://"+srv.SocketPath(),
		"DOCKER_BUILDKIT=1",
	)

	// Happy paths: these exercise pull, run (create+start+attach via logs),
	// and exec (hijack). Each step should exit 0.
	cases := []struct {
		name string
		args []string
	}{
		{"version", []string{"version"}},
		{"info", []string{"info"}},
		{"pull-busybox", []string{"pull", "busybox:latest"}},
		{"run-echo", []string{"run", "--rm", "busybox:latest", "echo", "hello-dind"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", tc.args...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("docker %v: %v\n%s", tc.args, err, out)
			}
			t.Logf("docker %v: OK\n%s", tc.args, truncate(string(out), 400))
		})
	}

	// Exec hijack via docker CLI. Start a container detached, exec into it,
	// verify output.
	runOut, err := exec.Command("docker", append([]string{"run", "-d", "busybox:latest", "sleep", "60"})...).Output()
	_ = runOut // unused: intentional — we just need the container ID from the next CLI
	if err != nil {
		// Retry with env set (Output() doesn't inherit from cmd.Env).
		c := exec.Command("docker", "run", "-d", "busybox:latest", "sleep", "60")
		c.Env = env
		runOut, err = c.Output()
		if err != nil {
			t.Fatalf("docker run -d: %v\n%s", err, runOut)
		}
	}
	cid := strings.TrimSpace(string(runOut))
	if cid == "" {
		t.Fatal("empty container id from docker run -d")
	}
	t.Cleanup(func() {
		killc := exec.Command("docker", "kill", cid)
		killc.Env = env
		_ = killc.Run()
		rmc := exec.Command("docker", "rm", cid)
		rmc.Env = env
		_ = rmc.Run()
	})

	execCmd := exec.Command("docker", "exec", cid, "echo", "exec-works")
	execCmd.Env = env
	eout, err := execCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec: %v\n%s", err, eout)
	}
	if !strings.Contains(string(eout), "exec-works") {
		t.Errorf("exec output %q missing 'exec-works'", eout)
	}
}

// TestE2E_Dind_BuildxBuild is the integration target — it runs `docker buildx
// build` against our fake daemon and verifies a build succeeds. Skipped if
// `docker` + `buildx` aren't installed. This is the regression test that
// covers the exec-hijack + attach bug path.
func TestE2E_Dind_BuildxBuild(t *testing.T) {
	if sharedCtrd == nil {
		t.Fatal("shared containerd not available")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH — skipping")
	}
	// buildx is a docker plugin — verify with `docker buildx version`.
	vcmd := exec.Command("docker", "buildx", "version")
	if err := vcmd.Run(); err != nil {
		t.Skip("docker buildx not available — skipping")
	}

	srv, err := dind.New(dind.Config{
		JobID:   "buildx-build",
		DataDir: sharedDataDir,
		Client:  sharedCtrd.Client(),
		Log:     sharedLog,
	})
	if err != nil {
		t.Fatalf("dind.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("dind.Start: %v", err)
	}
	defer srv.Stop()

	// Write a trivial Dockerfile.
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"),
		[]byte("FROM busybox:latest\nRUN echo buildx-ok > /proof.txt\n"), 0o644); err != nil {
		t.Fatalf("writing Dockerfile: %v", err)
	}

	env := append(os.Environ(),
		"DOCKER_HOST=unix://"+srv.SocketPath(),
		"DOCKER_BUILDKIT=1",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Use --load to actually boot buildkit + read through our daemon.
	cmd := exec.CommandContext(ctx, "docker", "buildx", "build", "--load", "-t", "dind-buildx-test:latest", ctxDir)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker buildx build: %v\n%s", err, out)
	}
	t.Logf("buildx build succeeded:\n%s", truncate(string(out), 1500))
}

// removeContainer best-effort cleans up a container created by a test.
func removeContainer(t *testing.T, client *http.Client, id string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete,
		"http://docker/containers/"+id+"?force=true", nil)
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}
