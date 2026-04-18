package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Loading with empty path should use defaults (but fail validation without github config)
	t.Setenv("GITHUB_TOKEN", "ghp_test123")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
owner = "testorg"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Runner.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", cfg.Runner.MaxConcurrent)
	}
	if cfg.Runner.JobTimeout != "2h" {
		t.Errorf("JobTimeout = %q, want %q", cfg.Runner.JobTimeout, "2h")
	}
	if cfg.Runner.ShutdownTimeout != "5m" {
		t.Errorf("ShutdownTimeout = %q, want %q", cfg.Runner.ShutdownTimeout, "5m")
	}
	if cfg.Webhook.Port != 8080 {
		t.Errorf("Webhook.Port = %d, want 8080", cfg.Webhook.Port)
	}
	if cfg.Webhook.Tunnel != "none" {
		t.Errorf("Webhook.Tunnel = %q, want %q", cfg.Webhook.Tunnel, "none")
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "info")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, "text")
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	// Empty path with no GITHUB_TOKEN owner should fail
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for missing owner, got nil")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	// Non-existent file should warn and return defaults (skips validation)
	cfg, err := Load("/nonexistent/config.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Runner.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want default 4", cfg.Runner.MaxConcurrent)
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`[invalid toml !!!`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML, got nil")
	}
}

func TestLoad_OverridesDefaults(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_override"
owner = "myorg"
repos = ["repo1", "repo2"]

[runner]
max_concurrent = 8
job_timeout = "4h"

[webhook]
port = 9090
tunnel = "none"

[log]
level = "debug"
format = "json"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.GitHub.Token != "ghp_override" {
		t.Errorf("Token = %q, want %q", cfg.GitHub.Token, "ghp_override")
	}
	if cfg.GitHub.Owner != "myorg" {
		t.Errorf("Owner = %q, want %q", cfg.GitHub.Owner, "myorg")
	}
	if len(cfg.GitHub.Repos) != 2 {
		t.Errorf("Repos len = %d, want 2", len(cfg.GitHub.Repos))
	}
	if cfg.Runner.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", cfg.Runner.MaxConcurrent)
	}
	if cfg.Runner.JobTimeout != "4h" {
		t.Errorf("JobTimeout = %q, want %q", cfg.Runner.JobTimeout, "4h")
	}
	if cfg.Webhook.Port != 9090 {
		t.Errorf("Webhook.Port = %d, want 9090", cfg.Webhook.Port)
	}
	if cfg.Webhook.Tunnel != "none" {
		t.Errorf("Webhook.Tunnel = %q, want %q", cfg.Webhook.Tunnel, "none")
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, "json")
	}
}

// --- validate() tests ---

func TestValidate_RequiresTokenOrAppID(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub: GitHubConfig{Owner: "org"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error when no token and no app_id")
	}
}

func TestValidate_AcceptsEnvToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_fromenv")

	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHub.Token != "ghp_fromenv" {
		t.Errorf("Token = %q, want %q", cfg.GitHub.Token, "ghp_fromenv")
	}
}

func TestValidate_AppID_RequiresInstallationID(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub: GitHubConfig{
			Owner:          "org",
			AppID:          12345,
			PrivateKeyPath: "/some/key.pem",
		},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error when app_id set without installation_id")
	}
}

func TestValidate_AppID_RequiresPrivateKeyPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub: GitHubConfig{
			Owner:          "org",
			AppID:          12345,
			InstallationID: 67890,
		},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error when app_id set without private_key_path")
	}
}

func TestValidate_RequiresOwner(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	cfg := &Config{
		GitHub: GitHubConfig{Token: "ghp_test"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error when owner is empty")
	}
}

func TestValidate_GeneratesWebhookSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "localtunnel"},
	}
	err := cfg.validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Webhook.Secret == "" {
		t.Fatal("expected webhook secret to be auto-generated")
	}
	// hex-encoded 32 bytes = 64 chars
	if len(cfg.Webhook.Secret) != 64 {
		t.Errorf("secret length = %d, want 64", len(cfg.Webhook.Secret))
	}
}

func TestValidate_NoSecretWhenTunnelNone(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Webhook.Secret != "" {
		t.Errorf("expected no secret when tunnel=none, got %q", cfg.Webhook.Secret)
	}
}

func TestValidate_PreservesExplicitSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "localtunnel", Secret: "my-explicit-secret"},
	}
	err := cfg.validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Webhook.Secret != "my-explicit-secret" {
		t.Errorf("secret = %q, want %q", cfg.Webhook.Secret, "my-explicit-secret")
	}
}

// --- Duration parser tests ---

func TestParsedPollInterval_Default(t *testing.T) {
	g := &GitHubConfig{}
	if d := g.ParsedPollInterval(); d != 30*time.Second {
		t.Errorf("empty PollInterval = %v, want 30s", d)
	}
}

func TestParsedPollInterval_Valid(t *testing.T) {
	g := &GitHubConfig{PollInterval: "15s"}
	if d := g.ParsedPollInterval(); d != 15*time.Second {
		t.Errorf("PollInterval = %v, want 15s", d)
	}
}

func TestParsedPollInterval_Invalid(t *testing.T) {
	g := &GitHubConfig{PollInterval: "notaduration"}
	if d := g.ParsedPollInterval(); d != 30*time.Second {
		t.Errorf("invalid PollInterval = %v, want 30s fallback", d)
	}
}

func TestParsedJobTimeout_Default(t *testing.T) {
	r := &RunnerConfig{}
	if d := r.ParsedJobTimeout(); d != 2*time.Hour {
		t.Errorf("empty JobTimeout = %v, want 2h", d)
	}
}

func TestParsedJobTimeout_Valid(t *testing.T) {
	r := &RunnerConfig{JobTimeout: "30m"}
	if d := r.ParsedJobTimeout(); d != 30*time.Minute {
		t.Errorf("JobTimeout = %v, want 30m", d)
	}
}

func TestParsedJobTimeout_Invalid(t *testing.T) {
	r := &RunnerConfig{JobTimeout: "bad"}
	if d := r.ParsedJobTimeout(); d != 2*time.Hour {
		t.Errorf("invalid JobTimeout = %v, want 2h fallback", d)
	}
}

func TestParsedShutdownTimeout_Default(t *testing.T) {
	r := &RunnerConfig{}
	if d := r.ParsedShutdownTimeout(); d != 5*time.Minute {
		t.Errorf("empty ShutdownTimeout = %v, want 5m", d)
	}
}

func TestParsedShutdownTimeout_Valid(t *testing.T) {
	r := &RunnerConfig{ShutdownTimeout: "10m"}
	if d := r.ParsedShutdownTimeout(); d != 10*time.Minute {
		t.Errorf("ShutdownTimeout = %v, want 10m", d)
	}
}

func TestParsedShutdownTimeout_Invalid(t *testing.T) {
	r := &RunnerConfig{ShutdownTimeout: "nope"}
	if d := r.ParsedShutdownTimeout(); d != 5*time.Minute {
		t.Errorf("invalid ShutdownTimeout = %v, want 5m fallback", d)
	}
}

// --- Logger tests ---

func TestLogger_Levels(t *testing.T) {
	tests := []struct {
		level string
	}{
		{"debug"},
		{"info"},
		{"warn"},
		{"error"},
		{"unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			cfg := &Config{Log: LogConfig{Level: tt.level}}
			logger := cfg.Logger()
			if logger == nil {
				t.Fatal("Logger() returned nil")
			}
		})
	}
}

func TestLogger_Formats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			cfg := &Config{Log: LogConfig{Format: format}}
			logger := cfg.Logger()
			if logger == nil {
				t.Fatal("Logger() returned nil")
			}
		})
	}
}

// --- LogRetentionDuration tests ---

func TestLogRetentionDuration_Default(t *testing.T) {
	lc := LogConfig{}
	if d := lc.LogRetentionDuration(); d != 7*24*time.Hour {
		t.Errorf("empty LogRetention = %v, want 168h", d)
	}
}

func TestLogRetentionDuration_Days(t *testing.T) {
	lc := LogConfig{LogRetention: "14d"}
	if d := lc.LogRetentionDuration(); d != 14*24*time.Hour {
		t.Errorf("LogRetention = %v, want 336h", d)
	}
}

func TestLogRetentionDuration_Hours(t *testing.T) {
	lc := LogConfig{LogRetention: "48h"}
	if d := lc.LogRetentionDuration(); d != 48*time.Hour {
		t.Errorf("LogRetention = %v, want 48h", d)
	}
}

func TestLogRetentionDuration_Invalid(t *testing.T) {
	lc := LogConfig{LogRetention: "garbage"}
	if d := lc.LogRetentionDuration(); d != 7*24*time.Hour {
		t.Errorf("invalid LogRetention = %v, want 168h fallback", d)
	}
}

// --- Provider() detection ---

func TestProvider_Default(t *testing.T) {
	cfg := &Config{}
	if p := cfg.Provider(); p != "github" {
		t.Errorf("Provider() = %q, want %q", p, "github")
	}
}

func TestProvider_Forgejo(t *testing.T) {
	cfg := &Config{
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"},
	}
	if p := cfg.Provider(); p != "forgejo" {
		t.Errorf("Provider() = %q, want %q", p, "forgejo")
	}
}

func TestProvider_Gitea(t *testing.T) {
	cfg := &Config{
		Gitea: GiteaConfig{InstanceURL: "https://gitea.example.com"},
	}
	if p := cfg.Provider(); p != "gitea" {
		t.Errorf("Provider() = %q, want %q", p, "gitea")
	}
}

func TestProvider_GitLab(t *testing.T) {
	cfg := &Config{
		GitLab: GitLabConfig{InstanceURL: "https://gitlab.com"},
	}
	if p := cfg.Provider(); p != "gitlab" {
		t.Errorf("Provider() = %q, want %q", p, "gitlab")
	}
}

func TestProvider_Precedence_ForgejoOverGitea(t *testing.T) {
	cfg := &Config{
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"},
		Gitea:   GiteaConfig{InstanceURL: "https://gitea.example.com"},
	}
	if p := cfg.Provider(); p != "forgejo" {
		t.Errorf("Provider() = %q, want %q (forgejo takes precedence over gitea)", p, "forgejo")
	}
}

func TestProvider_Precedence_GiteaOverGitLab(t *testing.T) {
	cfg := &Config{
		Gitea:  GiteaConfig{InstanceURL: "https://gitea.example.com"},
		GitLab: GitLabConfig{InstanceURL: "https://gitlab.com"},
	}
	if p := cfg.Provider(); p != "gitea" {
		t.Errorf("Provider() = %q, want %q (gitea takes precedence over gitlab)", p, "gitea")
	}
}

func TestProvider_Precedence_GitLabOverGitHub(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubConfig{Token: "ghp_test", Owner: "org"},
		GitLab: GitLabConfig{InstanceURL: "https://gitlab.com"},
	}
	if p := cfg.Provider(); p != "gitlab" {
		t.Errorf("Provider() = %q, want %q (gitlab takes precedence over github)", p, "gitlab")
	}
}

func TestProvider_Woodpecker(t *testing.T) {
	cfg := &Config{
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	if p := cfg.Provider(); p != "woodpecker" {
		t.Errorf("Provider() = %q, want %q", p, "woodpecker")
	}
}

func TestProvider_Precedence_GitLabOverWoodpecker(t *testing.T) {
	cfg := &Config{
		GitLab:     GitLabConfig{InstanceURL: "https://gitlab.com"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	if p := cfg.Provider(); p != "gitlab" {
		t.Errorf("Provider() = %q, want %q (gitlab takes precedence over woodpecker)", p, "gitlab")
	}
}

func TestProvider_Precedence_WoodpeckerOverGitHub(t *testing.T) {
	cfg := &Config{
		GitHub:     GitHubConfig{Token: "ghp_test", Owner: "org"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	if p := cfg.Provider(); p != "woodpecker" {
		t.Errorf("Provider() = %q, want %q (woodpecker takes precedence over github)", p, "woodpecker")
	}
}

// --- Per-provider validation ---

func TestValidate_ForgejoRequiresToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for missing forgejo token")
	}
}

func TestValidate_ForgejoValid(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org", Token: "tok"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_GiteaRequiresToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Gitea: GiteaConfig{InstanceURL: "https://gitea.example.com"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for missing gitea token")
	}
}

func TestValidate_GiteaValid(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Gitea:   GiteaConfig{InstanceURL: "https://gitea.example.com", Token: "tok"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_GitLabRequiresToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitLab: GitLabConfig{InstanceURL: "https://gitlab.com"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for missing gitlab token")
	}
}

func TestValidate_GitLabValid(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitLab:  GitLabConfig{InstanceURL: "https://gitlab.com", Token: "glrt-xxx"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_WoodpeckerRequiresAgentSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for missing woodpecker agent_secret")
	}
}

func TestValidate_WoodpeckerValid(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000", AgentSecret: "secret"},
		Webhook:    WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_ForgejoSkipsGitHubValidation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org", Token: "tok"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	// Should NOT fail even though no GitHub token/owner are set.
	if err := cfg.validate(); err != nil {
		t.Fatalf("forgejo config should not require github credentials: %v", err)
	}
}

// --- TOML parsing for new provider sections ---

func TestLoad_ForgejoConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[forgejo]
instance_url = "https://codeberg.org"
token = "my-token"
owner = "myorg"
repos = ["repo1", "repo2"]
job_image = "custom/image:latest"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Provider() != "forgejo" {
		t.Errorf("Provider() = %q, want %q", cfg.Provider(), "forgejo")
	}
	if cfg.Forgejo.InstanceURL != "https://codeberg.org" {
		t.Errorf("InstanceURL = %q, want %q", cfg.Forgejo.InstanceURL, "https://codeberg.org")
	}
	if cfg.Forgejo.Token != "my-token" {
		t.Errorf("Token = %q, want %q", cfg.Forgejo.Token, "my-token")
	}
	if cfg.Forgejo.Owner != "myorg" {
		t.Errorf("Owner = %q, want %q", cfg.Forgejo.Owner, "myorg")
	}
	if len(cfg.Forgejo.Repos) != 2 || cfg.Forgejo.Repos[0] != "repo1" {
		t.Errorf("Repos = %v, want [repo1, repo2]", cfg.Forgejo.Repos)
	}
	if cfg.Forgejo.JobImage != "custom/image:latest" {
		t.Errorf("JobImage = %q, want %q", cfg.Forgejo.JobImage, "custom/image:latest")
	}
}

func TestLoad_GiteaConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[gitea]
instance_url = "https://gitea.example.com"
token = "gitea-tok"
owner = "org"
repos = ["r1"]
job_image = "gitea/runner-images:ubuntu-22.04"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Provider() != "gitea" {
		t.Errorf("Provider() = %q, want %q", cfg.Provider(), "gitea")
	}
	if cfg.Gitea.InstanceURL != "https://gitea.example.com" {
		t.Errorf("InstanceURL = %q, want %q", cfg.Gitea.InstanceURL, "https://gitea.example.com")
	}
	if cfg.Gitea.Token != "gitea-tok" {
		t.Errorf("Token = %q, want %q", cfg.Gitea.Token, "gitea-tok")
	}
	if cfg.Gitea.Owner != "org" {
		t.Errorf("Owner = %q, want %q", cfg.Gitea.Owner, "org")
	}
	if len(cfg.Gitea.Repos) != 1 || cfg.Gitea.Repos[0] != "r1" {
		t.Errorf("Repos = %v, want [r1]", cfg.Gitea.Repos)
	}
	if cfg.Gitea.JobImage != "gitea/runner-images:ubuntu-22.04" {
		t.Errorf("JobImage = %q, want %q", cfg.Gitea.JobImage, "gitea/runner-images:ubuntu-22.04")
	}
}

func TestLoad_GitLabConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxxxxxxxxxxx"
tags = ["linux", "docker", "ephemerd"]
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Provider() != "gitlab" {
		t.Errorf("Provider() = %q, want %q", cfg.Provider(), "gitlab")
	}
	if cfg.GitLab.InstanceURL != "https://gitlab.com" {
		t.Errorf("InstanceURL = %q, want %q", cfg.GitLab.InstanceURL, "https://gitlab.com")
	}
	if cfg.GitLab.Token != "glrt-xxxxxxxxxxxx" {
		t.Errorf("Token = %q, want %q", cfg.GitLab.Token, "glrt-xxxxxxxxxxxx")
	}
	if len(cfg.GitLab.Tags) != 3 || cfg.GitLab.Tags[2] != "ephemerd" {
		t.Errorf("Tags = %v, want [linux, docker, ephemerd]", cfg.GitLab.Tags)
	}
}

func TestLoad_WoodpeckerConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[woodpecker]
server_url = "woodpecker.example.com:9000"
agent_secret = "my-secret"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Provider() != "woodpecker" {
		t.Errorf("Provider() = %q, want %q", cfg.Provider(), "woodpecker")
	}
	if cfg.Woodpecker.ServerURL != "woodpecker.example.com:9000" {
		t.Errorf("ServerURL = %q, want %q", cfg.Woodpecker.ServerURL, "woodpecker.example.com:9000")
	}
	if cfg.Woodpecker.AgentSecret != "my-secret" {
		t.Errorf("AgentSecret = %q, want %q", cfg.Woodpecker.AgentSecret, "my-secret")
	}
}

// --- VM config TOML parsing ---

func TestLoad_VMConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"

[webhook]
tunnel = "none"

[vm.linux]
enabled = true
cpus = 4
memory_mb = 4096
disk_size_gb = 100

[vm.macos]
disk_image = "/path/to/macos.img"
cpus = 8
memory_mb = 16384
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.VM.Linux.Enabled {
		t.Error("VM.Linux.Enabled = false, want true")
	}
	if cfg.VM.Linux.CPUs != 4 {
		t.Errorf("VM.Linux.CPUs = %d, want 4", cfg.VM.Linux.CPUs)
	}
	if cfg.VM.Linux.MemoryMB != 4096 {
		t.Errorf("VM.Linux.MemoryMB = %d, want 4096", cfg.VM.Linux.MemoryMB)
	}
	if cfg.VM.Linux.DiskSizeGB != 100 {
		t.Errorf("VM.Linux.DiskSizeGB = %d, want 100", cfg.VM.Linux.DiskSizeGB)
	}
	if cfg.VM.MacOS.DiskImage != "/path/to/macos.img" {
		t.Errorf("VM.MacOS.DiskImage = %q, want %q", cfg.VM.MacOS.DiskImage, "/path/to/macos.img")
	}
	if cfg.VM.MacOS.CPUs != 8 {
		t.Errorf("VM.MacOS.CPUs = %d, want 8", cfg.VM.MacOS.CPUs)
	}
	if cfg.VM.MacOS.MemoryMB != 16384 {
		t.Errorf("VM.MacOS.MemoryMB = %d, want 16384", cfg.VM.MacOS.MemoryMB)
	}
}
