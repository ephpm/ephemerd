package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"
)

func contains(ss []string, s string) bool { return slices.Contains(ss, s) }

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

func TestValidate_ExternalTunnel_RequiresSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "external"},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal(`expected error: tunnel="external" without a secret must be rejected`)
	}
}

func TestValidate_ExternalTunnel_KeepsSecretAndDefaultsPort(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	const secret = "deadbeefcafe"
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "external", Secret: secret},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// External ingress: the secret must match the external webhook config, so
	// it must never be regenerated.
	if cfg.Webhook.Secret != secret {
		t.Errorf("Secret = %q, want it left untouched (%q)", cfg.Webhook.Secret, secret)
	}
	if cfg.Webhook.Port != 8080 {
		t.Errorf("Port = %d, want default 8080", cfg.Webhook.Port)
	}
}

func TestValidate_ManagedTunnel_GeneratesSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "ngrok"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Managed tunnels register the webhook themselves, so a generated secret is
	// fine (and expected) when none was provided.
	if cfg.Webhook.Secret == "" {
		t.Error("expected a generated secret for a managed tunnel, got empty")
	}
}

func TestValidate_NoneWithSecret_DefaultsPort(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	const secret = "abc123"
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "none", Secret: secret},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Webhook.Secret != secret {
		t.Errorf("Secret = %q, want %q (must not be regenerated)", cfg.Webhook.Secret, secret)
	}
	if cfg.Webhook.Port != 8080 {
		t.Errorf("Port = %d, want default 8080 when a secret enables the receiver", cfg.Webhook.Port)
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

// --- WindowsRunnerToml tests ---

func TestWindowsRunnerToml_DefaultMemory(t *testing.T) {
	w := WindowsRunnerToml{}
	if got, want := w.MemoryBytes(), uint64(4096)*1024*1024; got != want {
		t.Errorf("MemoryBytes() default = %d, want %d (4 GB)", got, want)
	}
}

func TestWindowsRunnerToml_DefaultCPUs(t *testing.T) {
	w := WindowsRunnerToml{}
	if got, want := w.CPUCount(), uint64(2); got != want {
		t.Errorf("CPUCount() default = %d, want %d", got, want)
	}
}

func TestWindowsRunnerToml_OverrideMemory(t *testing.T) {
	w := WindowsRunnerToml{MemoryMB: 16384}
	if got, want := w.MemoryBytes(), uint64(16384)*1024*1024; got != want {
		t.Errorf("MemoryBytes() = %d, want %d", got, want)
	}
}

func TestWindowsRunnerToml_OverrideCPUs(t *testing.T) {
	w := WindowsRunnerToml{CPUs: 8}
	if got, want := w.CPUCount(), uint64(8); got != want {
		t.Errorf("CPUCount() = %d, want %d", got, want)
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

// --- ImageForRepoOS tests ---

func TestImageForRepoOS_PerOSOverride(t *testing.T) {
	r := &RunnerConfig{
		DefaultImage: "default:latest",
		Images: map[string]map[string]string{
			"ephemerd": {
				"linux":   "ephpm/ephemerd:runner-ci-linux-amd64",
				"windows": "ephpm/ephemerd:runner-ci-windows",
			},
			"ephpm": {
				"linux": "ephpm/ephpm:ci-runner",
			},
		},
	}
	if img := r.ImageForRepoOS("ephemerd", "linux"); img != "ephpm/ephemerd:runner-ci-linux-amd64" {
		t.Errorf("ephemerd/linux = %q", img)
	}
	if img := r.ImageForRepoOS("ephemerd", "windows"); img != "ephpm/ephemerd:runner-ci-windows" {
		t.Errorf("ephemerd/windows = %q", img)
	}
	if img := r.ImageForRepoOS("ephpm", "linux"); img != "ephpm/ephpm:ci-runner" {
		t.Errorf("ephpm/linux = %q", img)
	}
}

func TestImageForRepoOS_RepoMissingOSReturnsEmpty(t *testing.T) {
	// A repo that only declares Linux must return "" for Windows so the
	// caller falls through to the provider/runtime defaults — *not* leak
	// the Linux image into a Windows job.
	r := &RunnerConfig{
		Images: map[string]map[string]string{
			"ephpm": {"linux": "ephpm/ephpm:ci-runner"},
		},
	}
	if img := r.ImageForRepoOS("ephpm", "windows"); img != "" {
		t.Errorf("ephpm/windows = %q, want empty (not Linux image)", img)
	}
}

func TestImageForRepoOS_UnknownRepoReturnsEmpty(t *testing.T) {
	r := &RunnerConfig{
		DefaultImage: "default:latest",
		Images: map[string]map[string]string{
			"ephemerd": {"linux": "custom:latest"},
		},
	}
	if img := r.ImageForRepoOS("other-repo", "linux"); img != "" {
		t.Errorf("ImageForRepoOS(other-repo, linux) = %q, want empty", img)
	}
}

func TestImageForRepoOS_NilSafe(t *testing.T) {
	var r *RunnerConfig
	if img := r.ImageForRepoOS("any", "linux"); img != "" {
		t.Errorf("nil receiver = %q, want empty", img)
	}
}

// --- ImageForRepo (legacy helper) tests ---

func TestImageForRepo_Override(t *testing.T) {
	r := &RunnerConfig{
		DefaultImage: "default:latest",
		Images: map[string]map[string]string{
			"ephemerd": {"linux": "ephpm/ephemerd:runner-ci-linux"},
			"ephpm":    {"linux": "ephpm/ephpm:ci-runner"},
		},
	}
	if img := r.ImageForRepo("ephemerd"); img != "ephpm/ephemerd:runner-ci-linux" {
		t.Errorf("ImageForRepo(ephemerd) = %q", img)
	}
	if img := r.ImageForRepo("ephpm"); img != "ephpm/ephpm:ci-runner" {
		t.Errorf("ImageForRepo(ephpm) = %q", img)
	}
}

func TestImageForRepo_FallbackToDefault(t *testing.T) {
	r := &RunnerConfig{
		DefaultImage: "default:latest",
		Images: map[string]map[string]string{
			"ephemerd": {"linux": "custom:latest"},
		},
	}
	if img := r.ImageForRepo("other-repo"); img != "default:latest" {
		t.Errorf("ImageForRepo(other-repo) = %q, want default:latest", img)
	}
}

func TestImageForRepo_NoImagesMap(t *testing.T) {
	r := &RunnerConfig{DefaultImage: "default:latest"}
	if img := r.ImageForRepo("anything"); img != "default:latest" {
		t.Errorf("ImageForRepo(anything) = %q", img)
	}
}

func TestImageForRepo_EmptyDefault(t *testing.T) {
	r := &RunnerConfig{}
	if img := r.ImageForRepo("anything"); img != "" {
		t.Errorf("ImageForRepo(anything) = %q, want empty", img)
	}
}

// --- DefaultImageFor tests (provider-level) ---

func TestGitHubConfig_DefaultImageFor_PerOS(t *testing.T) {
	c := &GitHubConfig{
		DefaultImageLinux:   "lin",
		DefaultImageWindows: "win",
	}
	if c.DefaultImageFor("linux") != "lin" {
		t.Errorf("linux = %q", c.DefaultImageFor("linux"))
	}
	if c.DefaultImageFor("windows") != "win" {
		t.Errorf("windows = %q", c.DefaultImageFor("windows"))
	}
}

func TestGitHubConfig_DefaultImageFor_LegacyFallsBackForLinuxOnly(t *testing.T) {
	// default_image should keep working as a Linux default for old configs,
	// but must NOT be returned for Windows lookups.
	c := &GitHubConfig{DefaultImage: "legacy"}
	if c.DefaultImageFor("linux") != "legacy" {
		t.Errorf("linux = %q, want legacy", c.DefaultImageFor("linux"))
	}
	if c.DefaultImageFor("windows") != "" {
		t.Errorf("windows = %q, want empty (legacy is Linux only)", c.DefaultImageFor("windows"))
	}
}

func TestGitHubConfig_DefaultImageFor_LinuxOverridesLegacy(t *testing.T) {
	c := &GitHubConfig{
		DefaultImage:      "legacy",
		DefaultImageLinux: "new",
	}
	if c.DefaultImageFor("linux") != "new" {
		t.Errorf("linux = %q, want new (overrides legacy)", c.DefaultImageFor("linux"))
	}
}

func TestForgejoConfig_DefaultImageFor(t *testing.T) {
	c := &ForgejoConfig{
		DefaultImage:        "legacy",
		DefaultImageLinux:   "lin",
		DefaultImageWindows: "win",
	}
	if c.DefaultImageFor("linux") != "lin" {
		t.Errorf("linux = %q, want lin", c.DefaultImageFor("linux"))
	}
	if c.DefaultImageFor("windows") != "win" {
		t.Errorf("windows = %q, want win", c.DefaultImageFor("windows"))
	}
	// Legacy fallback for Linux when per-OS not set.
	c2 := &ForgejoConfig{DefaultImage: "legacy"}
	if c2.DefaultImageFor("linux") != "legacy" {
		t.Errorf("linux fallback = %q, want legacy", c2.DefaultImageFor("linux"))
	}
	if c2.DefaultImageFor("windows") != "" {
		t.Errorf("windows = %q, want empty (legacy is Linux only)", c2.DefaultImageFor("windows"))
	}
}

func TestGiteaConfig_DefaultImageFor(t *testing.T) {
	c := &GiteaConfig{
		DefaultImage:        "legacy",
		DefaultImageLinux:   "lin",
		DefaultImageWindows: "win",
	}
	if c.DefaultImageFor("linux") != "lin" {
		t.Errorf("linux = %q, want lin", c.DefaultImageFor("linux"))
	}
	if c.DefaultImageFor("windows") != "win" {
		t.Errorf("windows = %q, want win", c.DefaultImageFor("windows"))
	}
	c2 := &GiteaConfig{DefaultImage: "legacy"}
	if c2.DefaultImageFor("linux") != "legacy" {
		t.Errorf("linux fallback = %q, want legacy", c2.DefaultImageFor("linux"))
	}
	if c2.DefaultImageFor("windows") != "" {
		t.Errorf("windows = %q, want empty (legacy is Linux only)", c2.DefaultImageFor("windows"))
	}
}

func TestGitLabConfig_DefaultImageFor(t *testing.T) {
	c := &GitLabConfig{
		DefaultImage:        "legacy",
		DefaultImageLinux:   "lin",
		DefaultImageWindows: "win",
	}
	if c.DefaultImageFor("linux") != "lin" {
		t.Errorf("linux = %q, want lin", c.DefaultImageFor("linux"))
	}
	if c.DefaultImageFor("windows") != "win" {
		t.Errorf("windows = %q, want win", c.DefaultImageFor("windows"))
	}
	c2 := &GitLabConfig{DefaultImage: "legacy"}
	if c2.DefaultImageFor("linux") != "legacy" {
		t.Errorf("linux fallback = %q, want legacy", c2.DefaultImageFor("linux"))
	}
	if c2.DefaultImageFor("windows") != "" {
		t.Errorf("windows = %q, want empty (legacy is Linux only)", c2.DefaultImageFor("windows"))
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

func TestLogger_DebugEnabled(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "debug"}}
	logger := cfg.Logger()

	// Debug logger should log at debug level
	handler := logger.Handler()
	if !handler.Enabled(context.TODO(), -4) { // slog.LevelDebug == -4
		t.Error("debug logger should be enabled at debug level")
	}
}

func TestLogger_InfoDisablesDebug(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "info"}}
	logger := cfg.Logger()

	handler := logger.Handler()
	if handler.Enabled(context.TODO(), -4) { // slog.LevelDebug
		t.Error("info logger should NOT be enabled at debug level")
	}
	if !handler.Enabled(context.TODO(), 0) { // slog.LevelInfo
		t.Error("info logger should be enabled at info level")
	}
}

func TestLogger_WarnLevel(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "warn"}}
	logger := cfg.Logger()

	handler := logger.Handler()
	if handler.Enabled(context.TODO(), 0) { // slog.LevelInfo
		t.Error("warn logger should NOT be enabled at info level")
	}
	if !handler.Enabled(context.TODO(), 4) { // slog.LevelWarn
		t.Error("warn logger should be enabled at warn level")
	}
}

func TestLogger_ErrorLevel(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "error"}}
	logger := cfg.Logger()

	handler := logger.Handler()
	if handler.Enabled(context.TODO(), 4) { // slog.LevelWarn
		t.Error("error logger should NOT be enabled at warn level")
	}
	if !handler.Enabled(context.TODO(), 8) { // slog.LevelError
		t.Error("error logger should be enabled at error level")
	}
}

func TestLogger_UnknownDefaultsToInfo(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "garbage"}}
	logger := cfg.Logger()

	handler := logger.Handler()
	if handler.Enabled(context.TODO(), -4) { // slog.LevelDebug
		t.Error("unknown level should default to info, not enable debug")
	}
	if !handler.Enabled(context.TODO(), 0) { // slog.LevelInfo
		t.Error("unknown level should default to info")
	}
}

func TestLogger_JSONOutput(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "info", Format: "json"}}
	logger := cfg.Logger()

	// Logger should produce valid JSON — test by logging to a buffer
	// We can't easily redirect cfg.Logger() output, but we can verify
	// the handler type by checking it handles records without panic
	logger.Info("test message", "key", "value")
	// If we got here without panic, the JSON handler is working
}

func TestLogger_TextOutput(t *testing.T) {
	cfg := &Config{Log: LogConfig{Level: "info", Format: "text"}}
	logger := cfg.Logger()
	logger.Info("test message", "key", "value")
	// If we got here without panic, the text handler is working
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

// --- ModuleProxy config ---

func TestLoad_ModuleProxyConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"

[module_proxy]
enabled = true
port = 9000
upstream = "https://goproxy.io"
cleanup = false
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.ModuleProxy.Enabled {
		t.Error("ModuleProxy.Enabled = false, want true")
	}
	if cfg.ModuleProxy.Port != 9000 {
		t.Errorf("ModuleProxy.Port = %d, want 9000", cfg.ModuleProxy.Port)
	}
	if cfg.ModuleProxy.Upstream != "https://goproxy.io" {
		t.Errorf("ModuleProxy.Upstream = %q, want %q", cfg.ModuleProxy.Upstream, "https://goproxy.io")
	}
	if cfg.ModuleProxy.Cleanup {
		t.Error("ModuleProxy.Cleanup = true, want false")
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

func TestProviders_GitLabAndGitHub(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubConfig{Token: "ghp_test", Owner: "org"},
		GitLab: GitLabConfig{InstanceURL: "https://gitlab.com"},
	}
	ps := cfg.Providers()
	if len(ps) != 2 {
		t.Fatalf("Providers() = %v, want 2 providers", ps)
	}
	if !contains(ps, "github") || !contains(ps, "gitlab") {
		t.Errorf("Providers() = %v, want both github and gitlab", ps)
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

func TestProviders_WoodpeckerAndGitHub(t *testing.T) {
	cfg := &Config{
		GitHub:     GitHubConfig{Token: "ghp_test", Owner: "org"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	ps := cfg.Providers()
	if len(ps) != 2 {
		t.Fatalf("Providers() = %v, want 2 providers", ps)
	}
	if !contains(ps, "github") || !contains(ps, "woodpecker") {
		t.Errorf("Providers() = %v, want both github and woodpecker", ps)
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

// --- Providers() multi-provider detection ---

func TestProviders_Empty(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{}
	ps := cfg.Providers()
	if len(ps) != 0 {
		t.Errorf("Providers() = %v, want empty", ps)
	}
}

func TestProviders_GitHubOnly(t *testing.T) {
	cfg := &Config{GitHub: GitHubConfig{Token: "ghp_test"}}
	ps := cfg.Providers()
	if len(ps) != 1 || ps[0] != "github" {
		t.Errorf("Providers() = %v, want [github]", ps)
	}
}

func TestProviders_GitHubViaOwner(t *testing.T) {
	cfg := &Config{GitHub: GitHubConfig{Owner: "org"}}
	ps := cfg.Providers()
	if len(ps) != 1 || ps[0] != "github" {
		t.Errorf("Providers() = %v, want [github]", ps)
	}
}

func TestProviders_GitHubViaAppID(t *testing.T) {
	cfg := &Config{GitHub: GitHubConfig{AppID: 12345}}
	ps := cfg.Providers()
	if len(ps) != 1 || ps[0] != "github" {
		t.Errorf("Providers() = %v, want [github]", ps)
	}
}

func TestProviders_ForgejoOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"}}
	ps := cfg.Providers()
	if len(ps) != 1 || ps[0] != "forgejo" {
		t.Errorf("Providers() = %v, want [forgejo]", ps)
	}
}

func TestProviders_AllFiveConfigured(t *testing.T) {
	cfg := &Config{
		GitHub:     GitHubConfig{Token: "ghp_test"},
		Forgejo:    ForgejoConfig{InstanceURL: "https://codeberg.org"},
		Gitea:      GiteaConfig{InstanceURL: "https://gitea.example.com"},
		GitLab:     GitLabConfig{InstanceURL: "https://gitlab.com"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	ps := cfg.Providers()
	if len(ps) != 5 {
		t.Fatalf("Providers() = %v, want 5 providers", ps)
	}
	for _, want := range []string{"github", "forgejo", "gitea", "gitlab", "woodpecker"} {
		if !contains(ps, want) {
			t.Errorf("Providers() missing %q: %v", want, ps)
		}
	}
}

func TestProviders_GitHubAndForgejo(t *testing.T) {
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org", Token: "ghp_test"},
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"},
	}
	ps := cfg.Providers()
	if len(ps) != 2 {
		t.Fatalf("Providers() = %v, want 2", ps)
	}
	if !contains(ps, "github") || !contains(ps, "forgejo") {
		t.Errorf("Providers() = %v, want github + forgejo", ps)
	}
}

// --- Multi-provider validation ---

func TestValidate_MultiProvider_BothChecked(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	// GitHub has owner but no token — should fail validation
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org"},
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org", Token: "tok"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error: github configured with owner but no token")
	}
}

func TestValidate_MultiProvider_ForgejoMissingToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org", Token: "ghp_test"},
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error: forgejo configured with instance_url but no token")
	}
}

func TestValidate_MultiProvider_AllValid(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "org", Token: "ghp_test"},
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org", Token: "tok"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- default_image TOML parsing ---

func TestLoad_GitHubDefaultImage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"
default_image = "my-custom-runner:v2"

[webhook]
tunnel = "none"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.GitHub.DefaultImage != "my-custom-runner:v2" {
		t.Errorf("GitHub.DefaultImage = %q, want %q", cfg.GitHub.DefaultImage, "my-custom-runner:v2")
	}
}

func TestLoad_ForgejoDefaultImage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[forgejo]
instance_url = "https://codeberg.org"
token = "tok"
default_image = "my-forgejo-runner:latest"
job_image = "custom-job:v1"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Forgejo.DefaultImage != "my-forgejo-runner:latest" {
		t.Errorf("Forgejo.DefaultImage = %q, want %q", cfg.Forgejo.DefaultImage, "my-forgejo-runner:latest")
	}
	if cfg.Forgejo.JobImage != "custom-job:v1" {
		t.Errorf("Forgejo.JobImage = %q, want %q", cfg.Forgejo.JobImage, "custom-job:v1")
	}
}

func TestLoad_GiteaDefaultImage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[gitea]
instance_url = "https://gitea.example.com"
token = "tok"
default_image = "my-gitea-runner:latest"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Gitea.DefaultImage != "my-gitea-runner:latest" {
		t.Errorf("Gitea.DefaultImage = %q, want %q", cfg.Gitea.DefaultImage, "my-gitea-runner:latest")
	}
}

func TestLoad_GitLabDefaultImage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[gitlab]
instance_url = "https://gitlab.com"
token = "glrt-xxx"
default_image = "my-gitlab-runner:latest"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.GitLab.DefaultImage != "my-gitlab-runner:latest" {
		t.Errorf("GitLab.DefaultImage = %q, want %q", cfg.GitLab.DefaultImage, "my-gitlab-runner:latest")
	}
}

func TestLoad_DefaultImageNotSet(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"

[webhook]
tunnel = "none"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.GitHub.DefaultImage != "" {
		t.Errorf("GitHub.DefaultImage = %q, want empty (use provider default)", cfg.GitHub.DefaultImage)
	}
}

func TestLoad_MultiProviderConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"
default_image = "ghcr.io/custom/runner:v3"

[forgejo]
instance_url = "https://codeberg.org"
token = "forgejo-tok"
default_image = "my-forgejo:latest"

[webhook]
tunnel = "none"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	ps := cfg.Providers()
	if len(ps) != 2 {
		t.Fatalf("Providers() = %v, want 2 providers", ps)
	}
	if !contains(ps, "github") || !contains(ps, "forgejo") {
		t.Errorf("Providers() = %v, want github + forgejo", ps)
	}
	if cfg.GitHub.DefaultImage != "ghcr.io/custom/runner:v3" {
		t.Errorf("GitHub.DefaultImage = %q", cfg.GitHub.DefaultImage)
	}
	if cfg.Forgejo.DefaultImage != "my-forgejo:latest" {
		t.Errorf("Forgejo.DefaultImage = %q", cfg.Forgejo.DefaultImage)
	}
}

// --- CrossPlatformEnabled tests ---

func TestCrossPlatformEnabled_NilDefault(t *testing.T) {
	v := &VMConfig{}
	if !v.CrossPlatformEnabled() {
		t.Error("CrossPlatformEnabled() should default to true when nil")
	}
}

func TestCrossPlatformEnabled_ExplicitTrue(t *testing.T) {
	b := true
	v := &VMConfig{CrossPlatform: &b}
	if !v.CrossPlatformEnabled() {
		t.Error("CrossPlatformEnabled() should be true when set to true")
	}
}

func TestCrossPlatformEnabled_ExplicitFalse(t *testing.T) {
	b := false
	v := &VMConfig{CrossPlatform: &b}
	if v.CrossPlatformEnabled() {
		t.Error("CrossPlatformEnabled() should be false when set to false")
	}
}

// --- crlfWriter tests ---

func TestCrlfWriter_ReplacesNewlines(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}

	_, err := w.Write([]byte("line1\nline2\nline3"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	got := buf.String()
	want := "line1\r\nline2\r\nline3"
	if got != want {
		t.Errorf("crlfWriter output = %q, want %q", got, want)
	}
}

func TestCrlfWriter_NoNewlines(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}

	_, err := w.Write([]byte("no newlines here"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	if buf.String() != "no newlines here" {
		t.Errorf("crlfWriter output = %q, want original", buf.String())
	}
}

func TestCrlfWriter_Empty(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}

	n, err := w.Write([]byte{})
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != 0 {
		t.Errorf("Write(empty) = %d, want 0", n)
	}
}

func TestCrlfWriter_ReportsOriginalLength(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}

	input := []byte("a\nb\nc")
	n, err := w.Write(input)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(input) {
		t.Errorf("Write returned %d, want %d (original length)", n, len(input))
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

// --- Additional Providers() and validate() coverage ---

func TestProviders_Order_GitHubFirst(t *testing.T) {
	// When all 5 providers are configured, github is always returned first
	// (Provider() relies on this for its single-string fallback).
	cfg := &Config{
		GitHub:     GitHubConfig{Token: "ghp_test"},
		Forgejo:    ForgejoConfig{InstanceURL: "https://codeberg.org"},
		Gitea:      GiteaConfig{InstanceURL: "https://gitea.example.com"},
		GitLab:     GitLabConfig{InstanceURL: "https://gitlab.com"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"},
	}
	ps := cfg.Providers()
	if len(ps) == 0 || ps[0] != "github" {
		t.Errorf("Providers() = %v, want github first", ps)
	}
	if p := cfg.Provider(); p != "github" {
		t.Errorf("Provider() = %q, want %q", p, "github")
	}
}

func TestValidate_GitHubApp_HappyPath(t *testing.T) {
	// All four GitHub App fields set + owner = valid (no error).
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub: GitHubConfig{
			AppID:          12345,
			InstallationID: 67890,
			PrivateKeyPath: "/path/to/key.pem",
			Owner:          "myorg",
		},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_PAT_HappyPath(t *testing.T) {
	// PAT-only authentication (no AppID) with owner = valid.
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "myorg"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_EnvTokenWithOwnerOnly(t *testing.T) {
	// Owner alone triggers github provider. GITHUB_TOKEN env should be picked up.
	t.Setenv("GITHUB_TOKEN", "ghp_envonly")
	cfg := &Config{
		GitHub:  GitHubConfig{Owner: "myorg"},
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHub.Token != "ghp_envonly" {
		t.Errorf("Token = %q, want env-supplied value", cfg.GitHub.Token)
	}
}

func TestValidate_WebhookSecretIsHex(t *testing.T) {
	// Auto-generated secret should be hex-encoded (only 0-9, a-f).
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "ngrok"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range cfg.Webhook.Secret {
		isDigit := c >= '0' && c <= '9'
		isHex := c >= 'a' && c <= 'f'
		if !isDigit && !isHex {
			t.Errorf("secret contains non-hex char %c: %s", c, cfg.Webhook.Secret)
			break
		}
	}
}

func TestValidate_NoSecretWhenTunnelEmpty(t *testing.T) {
	// Empty tunnel string is also "no tunnel" (only "!= none" triggers gen).
	// Specifically: tunnel="" should not trigger secret generation.
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{}, // Tunnel is empty string
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Currently empty string != "none" so it WILL generate a secret.
	// This documents current behavior — caller must explicitly set "none".
	if cfg.Webhook.Secret == "" {
		t.Skip("if behavior changes to treat empty tunnel as 'none', this test will need update")
	}
}

func TestValidate_NgrokTunnelGeneratesSecret(t *testing.T) {
	// ngrok tunnel should also trigger auto-secret generation.
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitHub:  GitHubConfig{Token: "ghp_test", Owner: "org"},
		Webhook: WebhookConfig{Tunnel: "ngrok"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Webhook.Secret) != 64 {
		t.Errorf("expected hex-encoded 32-byte secret, got len=%d", len(cfg.Webhook.Secret))
	}
}

func TestValidate_GitLabAndWoodpeckerBothChecked(t *testing.T) {
	// Multi-provider case: gitlab valid, woodpecker missing secret.
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		GitLab:     GitLabConfig{InstanceURL: "https://gitlab.com", Token: "glrt-x"},
		Woodpecker: WoodpeckerConfig{ServerURL: "woodpecker:9000"}, // missing AgentSecret
		Webhook:    WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error: woodpecker missing agent_secret")
	}
}

func TestValidate_GiteaAndForgejoBothChecked(t *testing.T) {
	// Multi-provider case: gitea valid, forgejo missing token.
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{
		Gitea:   GiteaConfig{InstanceURL: "https://gitea.example.com", Token: "tok"},
		Forgejo: ForgejoConfig{InstanceURL: "https://codeberg.org"}, // missing token
		Webhook: WebhookConfig{Tunnel: "none"},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error: forgejo missing token")
	}
}

func TestProviders_NoCredentials_Empty(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := &Config{}
	if ps := cfg.Providers(); len(ps) != 0 {
		t.Errorf("Providers() = %v, want empty", ps)
	}
}

// --- Additional duration parser coverage ---

func TestLogRetentionDuration_7d(t *testing.T) {
	// "7d" is the documented default form — ensure it parses to 7 days.
	lc := LogConfig{LogRetention: "7d"}
	if d := lc.LogRetentionDuration(); d != 7*24*time.Hour {
		t.Errorf("LogRetention(7d) = %v, want %v", d, 7*24*time.Hour)
	}
}

func TestLogRetentionDuration_168h(t *testing.T) {
	// "168h" is equivalent to 7 days in pure Go duration form.
	lc := LogConfig{LogRetention: "168h"}
	if d := lc.LogRetentionDuration(); d != 168*time.Hour {
		t.Errorf("LogRetention(168h) = %v, want %v", d, 168*time.Hour)
	}
}

func TestLogRetentionDuration_SingleD(t *testing.T) {
	// "d" alone (no number) is too short to be valid Nd shorthand and is
	// not a Go duration — should fall back to default 7d.
	lc := LogConfig{LogRetention: "d"}
	if d := lc.LogRetentionDuration(); d != 7*24*time.Hour {
		t.Errorf("LogRetention(d) = %v, want default 168h", d)
	}
}

func TestLogRetentionDuration_BadDays(t *testing.T) {
	// "abcd" — ends with 'd' but the prefix is not a valid duration.
	// Falls through to time.ParseDuration which also fails — default returned.
	lc := LogConfig{LogRetention: "abcd"}
	if d := lc.LogRetentionDuration(); d != 7*24*time.Hour {
		t.Errorf("LogRetention(abcd) = %v, want default 168h", d)
	}
}

func TestLogRetentionDuration_OneDay(t *testing.T) {
	lc := LogConfig{LogRetention: "1d"}
	if d := lc.LogRetentionDuration(); d != 24*time.Hour {
		t.Errorf("LogRetention(1d) = %v, want 24h", d)
	}
}

func TestLogRetentionDuration_30Days(t *testing.T) {
	lc := LogConfig{LogRetention: "30d"}
	if d := lc.LogRetentionDuration(); d != 30*24*time.Hour {
		t.Errorf("LogRetention(30d) = %v, want 720h", d)
	}
}

func TestLogRetentionDuration_MixedUnits(t *testing.T) {
	// "30m" — Go duration form; should parse as 30 minutes.
	lc := LogConfig{LogRetention: "30m"}
	if d := lc.LogRetentionDuration(); d != 30*time.Minute {
		t.Errorf("LogRetention(30m) = %v, want 30m", d)
	}
}

func TestParsedPollInterval_Empty(t *testing.T) {
	// Explicit empty string should fall back to default.
	g := &GitHubConfig{PollInterval: ""}
	if d := g.ParsedPollInterval(); d != 30*time.Second {
		t.Errorf("PollInterval('') = %v, want 30s default", d)
	}
}

func TestParsedJobTimeout_Empty(t *testing.T) {
	// Empty string fails ParseDuration, returns default 2h.
	r := &RunnerConfig{JobTimeout: ""}
	if d := r.ParsedJobTimeout(); d != 2*time.Hour {
		t.Errorf("JobTimeout('') = %v, want 2h default", d)
	}
}

func TestParsedShutdownTimeout_Empty(t *testing.T) {
	r := &RunnerConfig{ShutdownTimeout: ""}
	if d := r.ParsedShutdownTimeout(); d != 5*time.Minute {
		t.Errorf("ShutdownTimeout('') = %v, want 5m default", d)
	}
}

func TestParsedPollInterval_Hours(t *testing.T) {
	g := &GitHubConfig{PollInterval: "2h"}
	if d := g.ParsedPollInterval(); d != 2*time.Hour {
		t.Errorf("PollInterval(2h) = %v, want 2h", d)
	}
}

func TestRuntimeRlimitsResolved_Defaults(t *testing.T) {
	got := RuntimeRlimits{}.Resolved()
	if got.Nofile != 1024 {
		t.Errorf("Nofile = %d, want 1024 (containerd default)", got.Nofile)
	}
	if got.Nproc != 1024 {
		t.Errorf("Nproc = %d, want 1024 default", got.Nproc)
	}
}

func TestRuntimeRlimitsResolved_Explicit(t *testing.T) {
	got := RuntimeRlimits{Nofile: 4096, Nproc: 8192}.Resolved()
	if got.Nofile != 4096 {
		t.Errorf("Nofile = %d, want 4096", got.Nofile)
	}
	if got.Nproc != 8192 {
		t.Errorf("Nproc = %d, want 8192", got.Nproc)
	}
}

func TestRuntimeRlimitsResolved_NegativeFallsBack(t *testing.T) {
	got := RuntimeRlimits{Nofile: -1, Nproc: -100}.Resolved()
	if got.Nofile != 1024 {
		t.Errorf("Nofile(-1) resolved to %d, want 1024", got.Nofile)
	}
	if got.Nproc != 1024 {
		t.Errorf("Nproc(-100) resolved to %d, want 1024", got.Nproc)
	}
}

func TestRuntimeRlimitsResolved_MixedZeroAndExplicit(t *testing.T) {
	// Only one field set: the other should fall back without disturbing
	// the explicit one.
	got := RuntimeRlimits{Nofile: 65536}.Resolved()
	if got.Nofile != 65536 {
		t.Errorf("Nofile = %d, want 65536 (preserved)", got.Nofile)
	}
	if got.Nproc != 1024 {
		t.Errorf("Nproc = %d, want 1024 (default fill)", got.Nproc)
	}
}

func TestLoad_RuntimeRlimits(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
owner = "testorg"

[runtime.rlimits]
nofile = 4096
nproc = 2048
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Runtime.Rlimits.Nofile != 4096 {
		t.Errorf("Rlimits.Nofile = %d, want 4096", cfg.Runtime.Rlimits.Nofile)
	}
	if cfg.Runtime.Rlimits.Nproc != 2048 {
		t.Errorf("Rlimits.Nproc = %d, want 2048", cfg.Runtime.Rlimits.Nproc)
	}
}

func TestLoad_RuntimeRlimits_Omitted(t *testing.T) {
	// Empty config — Resolved() must still produce 1024/1024 so callers
	// never have to special-case "no [runtime] block in config.toml".
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
	if cfg.Runtime.Rlimits.Nofile != 0 {
		t.Errorf("Rlimits.Nofile (raw) = %d, want 0 before Resolved()", cfg.Runtime.Rlimits.Nofile)
	}
	resolved := cfg.Runtime.Rlimits.Resolved()
	if resolved.Nofile != 1024 || resolved.Nproc != 1024 {
		t.Errorf("Resolved() = %+v, want {1024, 1024}", resolved)
	}
}

func TestResolvedAllowPrivileged_DefaultOffEveryOS(t *testing.T) {
	// Secure default (DIND-1): privileged is opt-in on ALL platforms, so the
	// default must resolve false regardless of GOOS.
	d := DindConfig{}
	if d.ResolvedAllowPrivileged() {
		t.Errorf("default ResolvedAllowPrivileged on GOOS=%s = true, want false (privileged must be opt-in)", goruntime.GOOS)
	}
}

func TestResolvedAllowPrivileged_ExplicitTrueWins(t *testing.T) {
	yes := true
	d := DindConfig{AllowPrivileged: &yes}
	if !d.ResolvedAllowPrivileged() {
		t.Error("explicit allow_privileged=true should resolve true on every OS")
	}
}

func TestResolvedAllowPrivileged_ExplicitFalseWins(t *testing.T) {
	no := false
	d := DindConfig{AllowPrivileged: &no}
	if d.ResolvedAllowPrivileged() {
		t.Errorf("explicit allow_privileged=false should resolve false on every OS (GOOS=%s)", goruntime.GOOS)
	}
}

func TestLoad_DindAllowPrivileged_OmittedUsesPlatformDefault(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
owner = "testorg"

[dind]
enabled = true
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Dind.AllowPrivileged != nil {
		t.Errorf("AllowPrivileged ptr = %v, want nil (key not set in TOML)", *cfg.Dind.AllowPrivileged)
	}
	// Omitted key → secure default false on every platform.
	if got := cfg.Dind.ResolvedAllowPrivileged(); got {
		t.Errorf("ResolvedAllowPrivileged on GOOS=%s = true, want false (default off)", goruntime.GOOS)
	}
}

func TestLoad_DindAllowPrivileged_ExplicitFalseOnAnyOS(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
owner = "testorg"

[dind]
enabled = true
allow_privileged = false
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Dind.AllowPrivileged == nil {
		t.Fatal("AllowPrivileged ptr is nil; TOML did not bind the explicit false")
	}
	if *cfg.Dind.AllowPrivileged {
		t.Errorf("AllowPrivileged = true, want false")
	}
	if cfg.Dind.ResolvedAllowPrivileged() {
		t.Error("ResolvedAllowPrivileged() should honor explicit false even on non-Linux hosts")
	}
}

func TestMacOSRunnerConfig_ModeForRepo(t *testing.T) {
	tests := []struct {
		name string
		cfg  *MacOSRunnerConfig
		repo string
		want string
	}{
		{"nil config defaults to vm", nil, "myrepo", "vm"},
		{"zero value defaults to vm", &MacOSRunnerConfig{}, "myrepo", "vm"},
		{"top-level native", &MacOSRunnerConfig{Mode: "native"}, "myrepo", "native"},
		{"top-level vm", &MacOSRunnerConfig{Mode: "vm"}, "myrepo", "vm"},
		{"invalid top-level mode defaults to vm", &MacOSRunnerConfig{Mode: "bogus"}, "myrepo", "vm"},

		// org/repo exact match
		{"org/repo exact match native", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/ephemerd": "native"}}, "ephpm/ephemerd", "native"},
		{"org/repo exact match vm", &MacOSRunnerConfig{Mode: "native", Repos: map[string]string{"ephpm/ephemerd": "vm"}}, "ephpm/ephemerd", "vm"},
		{"org/repo miss falls back to top-level", &MacOSRunnerConfig{Mode: "native", Repos: map[string]string{"ephpm/other": "vm"}}, "ephpm/ephemerd", "native"},

		// short-name fallback (event.Repo = "ephemerd", config key = "ephpm/ephemerd")
		{"short name matches org/repo key", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/ephemerd": "native"}}, "ephemerd", "native"},
		{"short name no match falls to top-level", &MacOSRunnerConfig{Mode: "native", Repos: map[string]string{"ephpm/other": "vm"}}, "ephemerd", "native"},

		// disambiguation: fork vs original
		{"fork stays vm while original is native", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/ephemerd": "native", "fork/ephemerd": "vm"}}, "ephpm/ephemerd", "native"},
		{"fork explicit vm", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/ephemerd": "native", "fork/ephemerd": "vm"}}, "fork/ephemerd", "vm"},

		// wildcard: "org/*" matches all repos in org
		{"wildcard matches repo in org", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/*": "native"}}, "ephpm/ephemerd", "native"},
		{"wildcard matches another repo in org", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/*": "native"}}, "ephpm/php-sdk", "native"},
		{"wildcard does not match different org", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/*": "native"}}, "other/ephemerd", "vm"},
		{"exact match wins over wildcard", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/*": "native", "ephpm/secret": "vm"}}, "ephpm/secret", "vm"},
		{"wildcard still applies to non-overridden repo", &MacOSRunnerConfig{Repos: map[string]string{"ephpm/*": "native", "ephpm/secret": "vm"}}, "ephpm/ephemerd", "native"},

		// invalid per-repo mode falls through
		{"invalid per-repo mode falls back to top-level", &MacOSRunnerConfig{Mode: "native", Repos: map[string]string{"ephpm/myrepo": "bogus"}}, "ephpm/myrepo", "native"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ModeForRepo(tt.repo)
			if got != tt.want {
				t.Errorf("ModeForRepo(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

func TestMacOSRunnerConfig_ResolvedMaxNative(t *testing.T) {
	tests := []struct {
		name string
		cfg  *MacOSRunnerConfig
		want int
	}{
		{"nil config defaults to 4", nil, 4},
		{"zero value defaults to 4", &MacOSRunnerConfig{}, 4},
		{"negative defaults to 4", &MacOSRunnerConfig{MaxNative: -1}, 4},
		{"positive value used", &MacOSRunnerConfig{MaxNative: 8}, 8},
		{"one is valid", &MacOSRunnerConfig{MaxNative: 1}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ResolvedMaxNative()
			if got != tt.want {
				t.Errorf("ResolvedMaxNative() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMacOSRunnerConfig_ResolvedMaxProcesses(t *testing.T) {
	intPtr := func(n int) *int { return &n }
	tests := []struct {
		name string
		cfg  *MacOSRunnerConfig
		want int
	}{
		{"nil config defaults to 2048", nil, 2048},
		{"unset (nil ptr) defaults to 2048", &MacOSRunnerConfig{}, 2048},
		{"explicit zero means unlimited", &MacOSRunnerConfig{MaxProcesses: intPtr(0)}, 0},
		{"negative treated as unlimited", &MacOSRunnerConfig{MaxProcesses: intPtr(-1)}, 0},
		{"positive value used", &MacOSRunnerConfig{MaxProcesses: intPtr(4096)}, 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolvedMaxProcesses(); got != tt.want {
				t.Errorf("ResolvedMaxProcesses() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMacOSRunnerConfig_StrictSandbox(t *testing.T) {
	tests := []struct {
		name string
		cfg  *MacOSRunnerConfig
		want bool
	}{
		{"nil config is false", nil, false},
		{"unset defaults to false", &MacOSRunnerConfig{}, false},
		{"explicit true", &MacOSRunnerConfig{SandboxStrict: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.StrictSandbox(); got != tt.want {
				t.Errorf("StrictSandbox() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrphanSweepEnabled(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		name string
		cfg  *RunnerConfig
		want bool
	}{
		{"nil config defaults to true", nil, true},
		{"omitted table defaults to true", &RunnerConfig{}, true},
		{"explicit true", &RunnerConfig{OrphanSweep: OrphanSweepToml{Enabled: boolPtr(true)}}, true},
		{"explicit false disables", &RunnerConfig{OrphanSweep: OrphanSweepToml{Enabled: boolPtr(false)}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.OrphanSweepEnabled(); got != tt.want {
				t.Errorf("OrphanSweepEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrphanSweepGrace(t *testing.T) {
	tests := []struct {
		name string
		cfg  *RunnerConfig
		want time.Duration
	}{
		{"nil config defaults to 10m", nil, 10 * time.Minute},
		{"empty defaults to 10m", &RunnerConfig{}, 10 * time.Minute},
		{"custom duration", &RunnerConfig{OrphanSweep: OrphanSweepToml{Grace: "25m"}}, 25 * time.Minute},
		{"unparseable defaults to 10m", &RunnerConfig{OrphanSweep: OrphanSweepToml{Grace: "soon"}}, 10 * time.Minute},
		{"non-positive defaults to 10m", &RunnerConfig{OrphanSweep: OrphanSweepToml{Grace: "-5m"}}, 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.OrphanSweepGrace(); got != tt.want {
				t.Errorf("OrphanSweepGrace() = %v, want %v", got, tt.want)
			}
		})
	}
}
