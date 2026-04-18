package forgejo

import (
	"context"
	"log/slog"
	"testing"

	"github.com/ephpm/ephemerd/pkg/providers"
)

func TestNew_RequiresInstanceURL(t *testing.T) {
	_, err := New(Config{Token: "tok", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing instance_url")
	}
}

func TestNew_RequiresToken(t *testing.T) {
	_, err := New(Config{InstanceURL: "https://codeberg.org", Log: slog.Default()})
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNew_Valid(t *testing.T) {
	p, err := New(Config{
		InstanceURL: "https://codeberg.org",
		Token:       "tok",
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
	if p.Name() != "forgejo" {
		t.Errorf("Name() = %q, want %q", p.Name(), "forgejo")
	}
}

func TestDefaultImage(t *testing.T) {
	p := &Provider{}
	want := "data.forgejo.org/forgejo/runner:12"
	if p.DefaultImage() != want {
		t.Errorf("DefaultImage() = %q, want %q", p.DefaultImage(), want)
	}
}

func TestDefaultJobImage_Default(t *testing.T) {
	p := &Provider{}
	want := "docker.io/gitea/runner-images:ubuntu-24.04"
	if p.DefaultJobImage() != want {
		t.Errorf("DefaultJobImage() = %q, want %q", p.DefaultJobImage(), want)
	}
}

func TestDefaultJobImage_Override(t *testing.T) {
	p := &Provider{cfg: Config{JobImage: "custom/image:latest"}}
	if p.DefaultJobImage() != "custom/image:latest" {
		t.Errorf("DefaultJobImage() = %q, want %q", p.DefaultJobImage(), "custom/image:latest")
	}
}

func TestClaimJob_EnvVars(t *testing.T) {
	p := &Provider{
		cfg: Config{
			InstanceURL: "https://codeberg.org",
			Token:       "reg-token",
		},
		runnerID:    42,
		runnerToken: "persistent-token",
		runnerUUID:  "uuid-abc",
	}

	event := &providers.JobEvent{Repo: "myorg/myrepo", JobID: 1}
	claim, err := p.ClaimJob(context.Background(), event, "runner-1", []string{"linux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if claim.RunnerID != 42 {
		t.Errorf("RunnerID = %d, want 42", claim.RunnerID)
	}
	if claim.RunnerName != "runner-1" {
		t.Errorf("RunnerName = %q, want %q", claim.RunnerName, "runner-1")
	}
	if claim.Repo != "myorg/myrepo" {
		t.Errorf("Repo = %q, want %q", claim.Repo, "myorg/myrepo")
	}

	wantEnv := map[string]string{
		"FORGEJO_INSTANCE_URL": "https://codeberg.org",
		"FORGEJO_RUNNER_TOKEN": "persistent-token",
		"FORGEJO_RUNNER_UUID":  "uuid-abc",
	}
	for k, v := range wantEnv {
		if claim.Env[k] != v {
			t.Errorf("Env[%q] = %q, want %q", k, claim.Env[k], v)
		}
	}
	if claim.RunnerConfig != "" {
		t.Errorf("RunnerConfig = %q, want empty", claim.RunnerConfig)
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
