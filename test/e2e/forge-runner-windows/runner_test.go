//go:build e2e && privileged && windows

// Package forge_runner_windows_test runs an end-to-end test of the custom
// forge-runner binary inside a Windows Hyper-V isolated container against
// a real Forgejo instance.
//
// The test:
//  1. Builds forge-runner.exe natively
//  2. Boots Forgejo via docker-compose (Linux container in WSL2)
//  3. Starts ephemerd's embedded containerd
//  4. Pulls a Windows Server Core image matching the host OS
//  5. Creates a Hyper-V container with forge-runner.exe mounted in
//  6. The runner registers, fetches a task, executes it with pwsh/cmd
//  7. Verifies the workflow completes with status=success
//
// Requirements:
//   - Hyper-V enabled
//   - Docker Desktop (for Forgejo Linux container)
//   - containerd accessible (ephemerd's embedded or standalone)
//
// Run with: go test -tags "e2e,privileged" -timeout 10m -v ./test/e2e/forge-runner-windows/
package forge_runner_windows_test

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

	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/forgerpc"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runtime"

	"github.com/containerd/containerd/v2/client"
)

const (
	forgejoPort    = "3012"
	adminUser      = "e2eadmin"
	adminPass      = "admin1234Admin!"
	adminEmail     = "admin@localhost"
	testOrg        = "winrunner-org"
	testRepo       = "winrunner-repo"
	composeProject = "ephemerd-winrunner-e2e"
	healthTimeout  = 60 * time.Second
	runTimeout     = 5 * time.Minute
)

func baseURL() string { return "http://localhost:" + forgejoPort }

func TestForgeRunnerWindows_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping windows forge-runner e2e in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	log := slog.Default()
	dataDir := t.TempDir()

	// --- Step 1: Build forge-runner.exe natively ---
	runnerDir := filepath.Join(dataDir, "forge-runner")
	if err := os.MkdirAll(runnerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binaryPath := filepath.Join(runnerDir, "forge-runner.exe")
	t.Log("building forge-runner.exe")
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./cmd/forge-runner/")
	buildCmd.Dir = repoRoot(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build forge-runner.exe: %v\n%s", err, out)
	}
	t.Logf("built %s", binaryPath)

	// --- Step 2: Boot Forgejo (Linux container via Docker Desktop) ---
	composeBin := findCompose(t)
	composeFile := writeComposeFile(t)
	composeDown(t, composeBin, composeFile)
	t.Log("starting Forgejo via docker-compose")
	composeUp(t, composeBin, composeFile)
	defer func() {
		t.Log("tearing down Forgejo")
		composeDown(t, composeBin, composeFile)
	}()
	waitForHealth(t, ctx)
	createAdmin(t)
	apiToken := createAPIToken(t, ctx)
	t.Log("Forgejo API ready")

	// Create org + repo
	apiPost(t, ctx, apiToken, "/api/v1/orgs", map[string]any{
		"username": testOrg, "visibility": "public",
	})
	apiPost(t, ctx, apiToken, fmt.Sprintf("/api/v1/orgs/%s/repos", testOrg), map[string]any{
		"name": testRepo, "auto_init": true, "default_branch": "main",
	})
	t.Logf("created %s/%s", testOrg, testRepo)

	// Get registration token
	rpcClient := forgerpc.NewClient(baseURL(), nil)
	regToken, err := rpcClient.RegistrationToken(ctx, apiToken, testOrg, testRepo)
	if err != nil {
		t.Fatalf("RegistrationToken: %v", err)
	}
	t.Logf("registration token: %s...", regToken[:8])

	// --- Step 3: Start containerd ---
	t.Log("starting embedded containerd")
	ctrdClient, cleanup, err := startContainerd(ctx, dataDir, log)
	if err != nil {
		t.Fatalf("start containerd: %v", err)
	}
	defer cleanup()
	t.Log("containerd ready")

	// --- Step 4: Create runtime and pull Windows image ---
	net, err := networking.New(networking.Config{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		t.Fatalf("networking: %v", err)
	}
	defer net.Cleanup()

	rt, err := runtime.New(runtime.Config{
		Client:      ctrdClient,
		LogDir:      filepath.Join(dataDir, "logs"),
		DataDir:     dataDir,
		DindEnabled: false,
		Network:     net,
		Log:         log,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}

	// Pull will use the auto-detected Windows Server Core image
	t.Log("pulling Windows Server Core image (may take a while on first run)...")
	if err := rt.PullImage(ctx, ""); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	t.Log("image ready")

	// --- Step 5: Push workflow with pwsh steps ---
	// The runner needs to register before the push (Forgejo won't create
	// runs without a matching runner). Register via forgerpc first.
	runner, err := rpcClient.Register(ctx, "win-e2e-runner", regToken, "ephemerd/v1",
		[]string{"windows-latest"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := rpcClient.Declare(ctx, forgerpc.DeclareLabels([]string{"windows-latest"})); err != nil {
		t.Logf("declare: %v (non-fatal)", err)
	}
	t.Logf("registered runner id=%d uuid=%s", runner.ID, runner.UUID)

	time.Sleep(10 * time.Second) // let registration propagate

	workflow := `name: windows-forge-runner-e2e
on: [push]
jobs:
  hello:
    runs-on: windows-latest
    steps:
      - name: Verify Windows runner
        run: |
          Write-Host "Hello from forge-runner on Windows!"
          Write-Host "OS: $env:OS"
          Write-Host "CI: $env:CI"
          if ($env:CI -ne "true") { exit 1 }
        shell: pwsh
`
	apiPost(t, ctx, apiToken,
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/.forgejo/workflows/test.yaml", testOrg, testRepo),
		map[string]any{
			"message": "add windows test workflow",
			"content": base64.StdEncoding.EncodeToString([]byte(workflow)),
		})
	t.Log("pushed Windows workflow")

	// --- Step 6: Create Hyper-V container with forge-runner mounted ---
	// The forge-runner.exe connects to Forgejo and picks up the task.
	// Use the host's IP (not localhost) since the container runs in Hyper-V.
	forgejoURL := fmt.Sprintf("http://host.docker.internal:%s", forgejoPort)

	t.Log("creating Windows container with forge-runner.exe")
	env, err := rt.Create(ctx, runtime.CreateConfig{
		ID: "forge-runner-win-e2e",
		Env: map[string]string{
			"FORGEJO_INSTANCE_URL": forgejoURL,
			"FORGEJO_REG_TOKEN":    regToken,
		},
		Entrypoint: []string{
			`C:\forge-runner\forge-runner.exe`,
			"--instance", forgejoURL,
			"--token", regToken,
			"--label", "windows-latest",
		},
		Mounts: map[string]string{
			runnerDir: `C:\forge-runner`,
		},
	})
	if err != nil {
		t.Fatalf("Runtime.Create: %v", err)
	}
	defer func() {
		if destroyErr := rt.Destroy(context.Background(), env); destroyErr != nil {
			t.Logf("destroy: %v", destroyErr)
		}
	}()
	t.Log("container created, waiting for job execution...")

	// --- Step 7: Wait for completion ---
	exitCode, waitErr := rt.Wait(ctx, env)
	if waitErr != nil {
		t.Logf("wait error: %v", waitErr)
	}
	t.Logf("container exited with code %d", exitCode)

	// --- Step 8: Verify via REST API ---
	t.Log("checking workflow status via REST API...")
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
		t.Fatalf("workflow completed with status %q, expected success", finalStatus)
	}
	t.Log("Windows forge-runner e2e passed!")
}

// startContainerd boots ephemerd's embedded containerd and returns a client.
func startContainerd(_ context.Context, dataDir string, log *slog.Logger) (*client.Client, func(), error) {
	srv, err := containerdpkg.New(containerdpkg.Config{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("containerd: %w", err)
	}

	c := srv.Client()
	cleanup := func() {
		if closeErr := c.Close(); closeErr != nil {
			log.Warn("close containerd client", "error", closeErr)
		}
		srv.Stop()
	}
	return c, cleanup, nil
}

// --- Helpers (same patterns as Linux e2e) ---

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
			t.Fatal("could not find repo root")
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
				t.Log("Forgejo is healthy")
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("timed out waiting for Forgejo")
}

func createAdmin(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "--user", "git", composeProject+"-forgejo",
		"forgejo", "admin", "user", "create", "--admin",
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
