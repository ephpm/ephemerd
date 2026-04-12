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
	if cfg.Webhook.Tunnel != "localtunnel" {
		t.Errorf("Webhook.Tunnel = %q, want %q", cfg.Webhook.Tunnel, "localtunnel")
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
	if d := g.ParsedPollInterval(); d != 10*time.Second {
		t.Errorf("empty PollInterval = %v, want 10s", d)
	}
}

func TestParsedPollInterval_Valid(t *testing.T) {
	g := &GitHubConfig{PollInterval: "30s"}
	if d := g.ParsedPollInterval(); d != 30*time.Second {
		t.Errorf("PollInterval = %v, want 30s", d)
	}
}

func TestParsedPollInterval_Invalid(t *testing.T) {
	g := &GitHubConfig{PollInterval: "notaduration"}
	if d := g.ParsedPollInterval(); d != 10*time.Second {
		t.Errorf("invalid PollInterval = %v, want 10s fallback", d)
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
enabled = true
base_image = "/path/to/macos.img"
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
	if !cfg.VM.MacOS.Enabled {
		t.Error("VM.MacOS.Enabled = false, want true")
	}
	if cfg.VM.MacOS.BaseImage != "/path/to/macos.img" {
		t.Errorf("VM.MacOS.BaseImage = %q, want %q", cfg.VM.MacOS.BaseImage, "/path/to/macos.img")
	}
	if cfg.VM.MacOS.CPUs != 8 {
		t.Errorf("VM.MacOS.CPUs = %d, want 8", cfg.VM.MacOS.CPUs)
	}
	if cfg.VM.MacOS.MemoryMB != 16384 {
		t.Errorf("VM.MacOS.MemoryMB = %d, want 16384", cfg.VM.MacOS.MemoryMB)
	}
}
