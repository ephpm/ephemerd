//go:build e2e && privileged

// Package gitea_test runs end-to-end tests for the Gitea provider against
// a real Gitea instance.
//
// The test boots a Gitea container via docker-compose, provisions a test
// org/repo via the Gitea REST API, then uses ephemerd's gitea.Provider
// (which speaks ConnectRPC) to register a runner, poll for tasks, and
// verify job events are received correctly.
//
// Run with: mage e2egitea
// Requires: docker (or podman) with compose support.
package gitea_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
	giteaprovider "github.com/ephpm/ephemerd/pkg/providers/gitea"
	"github.com/ephpm/ephemerd/pkg/providers"
)

const (
	giteaPort      = "3001"
	adminUser      = "e2eadmin"
	adminPass      = "admin1234Admin!"
	adminEmail     = "admin@localhost"
	testOrg        = "test-org"
	testRepo       = "test-repo"
	composeProject = "ephemerd-gitea-e2e"
	healthTimeout  = 60 * time.Second
	runTimeout     = 3 * time.Minute
)

func baseURL() string { return "http://localhost:" + giteaPort }

// TestGitea_E2E exercises ephemerd's Gitea provider against a real instance.
//
// Flow:
//  1. Boot Gitea, create org/repo
//  2. Get runner registration token via REST API
//  3. Start gitea.Provider → registers via ConnectRPC, polls FetchTask
//  4. Push a workflow to trigger a task
//  5. Receive JobEvent on the events channel
//  6. Verify ClaimJob returns correct env vars
//  7. Verify FetchJobImage extracts EPHEMERD_IMAGE from the task payload
func TestGitea_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gitea e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	composeDown(t, composeBin, composeFile)
	t.Log("starting Gitea via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down Gitea")
		composeDown(t, composeBin, composeFile)
	}()

	waitForHealth(t, ctx)
	createAdmin(t)
	apiToken := createAPIToken(t, ctx)
	t.Log("API token obtained")

	// Create org + repo.
	apiPost(t, ctx, apiToken, "/api/v1/orgs", map[string]any{
		"username": testOrg, "visibility": "public",
	})
	apiPost(t, ctx, apiToken, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name": testRepo, "auto_init": true, "default_branch": "main",
	})
	t.Logf("created %s/%s", testOrg, testRepo)

	// Get runner registration token.
	// Fetch registration token (handles Gitea GET/POST divergence automatically).
	rpcClient := forgerpc.NewClient(baseURL(), nil)
	regToken, err := rpcClient.RegistrationToken(ctx, apiToken, testOrg, testRepo)
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	t.Logf("registration token: %s...", regToken[:8])

	// --- Start the provider (Register + FetchTask poll) ---
	p, err := giteaprovider.New(giteaprovider.Config{
		InstanceURL: baseURL(),
		Token:       regToken,
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("gitea.New: %v", err)
	}

	events, err := p.Start(ctx, providers.PollConfig{PollInterval: 1})
	if err != nil {
		t.Fatalf("provider.Start: %v", err)
	}
	defer func() {
		if stopErr := p.Stop(context.Background()); stopErr != nil {
			t.Logf("provider.Stop: %v", stopErr)
		}
	}()
	t.Log("provider started, runner registered via ConnectRPC")

	// Give the registration time to propagate before pushing the workflow.
	// The runner must complete at least one FetchTask round-trip for the forge
	// to mark it as "online". FetchTask long-polls for ~5s on the server.
	time.Sleep(10 * time.Second)

	// Push a workflow with EPHEMERD_IMAGE set so we can test FetchJobImage.
	workflow := `name: e2e-test
on: [push]
jobs:
  hello:
    runs-on: ubuntu-latest
    env:
      EPHEMERD_IMAGE: custom/runner:e2e
    steps:
      - run: echo hello from ephemerd gitea e2e
`
	apiPost(t, ctx, apiToken,
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/.gitea/workflows/test.yaml", testOrg, testRepo),
		map[string]any{
			"message": "add test workflow",
			"content": base64.StdEncoding.EncodeToString([]byte(workflow)),
		})
	t.Log("pushed test workflow")

	// --- Wait for the provider to receive a JobEvent via FetchTask ---
	t.Log("waiting for provider to receive task via ConnectRPC FetchTask...")
	var event providers.JobEvent
	select {
	case event = <-events:
		t.Logf("received event: action=%s repo=%s job_id=%d", event.Action, event.Repo, event.JobID)
	case <-time.After(90 * time.Second):
		t.Fatal("timed out waiting for JobEvent from provider")
	}

	if event.Action != "queued" {
		t.Errorf("event.Action = %q, want queued", event.Action)
	}
	if event.JobID == 0 {
		t.Error("event.JobID is 0")
	}
	if event.Repo == "" {
		t.Error("event.Repo is empty")
	}

	// --- Test ClaimJob ---
	claim, err := p.ClaimJob(ctx, &event, "ephemerd-e2e-runner", []string{"linux"})
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if claim.Env["GITEA_INSTANCE_URL"] != baseURL() {
		t.Errorf("GITEA_INSTANCE_URL = %q", claim.Env["GITEA_INSTANCE_URL"])
	}
	if claim.Env["GITEA_RUNNER_TOKEN"] == "" {
		t.Error("GITEA_RUNNER_TOKEN is empty")
	}
	if len(claim.Entrypoint) == 0 {
		t.Error("Entrypoint is empty, expected self-registration command")
	}
	t.Logf("ClaimJob: runner_id=%d entrypoint=%v", claim.RunnerID, claim.Entrypoint)

	// --- Test FetchJobImage ---
	image := p.FetchJobImage(ctx, &event)
	if image != "custom/runner:e2e" {
		t.Errorf("FetchJobImage() = %q, want custom/runner:e2e", image)
	}
	t.Logf("FetchJobImage: %s", image)

	t.Log("all provider assertions passed")
}

// TestGitea_E2E_ForgeRPC validates the low-level forgerpc ConnectRPC client
// against a real Gitea instance (wire format, URL paths, auth headers).
func TestGitea_E2E_ForgeRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gitea forgerpc e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	composeDown(t, composeBin, composeFile)
	composeUp(t, composeBin, composeFile)
	defer composeDown(t, composeBin, composeFile)

	waitForHealth(t, ctx)
	createAdmin(t)
	apiToken := createAPIToken(t, ctx)

	apiPost(t, ctx, apiToken, "/api/v1/orgs", map[string]any{
		"username": testOrg, "visibility": "public",
	})
	apiPost(t, ctx, apiToken, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name": testRepo, "auto_init": true, "default_branch": "main",
	})

	client := forgerpc.NewClient(baseURL(), nil)
	regToken, err := client.RegistrationToken(ctx, apiToken, testOrg, testRepo)
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}

	// --- Register ---
	labels := []string{"ubuntu-latest:docker://node:20-bookworm"}
	runner, err := client.Register(ctx, "ephemerd-e2e-rpc", regToken, "ephemerd/v1", labels)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runner.ID == 0 || runner.UUID == "" || runner.Token == "" {
		t.Fatalf("incomplete runner: %+v", runner)
	}
	t.Logf("registered: id=%d uuid=%s", runner.ID, runner.UUID)

	// --- Declare ---
	if err := client.Declare(ctx, forgerpc.DeclareLabels(labels)); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	t.Log("declared labels")

	// --- FetchTask (no task expected) ---
	result, err := client.FetchTask(ctx, 0)
	if err != nil {
		t.Fatalf("FetchTask: %v", err)
	}
	if result.Task != nil {
		t.Errorf("expected nil task, got id=%d", result.Task.ID)
	}
	t.Logf("FetchTask: no task (expected), tasksVersion=%d", result.TasksVersion)
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
  gitea:
    image: gitea/gitea:latest
    container_name: %s-gitea
    environment:
      - GITEA__security__INSTALL_LOCK=true
      - GITEA__server__ROOT_URL=http://localhost:%s/
      - GITEA__service__DISABLE_REGISTRATION=true
      - GITEA__actions__ENABLED=true
    ports:
      - "%s:3000"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:3000/api/v1/version"]
      interval: 2s
      timeout: 5s
      retries: 30
      start_period: 5s
`, composeProject, giteaPort, giteaPort)
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
			t.Fatal("context cancelled waiting for gitea health")
		}
		resp, err := http.Get(baseURL() + "/api/v1/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Log("Gitea is healthy")
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("timed out waiting for Gitea")
}

func createAdmin(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "--user", "git", composeProject+"-gitea",
		"gitea", "admin", "user", "create", "--admin",
		"--username", adminUser, "--password", adminPass,
		"--email", adminEmail, "--must-change-password=false",
	).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			return
		}
		t.Fatalf("creating admin: %v\n%s", err, out)
	}
}

func createAPIToken(t *testing.T, ctx context.Context) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"name": fmt.Sprintf("e2e-%d", time.Now().UnixNano()), "scopes": []string{"all"},
	})
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
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	tok, _ := result["sha1"].(string)
	if tok == "" {
		t.Fatalf("missing sha1 in token response: %v", result)
	}
	return tok
}

func apiPost(t *testing.T, ctx context.Context, token, path string, body map[string]any) {
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
		return
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, b)
	}
}

// apiPostJSON sends a POST and returns the parsed JSON response.
// Used for endpoints like registration-token that require POST in Gitea 1.26+.
func apiPostJSON(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL()+path, nil)
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, b)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("POST %s: unmarshal: %v", path, err)
	}
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
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("GET %s: unmarshal: %v", path, err)
	}
	return result
}
