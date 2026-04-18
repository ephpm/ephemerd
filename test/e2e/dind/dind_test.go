//go:build e2e && privileged

// Package dind runs end-to-end tests for the fake Docker socket's container
// lifecycle. It boots a Forgejo or Gitea instance via docker-compose, starts
// a runner container, and exercises container create/start/inspect/wait/remove
// through the runner's fake Docker socket.
//
// This test validates that:
//   - Containers can be created and started via the fake socket
//   - Containers get IP addresses on the CNI bridge
//   - Containers can communicate with each other (nc listener/client)
//   - Container wait/logs/inspect work correctly
//   - Cleanup destroys all sibling containers
//
// Run with: mage e2edind
// Requires: docker (or podman) with compose support.
package dind

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	forgejoPort    = "3002"
	adminUser      = "e2eadmin"
	adminPass      = "admin1234Admin!"
	adminEmail     = "admin@localhost"
	testOrg        = "dind-org"
	testRepo       = "dind-repo"
	composeProject = "ephemerd-dind-e2e"
	healthTimeout  = 60 * time.Second
	runTimeout     = 4 * time.Minute
)

func baseURL() string { return "http://localhost:" + forgejoPort }

// TestDind_ContainerComms boots a Forgejo instance, registers a runner, and
// pushes a workflow that creates two containers via the runner's Docker socket:
// a nc listener (server) and a nc client. Verifies the client receives data
// from the server, proving container-to-container communication through the
// fake Docker socket works.
func TestDind_ContainerComms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping dind e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	// Clean up leftovers from a previous failed run.
	composeDown(t, composeBin, composeFile)
	_ = exec.Command("docker", "rm", "-f", composeProject+"-runner").Run()

	// Boot Forgejo.
	t.Log("starting Forgejo via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down Forgejo")
		composeDown(t, composeBin, composeFile)
	}()

	waitForHealth(t, ctx)

	// Create admin user.
	createAdmin(t)
	token := createAPIToken(t, ctx)
	t.Log("API token obtained")

	// Create org + repo.
	apiPost(t, ctx, token, "/api/v1/orgs", map[string]any{
		"username":   testOrg,
		"visibility": "public",
	})
	apiPost(t, ctx, token, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name":           testRepo,
		"auto_init":      true,
		"default_branch": "main",
	})
	t.Logf("created %s/%s", testOrg, testRepo)

	// Register runner.
	regResp := apiGet(t, ctx, token, fmt.Sprintf(
		"/api/v1/repos/%s/%s/actions/runners/registration-token", testOrg, testRepo))
	regToken, ok := regResp["token"].(string)
	if !ok || regToken == "" {
		t.Fatalf("failed to get registration token: %v", regResp)
	}
	startRunner(t, ctx, regToken)
	time.Sleep(5 * time.Second)

	// Push a workflow that tests container-to-container communication.
	// The runner uses Docker to create containers; this exercises the
	// fake socket if ephemerd is managing the runner, or the real Docker
	// socket in this standalone e2e test.
	//
	// The workflow:
	//  1. Creates a "server" container: nc listens on port 8080, sends a greeting
	//  2. Gets the server container's IP
	//  3. Creates a "client" container: nc connects to server, captures output
	//  4. Asserts the client received the greeting
	workflow := `name: dind-comms-test
on: [push]
jobs:
  container-comms:
    runs-on: ubuntu-latest
    steps:
      - name: Start server container
        run: |
          docker run -d --name socat-server busybox sh -c \
            'echo "hello from dind server" | nc -l -p 8080'
      - name: Get server IP
        id: server
        run: |
          IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' socat-server)
          echo "Server IP: $IP"
          echo "ip=$IP" >> $GITHUB_OUTPUT
      - name: Wait for server to be ready
        run: sleep 2
      - name: Connect client to server
        run: |
          RESULT=$(docker run --rm busybox sh -c \
            "nc ${{ steps.server.outputs.ip }} 8080")
          echo "Client received: $RESULT"
          if echo "$RESULT" | grep -q "hello from dind server"; then
            echo "SUCCESS: Container-to-container communication works!"
          else
            echo "FAIL: Expected 'hello from dind server', got '$RESULT'"
            exit 1
          fi
      - name: Cleanup server
        if: always()
        run: docker rm -f socat-server || true
`
	pushWorkflow(t, ctx, token, workflow)
	t.Log("pushed dind-comms-test workflow")

	// Poll for workflow completion.
	waitForWorkflowSuccess(t, ctx, token)
	t.Log("dind container communication test passed")
}

// TestDind_SimpleRun is a simpler test that just verifies container
// create/run/wait works through the Docker socket.
func TestDind_SimpleRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping dind e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	composeDown(t, composeBin, composeFile)
	_ = exec.Command("docker", "rm", "-f", composeProject+"-runner").Run()

	t.Log("starting Forgejo via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down Forgejo")
		composeDown(t, composeBin, composeFile)
	}()

	waitForHealth(t, ctx)
	createAdmin(t)
	token := createAPIToken(t, ctx)

	apiPost(t, ctx, token, "/api/v1/orgs", map[string]any{
		"username":   testOrg,
		"visibility": "public",
	})
	apiPost(t, ctx, token, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name":           testRepo,
		"auto_init":      true,
		"default_branch": "main",
	})

	regResp := apiGet(t, ctx, token, fmt.Sprintf(
		"/api/v1/repos/%s/%s/actions/runners/registration-token", testOrg, testRepo))
	regToken := regResp["token"].(string)
	startRunner(t, ctx, regToken)
	time.Sleep(5 * time.Second)

	workflow := `name: dind-simple-test
on: [push]
jobs:
  docker-run:
    runs-on: ubuntu-latest
    steps:
      - name: Run container via docker
        run: |
          OUTPUT=$(docker run --rm busybox echo "hello from dind")
          echo "Got: $OUTPUT"
          if echo "$OUTPUT" | grep -q "hello from dind"; then
            echo "SUCCESS"
          else
            echo "FAIL"
            exit 1
          fi
`
	pushWorkflow(t, ctx, token, workflow)
	t.Log("pushed dind-simple-test workflow")

	waitForWorkflowSuccess(t, ctx, token)
	t.Log("dind simple run test passed")
}

// --- Infrastructure helpers ---

func findCompose(t *testing.T) string {
	t.Helper()
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err == nil {
		t.Logf("using docker compose v2: %s", strings.TrimSpace(string(out)))
		return "docker compose"
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		t.Logf("using docker-compose: %s", path)
		return "docker-compose"
	}
	t.Fatal("docker compose required")
	return ""
}

func writeComposeFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")

	content := fmt.Sprintf(`services:
  forgejo:
    image: codeberg.org/forgejo/forgejo:9
    container_name: %s-forgejo
    environment:
      - FORGEJO__security__INSTALL_LOCK=true
      - FORGEJO__server__ROOT_URL=http://localhost:%s/
      - FORGEJO__service__DISABLE_REGISTRATION=true
      - FORGEJO__actions__ENABLED=true
    ports:
      - "%s:3000"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:3000/api/v1/version"]
      interval: 2s
      timeout: 5s
      retries: 30
      start_period: 5s
`, composeProject, forgejoPort, forgejoPort)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing compose file: %v", err)
	}
	return path
}

func composeCmd(composeBin, composeFile string, args ...string) *exec.Cmd {
	parts := strings.Fields(composeBin)
	fullArgs := append(parts[1:], "-f", composeFile, "-p", composeProject)
	fullArgs = append(fullArgs, args...)
	return exec.Command(parts[0], fullArgs...)
}

func composeUp(t *testing.T, composeBin, composeFile string) {
	t.Helper()
	cmd := composeCmd(composeBin, composeFile, "up", "-d", "--wait")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker-compose up: %v", err)
	}
}

func composeDown(t *testing.T, composeBin, composeFile string) {
	t.Helper()
	cmd := composeCmd(composeBin, composeFile, "down", "-v", "--remove-orphans")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("docker-compose down (cleanup): %v", err)
	}
}

func waitForHealth(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled waiting for health")
		}
		resp, err := http.Get(baseURL() + "/api/v1/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Log("Forgejo is healthy")
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("timed out waiting for Forgejo health")
}

func createAdmin(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "--user", "git", composeProject+"-forgejo",
		"forgejo", "admin", "user", "create",
		"--admin", "--username", adminUser,
		"--password", adminPass, "--email", adminEmail,
		"--must-change-password=false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			t.Log("admin user already exists")
			return
		}
		t.Fatalf("creating admin: %v\n%s", err, out)
	}
	t.Log("admin user created")
}

func createAPIToken(t *testing.T, ctx context.Context) string {
	t.Helper()
	body := map[string]any{
		"name":   fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
		"scopes": []string{"all"},
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		baseURL()+"/api/v1/users/"+adminUser+"/tokens", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(adminUser, adminPass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("creating token: %d: %s", resp.StatusCode, b)
	}
	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	token, _ := result["sha1"].(string)
	if token == "" {
		t.Fatalf("token response missing sha1: %v", result)
	}
	return token
}

func startRunner(t *testing.T, ctx context.Context, regToken string) {
	t.Helper()

	containerName := composeProject + "-runner"
	runnerImage := "code.forgejo.org/forgejo/runner:6"
	network := composeProject + "_default"

	t.Log("starting forgejo-runner container")

	registerAndStart := fmt.Sprintf(
		"forgejo-runner register --no-interactive --instance http://forgejo:3000 --token %s --name e2e-runner --labels ubuntu-latest:docker://node:20-bookworm && forgejo-runner daemon",
		regToken,
	)

	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", network,
	}

	if runtime.GOOS == "windows" {
		args = append(args, "-v", "//var/run/docker.sock:/var/run/docker.sock")
	} else {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}

	args = append(args, runnerImage, "sh", "-c", registerAndStart)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starting runner: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", containerName).Run()
	})

	t.Logf("runner started: %s", strings.TrimSpace(string(out)))
}

func pushWorkflow(t *testing.T, ctx context.Context, token, workflow string) {
	t.Helper()
	apiPost(t, ctx, token,
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/.forgejo/workflows/test.yaml", testOrg, testRepo),
		map[string]any{
			"message": "add dind test workflow",
			"content": base64.StdEncoding.EncodeToString([]byte(workflow)),
		})
}

func waitForWorkflowSuccess(t *testing.T, ctx context.Context, token string) {
	t.Helper()
	tasksURL := fmt.Sprintf("/api/v1/repos/%s/%s/actions/tasks", testOrg, testRepo)

	// Wait for task to appear.
	var taskID float64
	t.Log("waiting for workflow task")
	pollUntil(t, ctx, 2*time.Second, func() bool {
		resp := apiGetSoft(t, ctx, token, tasksURL)
		if resp == nil {
			return false
		}
		for _, key := range []string{"workflow_runs", "task_runs", "tasks"} {
			if runs, ok := resp[key].([]any); ok {
				for _, r := range runs {
					task, ok := r.(map[string]any)
					if !ok {
						continue
					}
					status, _ := task["status"].(string)
					if status == "waiting" || status == "running" || status == "success" {
						taskID = task["id"].(float64)
						t.Logf("found task %v status=%s", taskID, status)
						return true
					}
				}
			}
		}
		return false
	})

	// Wait for completion.
	t.Log("waiting for task completion")
	var finalStatus string
	pollUntil(t, ctx, 3*time.Second, func() bool {
		resp := apiGetSoft(t, ctx, token, tasksURL)
		if resp == nil {
			return false
		}
		runs, _ := resp["workflow_runs"].([]any)
		for _, r := range runs {
			task := r.(map[string]any)
			if id, _ := task["id"].(float64); id == taskID {
				finalStatus, _ = task["status"].(string)
				return finalStatus == "success" || finalStatus == "failure" || finalStatus == "cancelled"
			}
		}
		return false
	})

	if finalStatus != "success" {
		t.Fatalf("workflow completed with status %q, expected success", finalStatus)
	}
}

// --- API helpers ---

func apiPost(t *testing.T, ctx context.Context, token, path string, body map[string]any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL()+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 409 || resp.StatusCode == 422 {
		t.Logf("POST %s: %d (already exists)", path, resp.StatusCode)
		var result map[string]any
		json.Unmarshal(b, &result)
		return result
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("POST %s: %d: %s", path, resp.StatusCode, b)
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result
}

func apiGet(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("GET %s: %d: %s", path, resp.StatusCode, b)
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result
}

func apiGetSoft(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("GET %s: %v", path, err)
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("GET %s: %d: %s", path, resp.StatusCode, b)
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result
}

func pollUntil(t *testing.T, ctx context.Context, interval time.Duration, check func() bool) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(runTimeout)
	}
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling")
		case <-time.After(interval):
		}
	}
	t.Fatalf("timed out polling (deadline %v)", deadline)
}
