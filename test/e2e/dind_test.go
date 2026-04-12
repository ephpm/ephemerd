//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_Dind_WorkflowRun starts ephemerd with dind.enabled, triggers a
// workflow that runs docker commands inside the job, and verifies success.
//
// Requires:
//   - GITHUB_TOKEN with repo + admin:org scope
//   - gh CLI installed
//   - ephemerd binary built (run 'mage build' first)
//   - .github/workflows/dind-test.yml exists in the repo
//
// Run with:
//
//	go test -tags e2e -run TestE2E_Dind_WorkflowRun -v -timeout 10m ./test/e2e/
func TestE2E_Dind_WorkflowRun(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh CLI not found")
	}

	ephemerdBin := findEphemerdBinary(t)
	t.Logf("using ephemerd binary: %s", ephemerdBin)

	dataDir := t.TempDir()

	// Write config with dind enabled
	configPath := filepath.Join(dataDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`[github]
owner = "ephpm"
repos = ["ephemerd"]
poll_interval = "5s"

[dind]
enabled = true

[runner]
max_concurrent = 2
job_timeout = "10m"

[webhook]
tunnel = "none"

[log]
level = "debug"
format = "text"
`), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Start ephemerd as a subprocess
	cmd := exec.CommandContext(ctx, ephemerdBin, "serve",
		"--config", configPath,
		"--data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN="+token)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	t.Log("starting ephemerd serve...")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ephemerd: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	// Wait for ephemerd to be ready
	t.Log("waiting for ephemerd to initialize...")
	time.Sleep(15 * time.Second)

	// Trigger the dind-test workflow via gh CLI
	t.Log("triggering dind-test workflow via gh CLI...")
	ghRun := exec.CommandContext(ctx, "gh", "workflow", "run", "dind-test.yml",
		"--repo", "ephpm/ephemerd",
		"--ref", "main",
	)
	ghRun.Env = append(os.Environ(), "GH_TOKEN="+token)
	if out, err := ghRun.CombinedOutput(); err != nil {
		t.Fatalf("triggering workflow: %v\n%s", err, string(out))
	}

	// Wait a moment for GitHub to create the run
	time.Sleep(5 * time.Second)

	// Poll for workflow completion
	t.Log("polling for workflow run completion...")
	deadline := time.After(6 * time.Minute)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for dind-test workflow to complete")
		case <-ticker.C:
			status, conclusion := getLatestRunStatus(t, ctx, token)
			t.Logf("workflow status: %s, conclusion: %s", status, conclusion)
			if status == "completed" {
				if conclusion != "success" {
					t.Fatalf("dind-test workflow failed: %s", conclusion)
				}
				t.Log("==> dind-test workflow passed!")
				return
			}
		}
	}
}

// getLatestRunStatus returns the status and conclusion of the most recent
// dind-test.yml workflow run.
func getLatestRunStatus(t *testing.T, ctx context.Context, token string) (status, conclusion string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "gh", "run", "list",
		"--repo", "ephpm/ephemerd",
		"--workflow", "dind-test.yml",
		"--limit", "1",
		"--json", "status,conclusion",
	)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("gh run list: %v", err)
		return "unknown", ""
	}

	// Parse minimal JSON: [{"status":"completed","conclusion":"success"}]
	s := strings.TrimSpace(out.String())
	// Quick-and-dirty parse
	if strings.Contains(s, `"completed"`) {
		status = "completed"
	} else if strings.Contains(s, `"in_progress"`) {
		status = "in_progress"
	} else if strings.Contains(s, `"queued"`) {
		status = "queued"
	} else {
		status = "unknown"
	}
	if strings.Contains(s, `"success"`) {
		conclusion = "success"
	} else if strings.Contains(s, `"failure"`) {
		conclusion = "failure"
	}
	return
}

// findEphemerdBinary locates the compiled ephemerd binary.
func findEphemerdBinary(t *testing.T) string {
	t.Helper()
	repoRoot := findRepoRootFrom(t)
	for _, name := range []string{"ephemerd.exe", "ephemerd"} {
		p := filepath.Join(repoRoot, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("ephemerd"); err == nil {
		return p
	}
	t.Fatal("ephemerd binary not found — run 'mage build' first")
	return ""
}

func findRepoRootFrom(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (no go.mod found)")
		}
		dir = parent
	}
}
