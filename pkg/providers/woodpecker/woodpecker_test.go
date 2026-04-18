package woodpecker

import (
	"context"
	"log/slog"
	"testing"

	"github.com/ephpm/ephemerd/pkg/providers"
)

func TestNew_RequiresServerURL(t *testing.T) {
	_, err := New(Config{AgentSecret: "secret", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing server_url")
	}
}

func TestNew_RequiresAgentSecret(t *testing.T) {
	_, err := New(Config{ServerURL: "woodpecker:9000", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing agent_secret")
	}
}

func TestNew_Valid(t *testing.T) {
	p, err := New(Config{
		ServerURL:   "woodpecker:9000",
		AgentSecret: "secret",
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
	if p.Name() != "woodpecker" {
		t.Errorf("Name() = %q, want %q", p.Name(), "woodpecker")
	}
}

func TestDefaultImage(t *testing.T) {
	p := &Provider{}
	want := "docker.io/woodpeckerci/woodpecker-agent:latest"
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
			ServerURL:   "woodpecker.example.com:9000",
			AgentSecret: "my-secret",
		},
	}

	event := &providers.JobEvent{Repo: "myorg/myrepo", JobID: 1}
	claim, err := p.ClaimJob(context.Background(), event, "agent-1", []string{"linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if claim.RunnerName != "agent-1" {
		t.Errorf("RunnerName = %q, want %q", claim.RunnerName, "agent-1")
	}
	if claim.Repo != "myorg/myrepo" {
		t.Errorf("Repo = %q, want %q", claim.Repo, "myorg/myrepo")
	}

	wantEnv := map[string]string{
		"WOODPECKER_SERVER":       "woodpecker.example.com:9000",
		"WOODPECKER_AGENT_SECRET": "my-secret",
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

func TestFetchJobImage_Empty(t *testing.T) {
	p := &Provider{}
	if img := p.FetchJobImage(context.Background(), &providers.JobEvent{}); img != "" {
		t.Errorf("FetchJobImage() = %q, want empty", img)
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
