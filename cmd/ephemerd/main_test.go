package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ephpm/ephemerd/pkg/config"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestInitProviders_NoneConfigured pins the "no providers" error path. Without
// any provider section set, initProviders must refuse to start.
func TestInitProviders_NoneConfigured(t *testing.T) {
	cfg := &config.Config{}
	_, _, err := initProviders(cfg, quietLog())
	if err == nil {
		t.Fatal("expected error when no providers configured")
	}
	if !strings.Contains(err.Error(), "no providers configured") {
		t.Errorf("err = %v, want 'no providers configured'", err)
	}
}

// TestInitProviders_GitHubPATOnly verifies a token-only GitHub config produces
// exactly one provider.
func TestInitProviders_GitHubPATOnly(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			Token: "ghp_test",
			Owner: "ephpm",
		},
	}
	provs, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}
	t.Cleanup(cleanup)

	if len(provs) != 1 {
		t.Fatalf("provider count = %d, want 1", len(provs))
	}
	if provs[0].Name() != "github" {
		t.Errorf("provider name = %q, want github", provs[0].Name())
	}
}

// TestInitProviders_ForgejoOnly verifies the forgejo path. Side-effect:
// initProviders should auto-enable cfg.Dind.Enabled because forgejo uses
// the Docker API.
func TestInitProviders_ForgejoOnly(t *testing.T) {
	cfg := &config.Config{
		Forgejo: config.ForgejoConfig{
			InstanceURL: "https://codeberg.org",
			Token:       "fake-token",
		},
	}
	provs, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}
	t.Cleanup(cleanup)

	if len(provs) != 1 {
		t.Fatalf("provider count = %d, want 1", len(provs))
	}
	if provs[0].Name() != "forgejo" {
		t.Errorf("provider name = %q, want forgejo", provs[0].Name())
	}
	if !cfg.Dind.Enabled {
		t.Error("forgejo should auto-enable cfg.Dind.Enabled")
	}
}

// TestInitProviders_GiteaOnly mirrors the forgejo case for gitea.
func TestInitProviders_GiteaOnly(t *testing.T) {
	cfg := &config.Config{
		Gitea: config.GiteaConfig{
			InstanceURL: "https://gitea.example.com",
			Token:       "fake-token",
		},
	}
	provs, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}
	t.Cleanup(cleanup)

	if len(provs) != 1 {
		t.Fatalf("provider count = %d, want 1", len(provs))
	}
	if provs[0].Name() != "gitea" {
		t.Errorf("provider name = %q, want gitea", provs[0].Name())
	}
	if !cfg.Dind.Enabled {
		t.Error("gitea should auto-enable cfg.Dind.Enabled")
	}
}

// TestInitProviders_MultiActive verifies multiple providers can be active
// simultaneously and they're returned in a stable order.
func TestInitProviders_MultiActive(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			Token: "ghp_test",
			Owner: "ephpm",
		},
		Forgejo: config.ForgejoConfig{
			InstanceURL: "https://codeberg.org",
			Token:       "fake-token",
		},
		Gitea: config.GiteaConfig{
			InstanceURL: "https://gitea.example.com",
			Token:       "fake-token",
		},
	}
	provs, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}
	t.Cleanup(cleanup)

	if len(provs) != 3 {
		t.Fatalf("provider count = %d, want 3", len(provs))
	}

	names := make([]string, len(provs))
	for i, p := range provs {
		names[i] = p.Name()
	}
	wantOrder := []string{"github", "forgejo", "gitea"}
	for i, want := range wantOrder {
		if names[i] != want {
			t.Errorf("provider order: got %v, want order %v", names, wantOrder)
			break
		}
	}
}

// TestInitProviders_GitHubAppMissingKey verifies the AppAuth error path:
// AppID set but missing/invalid private key returns error.
func TestInitProviders_GitHubAppMissingKey(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			Owner:          "ephpm",
			AppID:          12345,
			InstallationID: 678,
			PrivateKeyPath: "/path/to/non/existent/key.pem",
		},
	}
	_, _, err := initProviders(cfg, quietLog())
	if err == nil {
		t.Fatal("expected error from missing private key")
	}
	if !strings.Contains(err.Error(), "github app auth") && !strings.Contains(err.Error(), "github") {
		t.Errorf("err = %v, want github app auth error", err)
	}
}

// TestInitProviders_GitHubOwnerOnly verifies that just an owner (no token,
// no app) is enough to enable GitHub. The provider creation may succeed
// even without a token because some test paths construct unauthenticated
// clients; we just check it goes down the github path.
func TestInitProviders_GitHubOwnerOnly(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			Owner: "ephpm",
		},
	}
	provs, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}
	t.Cleanup(cleanup)
	if len(provs) != 1 || provs[0].Name() != "github" {
		t.Errorf("provs = %+v, want single github provider", provs)
	}
}

// TestInitProviders_CleanupCallable verifies the returned cleanup function
// is safe to call even when no AppAuth was registered.
func TestInitProviders_CleanupCallable(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			Owner: "ephpm",
		},
	}
	_, cleanup, err := initProviders(cfg, quietLog())
	if err != nil {
		t.Fatalf("initProviders: %v", err)
	}

	// Cleanup with no app auth — should not panic.
	cleanup()
	// And should be idempotent.
	cleanup()
}
