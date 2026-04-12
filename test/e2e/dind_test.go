//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

[vm.linux]
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

	// Wait for ephemerd + WSL Linux VM + containerd + dispatch server to be ready.
	// The WSL boot takes ~20-30s on first run.
	t.Log("waiting for ephemerd + WSL to initialize...")
	time.Sleep(60 * time.Second)

	// Record time before triggering so we can find the right run
	triggerTime := time.Now()

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

	// Wait for GitHub to create the run
	time.Sleep(10 * time.Second)

	// Poll for workflow completion — only consider runs created after triggerTime
	t.Log("polling for workflow run completion...")
	deadline := time.After(6 * time.Minute)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for dind-test workflow to complete")
		case <-ticker.C:
			status, conclusion := getRunStatusAfter(t, ctx, token, triggerTime)
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

// getRunStatusAfter returns the status and conclusion of the most recent
// dind-test.yml workflow run created after the given time.
func getRunStatusAfter(t *testing.T, ctx context.Context, token string, after time.Time) (status, conclusion string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "gh", "run", "list",
		"--repo", "ephpm/ephemerd",
		"--workflow", "dind-test.yml",
		"--limit", "5",
		"--json", "status,conclusion,createdAt",
	)
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("gh run list: %v", err)
		return "unknown", ""
	}

	// Parse JSON array to find a run created after our trigger time
	type runEntry struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		CreatedAt  string `json:"createdAt"`
	}
	var runs []runEntry
	if err := json.Unmarshal(out.Bytes(), &runs); err != nil {
		t.Logf("parsing gh output: %v", err)
		return "unknown", ""
	}

	for _, r := range runs {
		created, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			continue
		}
		if created.After(after) {
			return r.Status, r.Conclusion
		}
	}

	return "pending", "" // run not yet visible
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
