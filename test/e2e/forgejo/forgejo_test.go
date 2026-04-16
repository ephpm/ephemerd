//go:build e2e && privileged

// Package forgejo runs end-to-end tests against a real Forgejo instance.
//
// The test boots a Forgejo container via docker-compose, provisions a test
// org/repo/workflow via the Forgejo API, registers a forgejo-runner, triggers
// a workflow run, and verifies the job completes successfully. Everything is
// torn down at the end regardless of pass/fail.
//
// Run with: mage e2eforgejo
// Requires: docker (or podman) with compose support.
package forgejo

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
	forgejoPort    = "3000"
	adminUser      = "e2eadmin"
	adminPass      = "admin1234Admin!"
	adminEmail     = "admin@localhost"
	testOrg        = "test-org"
	testRepo       = "test-repo"
	composeProject = "ephemerd-forgejo-e2e"
	healthTimeout  = 60 * time.Second
	runTimeout     = 3 * time.Minute
)

func baseURL() string { return "http://localhost:" + forgejoPort }

// TestForgejo_E2E boots a Forgejo instance, creates a test org/repo with
// a workflow, registers a runner, and verifies the workflow executes.
func TestForgejo_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping forgejo e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	// Clean up any leftovers from a previous failed run.
	composeDown(t, composeBin, composeFile)
	_ = exec.Command("docker", "rm", "-f", composeProject+"-runner").Run()

	// Boot Forgejo.
	t.Log("starting Forgejo via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down Forgejo")
		composeDown(t, composeBin, composeFile)
	}()

	// Wait for the API to respond.
	waitForHealth(t, ctx)

	// Create admin user via CLI inside the container.
	createAdmin(t)

	// Get API token.
	token := createAPIToken(t, ctx)
	t.Logf("API token obtained")

	// Create org + repo.
	apiPost(t, ctx, token, "/api/v1/orgs", map[string]any{
		"username":   testOrg,
		"visibility": "public",
	})
	t.Logf("created org %s", testOrg)

	apiPost(t, ctx, token, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name":           testRepo,
		"auto_init":      true,
		"default_branch": "main",
	})
	t.Logf("created repo %s/%s", testOrg, testRepo)

	// Get runner registration token.
	regResp := apiGet(t, ctx, token, fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners/registration-token", testOrg, testRepo))
	regToken, ok := regResp["token"].(string)
	if !ok || regToken == "" {
		t.Fatalf("failed to get runner registration token: %v", regResp)
	}
	t.Logf("runner registration token obtained")

	// Start the runner BEFORE pushing the workflow.
	// Forgejo won't create workflow runs unless a matching runner is registered.
	startRunner(t, ctx, regToken)

	// Give the runner a moment to register with Forgejo.
	time.Sleep(5 * time.Second)

	// Push a test workflow. Forgejo uses .forgejo/workflows/ path.
	// The push event triggers the workflow run now that a runner is registered.
	workflow := "name: e2e-test\non: [push]\njobs:\n  hello:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello from ephemerd forgejo e2e\n"
	apiPost(t, ctx, token, fmt.Sprintf("/api/v1/repos/%s/%s/contents/.forgejo/workflows/test.yaml", testOrg, testRepo), map[string]any{
		"message": "add test workflow",
		"content": base64.StdEncoding.EncodeToString([]byte(workflow)),
	})
	t.Logf("pushed test workflow")

	// Poll for the workflow task to appear and complete.
	// Forgejo v9 uses /actions/tasks (not /actions/runs).
	var taskID float64
	t.Log("waiting for workflow task to appear")
	tasksURL := fmt.Sprintf("/api/v1/repos/%s/%s/actions/tasks", testOrg, testRepo)
	pollAttempt := 0
	pollUntil(t, ctx, 2*time.Second, func() bool {
		pollAttempt++
		resp, statusCode, body := apiGetDebug(t, ctx, token, tasksURL)
		if pollAttempt <= 5 || pollAttempt%10 == 0 {
			t.Logf("poll attempt %d: status=%d body=%s", pollAttempt, statusCode, truncate(body, 500))
		}
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
						t.Logf("found task %v with status %s (key=%s)", taskID, status, key)
						return true
					}
				}
			}
		}
		return false
	})
	t.Logf("workflow task %v found", taskID)

	// Poll until the task completes.
	t.Log("waiting for workflow task to complete")
	var finalStatus string
	pollUntil(t, ctx, 3*time.Second, func() bool {
		resp := apiGetSoft(t, ctx, token, tasksURL)
		if resp == nil {
			return false
		}
		runs, _ := resp["workflow_runs"].([]any)
		for _, r := range runs {
			task := r.(map[string]any)
			id, _ := task["id"].(float64)
			if id == taskID {
				status, _ := task["status"].(string)
				finalStatus = status
				return status == "success" || status == "failure" || status == "cancelled"
			}
		}
		return false
	})

	if finalStatus != "success" {
		t.Fatalf("workflow run completed with status %q, expected success", finalStatus)
	}
	t.Logf("workflow run completed successfully")
}

// --- Infrastructure helpers ---

func findCompose(t *testing.T) string {
	t.Helper()
	// Try "docker compose" (v2 plugin) first, then "docker-compose" (standalone).
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err == nil {
		t.Logf("using docker compose v2: %s", strings.TrimSpace(string(out)))
		return "docker compose"
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		t.Logf("using docker-compose: %s", path)
		return "docker-compose"
	}
	t.Fatal("docker compose is required for forgejo e2e tests (neither 'docker compose' plugin nor 'docker-compose' binary found)")
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
		t.Logf("docker-compose down failed (cleanup): %v", err)
	}
}

func waitForHealth(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled waiting for forgejo health")
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
	t.Fatal("timed out waiting for Forgejo to become healthy")
}

func createAdmin(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "--user", "git", composeProject+"-forgejo",
		"forgejo", "admin", "user", "create",
		"--admin",
		"--username", adminUser,
		"--password", adminPass,
		"--email", adminEmail,
		"--must-change-password=false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// May already exist if test is re-run without cleanup.
		if strings.Contains(string(out), "already exists") {
			t.Log("admin user already exists, continuing")
			return
		}
		t.Fatalf("creating admin user: %v\n%s", err, out)
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
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL()+"/api/v1/users/"+adminUser+"/tokens", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(adminUser, adminPass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("creating API token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("creating API token: status %d: %s", resp.StatusCode, b)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	token, _ := result["sha1"].(string)
	if token == "" {
		t.Fatalf("API token response missing sha1: %v", result)
	}
	return token
}

func startRunner(t *testing.T, ctx context.Context, regToken string) {
	t.Helper()

	containerName := composeProject + "-runner"
	runnerImage := "code.forgejo.org/forgejo/runner:6"
	network := composeProject + "_default"

	t.Logf("starting forgejo-runner container")

	// The runner registers with Forgejo, then starts its daemon to poll for jobs.
	// Labels map "ubuntu-latest" to a docker image the runner will use.
	registerAndStart := fmt.Sprintf(
		"forgejo-runner register --no-interactive --instance http://forgejo:3000 --token %s --name e2e-runner --labels ubuntu-latest:docker://node:20-bookworm && forgejo-runner daemon",
		regToken,
	)

	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", network,
	}

	// Mount Docker socket so the runner can create sibling containers for jobs.
	if runtime.GOOS == "windows" {
		args = append(args, "-v", "//var/run/docker.sock:/var/run/docker.sock")
	} else {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}

	args = append(args, runnerImage, "sh", "-c", registerAndStart)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starting forgejo-runner: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", containerName).Run()
	})

	t.Logf("forgejo-runner container started: %s", strings.TrimSpace(string(out)))
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
		// Idempotent: resource already exists from a previous run.
		t.Logf("POST %s: %d (already exists, continuing)", path, resp.StatusCode)
		var result map[string]any
		json.Unmarshal(b, &result)
		return result
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, b)
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result
}

// apiGetDebug returns the parsed response, HTTP status code, and raw body.
// Does not fail on errors — used for debugging polling endpoints.
func apiGetDebug(t *testing.T, ctx context.Context, token, path string) (map[string]any, int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("Authorization", "token "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Sprintf("error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, string(b)
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result, resp.StatusCode, string(b)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// apiGetSoft is like apiGet but returns nil instead of failing on 404.
// Used for polling endpoints that may not have data yet.
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
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, b)
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
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, b)
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
