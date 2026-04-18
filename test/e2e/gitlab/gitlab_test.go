//go:build e2e && privileged

// Package gitlab runs end-to-end tests against a real GitLab CE instance.
//
// The test boots a GitLab CE container via docker-compose, provisions a test
// project via the GitLab API, registers a gitlab-runner, triggers a CI
// pipeline, and verifies the job completes successfully. Everything is torn
// down at the end regardless of pass/fail.
//
// Run with: mage e2egitlab
// Requires: docker (or podman) with compose support.
package gitlab

import (
	"bytes"
	"context"
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
	gitlabPort     = "8929"
	rootUser       = "root"
	rootPass       = "admin1234Admin!"
	testProject    = "test-project"
	composeProject = "ephemerd-gitlab-e2e"
	healthTimeout  = 5 * time.Minute
	runTimeout     = 5 * time.Minute
)

func baseURL() string { return "http://localhost:" + gitlabPort }

// TestGitLab_E2E boots a GitLab CE instance, creates a test project with a
// CI pipeline, registers a runner, and verifies the pipeline executes.
func TestGitLab_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gitlab e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)

	// Clean up any leftovers from a previous failed run.
	composeDown(t, composeBin, composeFile)
	_ = exec.Command("docker", "rm", "-f", composeProject+"-runner").Run()

	// Boot GitLab CE.
	t.Log("starting GitLab CE via docker-compose (this may take several minutes)")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down GitLab CE")
		composeDown(t, composeBin, composeFile)
	}()

	// Wait for the API to respond.
	waitForHealth(t, ctx)

	// Create API token via personal access tokens API with basic auth.
	token := createAPIToken(t, ctx)
	t.Logf("API token obtained")

	// Create a project under the root user's namespace.
	projResp := apiPost(t, ctx, token, "/api/v4/projects", map[string]any{
		"name":                   testProject,
		"visibility":             "public",
		"initialize_with_readme": true,
	})
	projectID := projResp["id"]
	if projectID == nil {
		t.Fatalf("failed to get project ID from response: %v", projResp)
	}
	// JSON numbers decode as float64.
	projectIDInt := int(projectID.(float64))
	t.Logf("created project %s (id=%d)", testProject, projectIDInt)

	// Create a runner authentication token via the runners API.
	runnerResp := apiPost(t, ctx, token, "/api/v4/user/runners", map[string]any{
		"runner_type":  "project_type",
		"project_id":   projectIDInt,
		"tag_list":     []string{"linux", "docker"},
		"run_untagged": true,
	})
	runnerToken, ok := runnerResp["token"].(string)
	if !ok || runnerToken == "" {
		t.Fatalf("failed to get runner token: %v", runnerResp)
	}
	t.Logf("runner authentication token obtained")

	// Start the runner BEFORE pushing the CI config.
	startRunner(t, ctx, runnerToken)

	// Give the runner a moment to register with GitLab.
	time.Sleep(10 * time.Second)

	// Push a .gitlab-ci.yml via the repository files API.
	ciYAML := `test-job:
  image: node:20-bookworm
  script:
    - echo "hello from ephemerd gitlab e2e"
`
	apiPost(t, ctx, token, fmt.Sprintf("/api/v4/projects/%d/repository/files/.gitlab-ci.yml", projectIDInt), map[string]any{
		"branch":         "main",
		"commit_message": "add CI pipeline",
		"content":        ciYAML,
	})
	t.Logf("pushed .gitlab-ci.yml")

	// Poll for the pipeline to appear and complete.
	t.Log("waiting for pipeline to appear")
	pipelinesURL := fmt.Sprintf("/api/v4/projects/%d/pipelines", projectIDInt)
	var pipelineID float64
	pollAttempt := 0
	pollUntil(t, ctx, 3*time.Second, func() bool {
		pollAttempt++
		pipelines := apiGetArray(t, ctx, token, pipelinesURL)
		if pollAttempt <= 5 || pollAttempt%10 == 0 {
			t.Logf("poll attempt %d: found %d pipelines", pollAttempt, len(pipelines))
		}
		for _, p := range pipelines {
			pipeline, ok := p.(map[string]any)
			if !ok {
				continue
			}
			status, _ := pipeline["status"].(string)
			if status != "" {
				pipelineID = pipeline["id"].(float64)
				t.Logf("found pipeline %v with status %s", pipelineID, status)
				return true
			}
		}
		return false
	})
	t.Logf("pipeline %v found", pipelineID)

	// Poll until the pipeline completes.
	t.Log("waiting for pipeline to complete")
	var finalStatus string
	pipelineURL := fmt.Sprintf("/api/v4/projects/%d/pipelines/%.0f", projectIDInt, pipelineID)
	pollUntil(t, ctx, 5*time.Second, func() bool {
		resp := apiGetSoft(t, ctx, token, pipelineURL)
		if resp == nil {
			return false
		}
		status, _ := resp["status"].(string)
		if status != "" {
			if status != finalStatus {
				t.Logf("pipeline status: %s", status)
				finalStatus = status
			}
		}
		return status == "success" || status == "failed" || status == "canceled"
	})

	if finalStatus != "success" {
		// Dump job logs for debugging.
		jobsURL := fmt.Sprintf("/api/v4/projects/%d/pipelines/%.0f/jobs", projectIDInt, pipelineID)
		jobs := apiGetArray(t, ctx, token, jobsURL)
		for _, j := range jobs {
			job, ok := j.(map[string]any)
			if !ok {
				continue
			}
			jobID, _ := job["id"].(float64)
			jobName, _ := job["name"].(string)
			jobStatus, _ := job["status"].(string)
			t.Logf("job %s (id=%.0f) status=%s", jobName, jobID, jobStatus)
			if jobStatus == "failed" {
				traceURL := fmt.Sprintf("/api/v4/projects/%d/jobs/%.0f/trace", projectIDInt, jobID)
				trace := apiGetRaw(t, ctx, token, traceURL)
				t.Logf("job trace:\n%s", truncate(trace, 2000))
			}
		}
		t.Fatalf("pipeline completed with status %q, expected success", finalStatus)
	}
	t.Logf("pipeline completed successfully")
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
	t.Fatal("docker compose is required for gitlab e2e tests (neither 'docker compose' plugin nor 'docker-compose' binary found)")
	return ""
}

func writeComposeFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")

	// GitLab CE listens on port 80 internally. We map host port 8929 to it.
	// external_url uses the service name so the runner can reach GitLab
	// inside the compose network. The runner connects to http://gitlab:80.
	content := fmt.Sprintf(`services:
  gitlab:
    image: gitlab/gitlab-ce:latest
    container_name: %s-gitlab
    environment:
      GITLAB_ROOT_PASSWORD: "%s"
      GITLAB_OMNIBUS_CONFIG: |
        external_url 'http://gitlab:80'
        gitlab_rails['initial_root_password'] = '%s'
        puma['worker_processes'] = 0
        sidekiq['concurrency'] = 5
        prometheus_monitoring['enable'] = false
        gitlab_rails['env'] = { 'MALLOC_CONF' => 'dirty_decay_ms:1000,muzzy_decay_ms:1000' }
    ports:
      - "%s:80"
    shm_size: "256m"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:80/-/health"]
      interval: 10s
      timeout: 5s
      retries: 30
      start_period: 120s
`, composeProject, rootPass, rootPass, gitlabPort)

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
	// GitLab can take a long time to start; the --wait flag relies on the
	// healthcheck which has a 120s start_period. Give compose plenty of time.
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
	attempt := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("context cancelled waiting for gitlab health")
		}
		attempt++
		resp, err := http.Get(baseURL() + "/-/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Logf("GitLab is healthy after %d attempts (response: %s)", attempt, strings.TrimSpace(string(body)))
				// Additional check: make sure the API is actually serving.
				apiResp, apiErr := http.Get(baseURL() + "/api/v4/version")
				if apiErr == nil {
					apiResp.Body.Close()
					if apiResp.StatusCode == 200 {
						t.Log("GitLab API is responding")
						return
					}
				}
			}
		}
		if attempt%10 == 0 {
			t.Logf("still waiting for GitLab to become healthy (attempt %d)", attempt)
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatal("timed out waiting for GitLab CE to become healthy")
}

func createAPIToken(t *testing.T, ctx context.Context) string {
	t.Helper()
	body := map[string]any{
		"name":   fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
		"scopes": []string{"api", "read_repository", "write_repository"},
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL()+"/api/v4/personal_access_tokens", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(rootUser, rootPass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("creating API token: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("creating API token: status %d: %s", resp.StatusCode, b)
	}

	var result map[string]any
	json.Unmarshal(b, &result)
	token, _ := result["token"].(string)
	if token == "" {
		t.Fatalf("API token response missing token field: %v", result)
	}
	return token
}

func startRunner(t *testing.T, ctx context.Context, runnerToken string) {
	t.Helper()

	containerName := composeProject + "-runner"
	runnerImage := "gitlab/gitlab-runner:latest"
	network := composeProject + "_default"

	t.Logf("starting gitlab-runner container")

	// The runner registers with GitLab, then starts to poll for jobs.
	registerAndStart := fmt.Sprintf(
		"gitlab-runner register --non-interactive --url http://gitlab:80 --token %s --executor docker --docker-image node:20-bookworm --name e2e-runner && gitlab-runner run",
		runnerToken,
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
		t.Fatalf("starting gitlab-runner: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		// Dump runner logs for debugging if the test failed.
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "50", containerName).CombinedOutput()
			t.Logf("gitlab-runner logs:\n%s", string(logs))
		}
		exec.Command("docker", "rm", "-f", containerName).Run()
	})

	t.Logf("gitlab-runner container started: %s", strings.TrimSpace(string(out)))
}

// --- API helpers ---

func apiPost(t *testing.T, ctx context.Context, token, path string, body map[string]any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL()+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 409 || resp.StatusCode == 422 {
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

func apiGet(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("PRIVATE-TOKEN", token)

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

// apiGetArray is like apiGet but for endpoints that return a JSON array.
func apiGetArray(t *testing.T, ctx context.Context, token, path string) []any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("GET %s: %v", path, err)
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Logf("GET %s: status %d: %s", path, resp.StatusCode, b)
		return nil
	}
	var result []any
	json.Unmarshal(b, &result)
	return result
}

// apiGetSoft is like apiGet but returns nil instead of failing on errors.
// Used for polling endpoints that may not have data yet.
func apiGetSoft(t *testing.T, ctx context.Context, token, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("PRIVATE-TOKEN", token)

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
		t.Logf("GET %s: status %d: %s", path, resp.StatusCode, b)
		return nil
	}
	var result map[string]any
	json.Unmarshal(b, &result)
	return result
}

// apiGetRaw returns the raw response body as a string.
func apiGetRaw(t *testing.T, ctx context.Context, token, path string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL()+path, nil)
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
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
