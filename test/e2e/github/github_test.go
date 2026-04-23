//go:build e2e

// Package github_test runs end-to-end tests against a fake GitHub REST API.
//
// Unlike the Forgejo/Gitea/GitLab e2e tests that boot real instances via
// docker-compose, this test uses a fake in-process httptest.Server that
// implements the GitHub API endpoints ephemerd needs (PollJobs,
// RegisterJITRunner, RemoveRunner). No GitHub account, no GITHUB_TOKEN,
// no Docker, no containerd required.
//
// This validates ephemerd's GitHub client → provider → scheduler flow
// without requiring external infrastructure.
//
// Run with: mage e2egithub
package github_test

import (
	"context"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/test/ghfake"
)

// TestGitHub_E2E_PollClaimRemove exercises the full job lifecycle:
// queue → discover → claim (JIT register) → remove runner.
func TestGitHub_E2E_PollClaimRemove(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := ghfake.New("testorg")
	defer srv.Close()

	// Queue a job before creating the client (so the client's repos list includes it).
	jobID := srv.QueueJob("myrepo", []string{"self-hosted", "linux", "x64"})
	t.Logf("queued job %d", jobID)

	client := srv.Client()

	// --- Poll for jobs ---
	events, err := client.PollJobs(ctx)
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("PollJobs() returned no events, expected at least 1")
	}

	var found bool
	for _, ev := range events {
		if ev.Job.GetID() == jobID {
			found = true
			if ev.Action != "queued" {
				t.Errorf("event action = %q, want %q", ev.Action, "queued")
			}
			if ev.Repo != "myrepo" {
				t.Errorf("event repo = %q, want %q", ev.Repo, "myrepo")
			}
			break
		}
	}
	if !found {
		t.Fatalf("queued job %d not found in poll results", jobID)
	}
	t.Log("job discovered via PollJobs")

	// --- Register JIT runner ---
	labels := []string{"self-hosted", "linux", "x64"}
	jitConfig, err := client.RegisterJITRunner(ctx, "myrepo", "ephemerd-test-runner", labels)
	if err != nil {
		t.Fatalf("RegisterJITRunner() error: %v", err)
	}

	runnerID := jitConfig.GetRunner().GetID()
	if runnerID == 0 {
		t.Fatal("runner ID is 0")
	}
	encodedConfig := jitConfig.GetEncodedJITConfig()
	if encodedConfig == "" {
		t.Fatal("encoded JIT config is empty")
	}
	t.Logf("registered runner %d with JIT config (%d bytes)", runnerID, len(encodedConfig))

	// --- Remove runner (simulates cleanup after container exits) ---
	if err := client.RemoveRunner(ctx, "myrepo", runnerID); err != nil {
		t.Fatalf("RemoveRunner() error: %v", err)
	}

	if !srv.Removed(runnerID) {
		t.Fatalf("runner %d not marked as removed on fake server", runnerID)
	}
	t.Logf("runner %d removed successfully", runnerID)
}

// TestGitHub_E2E_MultipleJobs verifies that multiple queued jobs are all discovered.
func TestGitHub_E2E_MultipleJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := ghfake.New("testorg")
	defer srv.Close()

	id1 := srv.QueueJob("repo-a", []string{"self-hosted", "linux"})
	id2 := srv.QueueJob("repo-a", []string{"self-hosted", "linux"})
	t.Logf("queued jobs %d, %d", id1, id2)

	client := srv.Client()

	events, err := client.PollJobs(ctx)
	if err != nil {
		t.Fatalf("PollJobs() error: %v", err)
	}

	foundIDs := make(map[int64]bool)
	for _, ev := range events {
		foundIDs[ev.Job.GetID()] = true
	}

	if !foundIDs[id1] {
		t.Errorf("job %d not found in poll results", id1)
	}
	if !foundIDs[id2] {
		t.Errorf("job %d not found in poll results", id2)
	}
	t.Logf("found %d jobs", len(events))
}

// TestGitHub_E2E_WaitForRemoval tests the async removal notification.
func TestGitHub_E2E_WaitForRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := ghfake.New("testorg")
	defer srv.Close()
	srv.QueueJob("myrepo", []string{"self-hosted", "linux"})

	client := srv.Client()

	jitConfig, err := client.RegisterJITRunner(ctx, "myrepo", "runner-wait", []string{"self-hosted"})
	if err != nil {
		t.Fatalf("RegisterJITRunner() error: %v", err)
	}
	runnerID := jitConfig.GetRunner().GetID()

	// Remove in background after a short delay.
	go func() {
		time.Sleep(100 * time.Millisecond)
		client.RemoveRunner(ctx, "myrepo", runnerID)
	}()

	if !srv.WaitForRemoval(runnerID, 5*time.Second) {
		t.Fatal("timed out waiting for runner removal")
	}
	t.Logf("runner %d removal detected via WaitForRemoval", runnerID)
}

// TestGitHub_E2E_FetchJobImage verifies the image lookup returns empty
// when no container.image is set in the workflow.
func TestGitHub_E2E_FetchJobImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv := ghfake.New("testorg")
	defer srv.Close()
	jobID := srv.QueueJob("myrepo", []string{"self-hosted", "linux"})

	client := srv.Client()
	image := client.FetchJobImage(ctx, "myrepo", 1, jobID)
	if image != "" {
		t.Errorf("FetchJobImage() = %q, want empty (no container.image)", image)
	}
}
