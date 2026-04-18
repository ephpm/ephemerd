package gitlab

import (
	"context"
	"log/slog"
	"testing"

	"github.com/ephpm/ephemerd/pkg/providers"
)

func TestNew_RequiresInstanceURL(t *testing.T) {
	_, err := New(Config{Token: "glrt-xxx", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing instance_url")
	}
}

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(Config{InstanceURL: "https://gitlab.com", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNew_Valid(t *testing.T) {
	p, err := New(Config{
		InstanceURL: "https://gitlab.com",
		Token:       "glrt-xxx",
		Tags:        []string{"linux", "docker"},
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestName(t *testing.T) {
	p := &Provider{}
	if p.Name() != "gitlab" {
		t.Errorf("Name() = %q, want %q", p.Name(), "gitlab")
	}
}

func TestDefaultImage(t *testing.T) {
	p := &Provider{}
	want := "ghcr.io/ephpm/runner-gitlab:latest"
	if p.DefaultImage() != want {
		t.Errorf("DefaultImage() = %q, want %q", p.DefaultImage(), want)
	}
}

func TestDefaultJobImage_Empty(t *testing.T) {
	p := &Provider{}
	if p.DefaultJobImage() != "" {
		t.Errorf("DefaultJobImage() = %q, want empty", p.DefaultJobImage())
	}
}

func TestClaimJob_EnvVars(t *testing.T) {
	p := &Provider{
		cfg: Config{
			InstanceURL: "https://gitlab.com",
			Token:       "glrt-xxx",
		},
		runnerID:    77,
		runnerToken: "runner-auth-token",
	}

	event := &providers.JobEvent{Repo: "group/project", JobID: 1}
	claim, err := p.ClaimJob(context.Background(), event, "runner-1", []string{"linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if claim.RunnerID != 77 {
		t.Errorf("RunnerID = %d, want 77", claim.RunnerID)
	}
	if claim.RunnerName != "runner-1" {
		t.Errorf("RunnerName = %q, want %q", claim.RunnerName, "runner-1")
	}
	if claim.Repo != "group/project" {
		t.Errorf("Repo = %q, want %q", claim.Repo, "group/project")
	}

	wantEnv := map[string]string{
		"CI_SERVER_URL":   "https://gitlab.com",
		"CI_RUNNER_TOKEN": "runner-auth-token",
	}
	for k, v := range wantEnv {
		if claim.Env[k] != v {
			t.Errorf("Env[%q] = %q, want %q", k, claim.Env[k], v)
		}
	}
	if len(claim.Env) != 2 {
		t.Errorf("Env has %d keys, want 2", len(claim.Env))
	}
}

func TestReleaseJob_Noop(t *testing.T) {
	p := &Provider{}
	if err := p.ReleaseJob(context.Background(), &providers.Claim{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchJobImage_Stub(t *testing.T) {
	p := &Provider{}
	if img := p.FetchJobImage(context.Background(), &providers.JobEvent{}); img != "" {
		t.Errorf("FetchJobImage() = %q, want empty (stub)", img)
	}
}

func TestStop_NilCancel(t *testing.T) {
	p := &Provider{}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() with nil cancel: %v", err)
	}
}

func TestStop_WithCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &Provider{cancel: cancel}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}
