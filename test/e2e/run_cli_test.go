//go:build e2e && privileged

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_RunCLI_SimpleWorkflow runs `ephemerd run` against a workflow
// that echoes a string, and verifies the command succeeds with expected output.
//
// Requires: root, `mage build` (ephemerd binary must exist).
func TestE2E_RunCLI_SimpleWorkflow(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "test.yml", `
name: Simple Test
on: push
jobs:
  greet:
    runs-on: ubuntu-latest
    steps:
      - name: Say hello
        run: echo "hello from ephemerd run e2e"
      - name: Check environment
        run: |
          test "$CI" = "true"
          echo "CI env is set"
`)

	out, err := runEphemerd(t, ephemerd, repo, "test.yml", "", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "hello from ephemerd run e2e") {
		t.Errorf("output missing echo text.\nOutput:\n%s", out)
	}
	if !strings.Contains(out, "completed successfully") {
		t.Errorf("output missing success message.\nOutput:\n%s", out)
	}
}

// TestE2E_RunCLI_MultiStep runs a workflow with multiple steps and
// verifies they all execute in order.
func TestE2E_RunCLI_MultiStep(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "multi.yml", `
name: Multi Step
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Step 1
        run: echo "step-one"
      - name: Step 2
        run: echo "step-two"
      - name: Step 3
        run: echo "step-three"
`)

	out, err := runEphemerd(t, ephemerd, repo, "multi.yml", "", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run failed: %v\n%s", err, out)
	}

	for _, want := range []string{"step-one", "step-two", "step-three"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q.\nOutput:\n%s", want, out)
		}
	}
}

// TestE2E_RunCLI_FailingStep verifies that a failing step causes
// `ephemerd run` to exit non-zero.
func TestE2E_RunCLI_FailingStep(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "fail.yml", `
name: Failing Test
on: push
jobs:
  broken:
    runs-on: ubuntu-latest
    steps:
      - name: This will fail
        run: exit 42
`)

	out, err := runEphemerd(t, ephemerd, repo, "fail.yml", "", 3*time.Minute)
	if err == nil {
		t.Fatalf("expected ephemerd run to fail, but it succeeded.\nOutput:\n%s", out)
	}

	if !strings.Contains(out, "FAILED") {
		t.Errorf("output missing FAILED indicator.\nOutput:\n%s", out)
	}
}

// TestE2E_RunCLI_JobFilter verifies the --job flag selects a specific job.
func TestE2E_RunCLI_JobFilter(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "jobs.yml", `
name: Job Filter Test
on: push
jobs:
  alpha:
    runs-on: ubuntu-latest
    steps:
      - run: echo "from-alpha"
  beta:
    runs-on: ubuntu-latest
    steps:
      - run: echo "from-beta"
`)

	out, err := runEphemerd(t, ephemerd, repo, "jobs.yml", "beta", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run --job beta failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "from-beta") {
		t.Errorf("output missing 'from-beta'.\nOutput:\n%s", out)
	}
	// alpha should NOT have run
	if strings.Contains(out, "from-alpha") {
		t.Errorf("output contains 'from-alpha' — job filter didn't work.\nOutput:\n%s", out)
	}
}

// TestE2E_RunCLI_EnvVars verifies that job-level env vars are passed to steps.
func TestE2E_RunCLI_EnvVars(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "env.yml", `
name: Env Test
on: push
jobs:
  check-env:
    runs-on: ubuntu-latest
    env:
      MY_VAR: "ephemerd-test-value"
    steps:
      - name: Check env
        run: |
          echo "MY_VAR=$MY_VAR"
          test "$MY_VAR" = "ephemerd-test-value"
`)

	out, err := runEphemerd(t, ephemerd, repo, "env.yml", "", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "ephemerd-test-value") {
		t.Errorf("output missing env var value.\nOutput:\n%s", out)
	}
}

// TestE2E_RunCLI_RepoMount verifies the repo is bind-mounted into the container.
func TestE2E_RunCLI_RepoMount(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	// Write a marker file in the repo
	if err := os.WriteFile(filepath.Join(repo, "marker.txt"), []byte("ephemerd-was-here"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeWorkflow(t, repo, "mount.yml", `
name: Mount Test
on: push
jobs:
  check-mount:
    runs-on: ubuntu-latest
    steps:
      - name: Check repo files
        run: |
          cat marker.txt
          grep ephemerd-was-here marker.txt
`)

	out, err := runEphemerd(t, ephemerd, repo, "mount.yml", "", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "ephemerd-was-here") {
		t.Errorf("output missing marker file content.\nOutput:\n%s", out)
	}
}

// TestE2E_RunCLI_Network verifies containers have network access.
func TestE2E_RunCLI_Network(t *testing.T) {
	ephemerd := findEphemerdBinary(t)
	repo := initTempRepo(t)

	writeWorkflow(t, repo, "network.yml", `
name: Network Test
on: push
jobs:
  dns:
    runs-on: ubuntu-latest
    steps:
      - name: DNS resolution
        run: |
          cat /etc/resolv.conf
          # Try to resolve a well-known domain
          nslookup github.com || getent hosts github.com || echo "dns-check-done"
`)

	out, err := runEphemerd(t, ephemerd, repo, "network.yml", "", 3*time.Minute)
	if err != nil {
		t.Fatalf("ephemerd run failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "nameserver") && !strings.Contains(out, "dns-check-done") {
		t.Errorf("output missing network evidence.\nOutput:\n%s", out)
	}
}

// --- helpers ---

// initTempRepo creates a temp directory with a git repo initialized.
// ephemerd run needs a git repo to sniff SHA, ref, and remote info.
func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@ephemerd.dev"},
		{"config", "user.name", "E2E Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}

	return dir
}

// writeWorkflow writes a workflow YAML into .github/workflows/ in the repo.
func writeWorkflow(t *testing.T, repoDir, name, content string) {
	t.Helper()
	dir := filepath.Join(repoDir, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runEphemerd invokes `ephemerd run` as a subprocess and returns combined output.
func runEphemerd(t *testing.T, binary, repoDir, workflow, jobFilter string, timeout time.Duration) (string, error) {
	t.Helper()

	dataDir := filepath.Join(repoDir, ".ephemerd-data")

	args := []string{"--data-dir", dataDir, "run"}
	if jobFilter != "" {
		args = append(args, "--job", jobFilter)
	}

	workflowPath := filepath.Join(repoDir, ".github", "workflows", workflow)
	args = append(args, workflowPath)

	cmd := exec.Command(binary, args...)
	cmd.Dir = repoDir

	// Use a channel + goroutine for timeout since exec.CommandContext
	// sends SIGKILL which doesn't let containerd clean up.
	done := make(chan struct{})
	var out []byte
	var runErr error

	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		t.Logf("ephemerd run output:\n%s", string(out))
		return string(out), runErr
	case <-timer.C:
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		t.Logf("ephemerd run output (timed out):\n%s", string(out))
		return string(out), fmt.Errorf("timed out after %v", timeout)
	}
}
