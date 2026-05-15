//go:build e2e && privileged

// Package ephemerd_runner_gitea_test runs end-to-end tests for the custom ephemerd-runner-gitea
// binary against a real Gitea instance.
//
// The test cross-compiles the ephemerd-runner-gitea for Linux, builds a Docker image
// containing it, boots Gitea via docker-compose, pushes a workflow, and
// verifies the runner executes the job to completion.
//
// Run with: mage e2egitearunner
// Requires: docker (or podman) with compose support, Go toolchain for cross-compile.
package ephemerd_runner_gitea_test

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
	"strings"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/forgerpc"
)

const (
	giteaPort      = "3011"
	adminUser      = "e2eadmin"
	adminPass      = "admin1234Admin!"
	adminEmail     = "admin@localhost"
	testOrg        = "runner-org"
	testRepo       = "runner-repo"
	composeProject = "ephemerd-gitearunner-e2e"
	runnerImage    = "ephemerd-ephemerd-runner-gitea:e2e"
	healthTimeout  = 60 * time.Second
	runTimeout     = 4 * time.Minute
)

func baseURL() string { return "http://localhost:" + giteaPort }

// TestGiteaRunner_E2E builds a Docker image with our custom ephemerd-runner-gitea binary,
// boots Gitea, registers the runner, pushes a workflow, and verifies the job
// completes successfully.
func TestGiteaRunner_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ephemerd-runner-gitea e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// --- Step 1: Cross-compile ephemerd-runner-gitea for Linux ---
	buildDir := t.TempDir()
	binaryPath := filepath.Join(buildDir, "ephemerd-runner-gitea")
	t.Log("cross-compiling ephemerd-runner-gitea for linux/amd64")
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./cmd/ephemerd-runner-gitea/")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	buildCmd.Dir = repoRoot(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile ephemerd-runner-gitea: %v\n%s", err, out)
	}
	t.Log("binary built")

	// --- Step 2: Build Docker image ---
	dockerfile := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(`FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates curl git && rm -rf /var/lib/apt/lists/*
COPY ephemerd-runner-gitea /usr/local/bin/ephemerd-runner-gitea
RUN chmod +x /usr/local/bin/ephemerd-runner-gitea
`), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	t.Log("building Docker image")
	imgCmd := exec.CommandContext(ctx, "docker", "build", "-t", runnerImage, buildDir)
	imgCmd.Stdout = os.Stdout
	imgCmd.Stderr = os.Stderr
	if err := imgCmd.Run(); err != nil {
		t.Fatalf("docker build: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rmi", runnerImage).Run()
	})

	// --- Step 3: Boot Gitea ---
	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)
	composeDown(t, composeBin, composeFile)

	t.Log("starting Gitea via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down")
		exec.Command("docker", "rm", "-f", composeProject+"-runner").Run()
		composeDown(t, composeBin, composeFile)
	}()

	waitForHealth(t, ctx)
	createAdmin(t)
	apiToken := createAPIToken(t, ctx)
	t.Log("API ready")

	apiPost(t, ctx, apiToken, "/api/v1/orgs", map[string]any{
		"username": testOrg, "visibility": "public",
	})
	apiPost(t, ctx, apiToken, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name": testRepo, "auto_init": true, "default_branch": "main",
	})
	t.Logf("created %s/%s", testOrg, testRepo)

	// --- Step 4: Get registration token ---
	client := forgerpc.NewClient(baseURL(), nil)
	regToken, err := client.RegistrationToken(ctx, apiToken, testOrg, testRepo)
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	t.Logf("registration token: %s...", regToken[:8])

	// --- Step 5: Start runner container ---
	containerName := composeProject + "-runner"
	network := composeProject + "_default"

	runArgs := []string{
		"run", "-d",
		"--name", containerName,
		"--network", network,
		runnerImage,
		"ephemerd-runner-gitea",
		"--instance", "http://gitea:3000",
		"--token", regToken,
		"--label", "ubuntu-latest",
	}

	t.Log("starting ephemerd-runner-gitea container")
	if out, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if out, err := exec.Command("docker", "logs", containerName).CombinedOutput(); err == nil {
				t.Logf("--- ephemerd-runner-gitea logs ---\n%s", string(out))
			}
		}
		exec.Command("docker", "rm", "-f", containerName).Run()
	})

	time.Sleep(5 * time.Second)

	// --- Step 6: Push workflow ---
	workflow := `name: ephemerd-runner-gitea-e2e
on: [push]
jobs:
  hello:
    runs-on: ubuntu-latest
    steps:
      - name: Verify runner
        run: echo "Hello from ephemerd-runner-gitea e2e"
      - name: Check environment
        run: |
          echo "CI=$CI"
          echo "GITHUB_REPOSITORY=$GITHUB_REPOSITORY"
          test "$CI" = "true"
`
	apiPost(t, ctx, apiToken,
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/.gitea/workflows/test.yaml", testOrg, testRepo),
		map[string]any{
			"message": "add test workflow",
			"content": base64.StdEncoding.EncodeToString([]byte(workflow)),
		})
	t.Log("pushed workflow")

	// --- Step 7: Poll for completion ---
	t.Log("waiting for workflow to complete...")
	tasksURL := fmt.Sprintf("/api/v1/repos/%s/%s/actions/tasks", testOrg, testRepo)

	var finalStatus string
	pollUntil(t, ctx, 3*time.Second, func() bool {
		resp := apiGetSoft(t, ctx, apiToken, tasksURL)
		if resp == nil {
			return false
		}
		runs, _ := resp["workflow_runs"].([]any)
		for _, r := range runs {
			task, ok := r.(map[string]any)
			if !ok {
				continue
			}
			status, _ := task["status"].(string)
			t.Logf("  task status: %s", status)
			if status == "success" || status == "failure" || status == "cancelled" {
				finalStatus = status
				return true
			}
		}
		return false
	})

	if finalStatus != "success" {
		if out, err := exec.Command("docker", "logs", containerName).CombinedOutput(); err == nil {
			t.Logf("--- ephemerd-runner-gitea logs ---\n%s", string(out))
		}
		t.Fatalf("workflow completed with status %q, expected success", finalStatus)
	}
	t.Log("workflow completed successfully with custom ephemerd-runner-gitea")
}

// --- Helpers ---

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

func findCompose(t *testing.T) string {
	t.Helper()
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err == nil {
		t.Logf("using docker compose v2: %s", strings.TrimSpace(string(out)))
		return "docker compose"
	}
	if path, err := exec.LookPath("docker-compose"); err == nil {
		return path
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
		t.Fatalf("write compose: %v", err)
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
		t.Logf("docker-compose down: %v", err)
	}
}

func waitForHealth(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatal("context cancelled waiting for health")
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
	if err != nil && !strings.Contains(string(out), "already exists") {
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
		t.Fatalf("creating token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("creating token: %d: %s", resp.StatusCode, b)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tok, _ := result["sha1"].(string)
	if tok == "" {
		t.Fatalf("missing sha1: %v", result)
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
	if resp.StatusCode >= 400 && resp.StatusCode != 409 && resp.StatusCode != 422 {
		t.Fatalf("POST %s: %d: %s", path, resp.StatusCode, b)
	}
}

func apiGetSoft(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		return nil
	}
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
			t.Fatal("context cancelled while polling")
		case <-time.After(interval):
		}
	}
	t.Fatal("timed out polling")
}
