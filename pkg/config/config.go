package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	goruntime "runtime"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub     GitHubConfig     `toml:"github"`
	Forgejo    ForgejoConfig    `toml:"forgejo"`
	Gitea      GiteaConfig      `toml:"gitea"`
	GitLab     GitLabConfig     `toml:"gitlab"`
	Woodpecker WoodpeckerConfig `toml:"woodpecker"`
	Webhook    WebhookConfig    `toml:"webhook"`
	Containerd ContainerdConfig `toml:"containerd"`
	Network    NetworkConfig    `toml:"network"`
	VM         VMConfig         `toml:"vm"`
	Dind        DindConfig        `toml:"dind"`
	ModuleProxy ModuleProxyConfig `toml:"module_proxy"`
	Runner      RunnerConfig      `toml:"runner"`
	Metrics    MetricsConfig    `toml:"metrics"`
	Log        LogConfig        `toml:"log"`
}

// Provider returns the name of the configured forge provider.
// Exactly one provider section should have credentials set.
// Returns "github" by default for backward compatibility.
func (c *Config) Provider() string {
	switch {
	case c.Forgejo.InstanceURL != "":
		return "forgejo"
	case c.Gitea.InstanceURL != "":
		return "gitea"
	case c.GitLab.InstanceURL != "":
		return "gitlab"
	case c.Woodpecker.ServerURL != "":
		return "woodpecker"
	default:
		return "github"
	}
}

// MetricsConfig configures the Prometheus metrics endpoint.
// Disabled by default. Set enabled = true to expose /metrics.
type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`  // enable /metrics endpoint (default false)
	Port    int    `toml:"port"`     // listen port (default 9090)
	Path    string `toml:"path"`     // metrics path (default "/metrics")
	TLSCert string `toml:"tls_cert"` // TLS certificate path (optional)
	TLSKey  string `toml:"tls_key"`  // TLS private key path (optional)
}

// WebhookConfig configures webhook delivery and tunnel providers.
// By default, ephemerd uses polling (tunnel = "none").
// Set tunnel = "localtunnel" or "ngrok" for instant webhook delivery.
type WebhookConfig struct {
	Secret            string `toml:"secret"`             // webhook HMAC secret (auto-generated if empty)
	Port              int    `toml:"port"`               // listen port for health endpoint (default 8080)
	TLSCert           string `toml:"tls_cert"`           // TLS certificate path (direct TLS, no tunnel)
	TLSKey            string `toml:"tls_key"`            // TLS private key path
	Tunnel            string `toml:"tunnel"`             // "none" (default, polling), "localtunnel", or "ngrok"
	TunnelURL         string `toml:"tunnel_url"`         // localtunnel: self-hosted server URL
	NgrokAuthtoken    string `toml:"ngrok_authtoken"`    // ngrok auth token (or use NGROK_AUTHTOKEN env)
	TunnelMaxRetries  int    `toml:"tunnel_max_retries"` // max consecutive reconnect failures before falling back to polling (default 5)
}

// NetworkConfig configures container networking.
type NetworkConfig struct {
	Subnet string `toml:"subnet"` // container subnet (auto-selected if empty)
	MTU    int    `toml:"mtu"`    // bridge MTU (auto-detected from host if 0)
}

type ContainerdConfig struct {
	// Reserved for future containerd-specific settings (e.g. snapshotter overrides)
}

// DindConfig configures the fake Docker daemon mounted into job containers.
type DindConfig struct {
	Enabled bool `toml:"enabled"` // mount /var/run/docker.sock with a fake Docker API
}

// ModuleProxyConfig configures the Go module caching proxy.
// When enabled, ephemerd runs a local GOPROXY on the bridge gateway that
// caches module downloads. Containers receive GOPROXY env var automatically.
type ModuleProxyConfig struct {
	Enabled  bool   `toml:"enabled"`  // enable Go module caching proxy
	Port     int    `toml:"port"`     // listen port on bridge gateway (default 8082)
	Upstream string `toml:"upstream"` // upstream proxy URL (default "https://proxy.golang.org")
	Cleanup  bool   `toml:"cleanup"`  // wipe cache on shutdown (default true)
}

// VMConfig configures virtual machines for cross-OS job execution.
type VMConfig struct {
	Linux LinuxVMToml `toml:"linux"`
	MacOS MacOSVMToml `toml:"macos"`
}

// LinuxVMToml configures the long-running Linux VM for Linux jobs
// on Windows (Hyper-V) and macOS (Virtualization.framework) hosts.
type LinuxVMToml struct {
	Enabled    bool   `toml:"enabled"`     // enable Linux VM for cross-OS Linux jobs
	CPUs       uint   `toml:"cpus"`        // virtual CPUs (default: 2)
	MemoryMB   uint64 `toml:"memory_mb"`   // memory in MB (default: 2048)
	DiskSizeGB uint64 `toml:"disk_size_gb"` // sparse disk size in GB (default: 50)
}

// MacOSVMToml configures per-job macOS VMs on macOS hosts.
type MacOSVMToml struct {
	Enabled   bool   `toml:"enabled"`    // enable macOS-native jobs
	BaseImage string `toml:"base_image"` // path to provisioned macOS disk image
	CPUs      uint   `toml:"cpus"`       // CPUs per VM (default: 4)
	MemoryMB  uint64 `toml:"memory_mb"`  // memory per VM in MB (default: 8192)
}

type GitHubConfig struct {
	// Authentication: either a PAT or GitHub App
	Token          string `toml:"token"`
	AppID          int64  `toml:"app_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`

	// Which org/user and repos to register runners for
	Owner string   `toml:"owner"`
	Repos []string `toml:"repos"`

	// Job discovery: polling interval (default "10s")
	PollInterval string `toml:"poll_interval"`
}

// ForgejoConfig configures the Forgejo Actions provider.
// Set instance_url and token to enable Forgejo instead of GitHub.
// Uses forgejo-runner binary with one-job --handle mode.
type ForgejoConfig struct {
	InstanceURL string   `toml:"instance_url"` // Forgejo instance URL (e.g., "https://codeberg.org")
	Token       string   `toml:"token"`        // runner registration token from Forgejo admin
	Owner       string   `toml:"owner"`        // org or user (empty = instance-level runner)
	Repos       []string `toml:"repos"`        // limit to specific repos (empty = all)
	JobImage    string   `toml:"job_image"`    // default job execution image (default: "gitea/runner-images:ubuntu-24.04")
}

// GiteaConfig configures the Gitea Actions provider.
// Set instance_url and token to enable Gitea instead of GitHub.
// Uses act_runner binary with --ephemeral mode.
type GiteaConfig struct {
	InstanceURL string   `toml:"instance_url"` // Gitea instance URL (e.g., "https://gitea.example.com")
	Token       string   `toml:"token"`        // runner registration token from Gitea admin
	Owner       string   `toml:"owner"`        // org or user (empty = instance-level runner)
	Repos       []string `toml:"repos"`        // limit to specific repos (empty = all)
	JobImage    string   `toml:"job_image"`    // default job execution image (default: "gitea/runner-images:ubuntu-24.04")
}

// GitLabConfig configures the GitLab CI provider.
// Set instance_url and token to enable GitLab instead of GitHub.
type GitLabConfig struct {
	InstanceURL string   `toml:"instance_url"` // GitLab instance URL (e.g., "https://gitlab.com")
	Token       string   `toml:"token"`        // runner authentication token (glrt-xxx for GitLab 16+)
	Tags        []string `toml:"tags"`         // runner tags for job matching
}

// WoodpeckerConfig configures the Woodpecker CI provider.
// Set server_url and agent_secret to enable Woodpecker instead of GitHub.
// Woodpecker requires a forge backend (Gitea/Forgejo) for repo management;
// ephemerd manages the agent lifecycle, not the server.
type WoodpeckerConfig struct {
	ServerURL   string `toml:"server_url"`   // Woodpecker server gRPC URL (e.g., "woodpecker.example.com:9000")
	AgentSecret string `toml:"agent_secret"` // shared secret for agent authentication
}

type RunnerConfig struct {
	MaxConcurrent   int      `toml:"max_concurrent"`
	ExtraLabels     []string `toml:"extra_labels"`
	DefaultImage    string   `toml:"default_image"`
	JobTimeout      string   `toml:"job_timeout"`
	ShutdownTimeout string   `toml:"shutdown_timeout"`
}

// ParsedPollInterval returns the poll interval as a time.Duration.
func (g *GitHubConfig) ParsedPollInterval() time.Duration {
	if g.PollInterval == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(g.PollInterval)
	if err != nil {
		return 10 * time.Second
	}
	return d
}

// ParsedJobTimeout returns the job timeout as a time.Duration.
func (r *RunnerConfig) ParsedJobTimeout() time.Duration {
	d, err := time.ParseDuration(r.JobTimeout)
	if err != nil {
		return 2 * time.Hour
	}
	return d
}

// ParsedShutdownTimeout returns the shutdown timeout as a time.Duration.
func (r *RunnerConfig) ParsedShutdownTimeout() time.Duration {
	d, err := time.ParseDuration(r.ShutdownTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

type LogConfig struct {
	Level        string `toml:"level"`
	Format       string `toml:"format"`        // "text" or "json"
	LogRetention string `toml:"log_retention"` // max age for job log files (e.g. "7d", "24h"); default "7d"
}

// LogRetentionDuration returns the parsed log retention duration.
// Supports Go duration strings (e.g. "168h") and a "d" suffix for days (e.g. "7d").
// Returns 7 days if the value is empty or invalid.
func (lc LogConfig) LogRetentionDuration() time.Duration {
	s := lc.LogRetention
	if s == "" {
		return 7 * 24 * time.Hour
	}
	// Support "Nd" shorthand for days.
	if len(s) > 1 && s[len(s)-1] == 'd' {
		if d, err := time.ParseDuration(s[:len(s)-1] + "h"); err == nil {
			return d * 24
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 7 * 24 * time.Hour
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Runner: RunnerConfig{
			MaxConcurrent:   4,
			JobTimeout:      "2h",
			ShutdownTimeout: "5m",
		},
		Webhook: WebhookConfig{
			Port:   8080,
			Tunnel: "none",
		},
		Metrics: MetricsConfig{
			Port: 9090,
			Path: "/metrics",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				slog.Warn("config file not found, using defaults", "path", path)
				return cfg, nil
			}
			return nil, fmt.Errorf("reading config: %w", err)
		}
		if err := toml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	switch c.Provider() {
	case "github":
		if err := c.validateGitHub(); err != nil {
			return err
		}
	case "forgejo":
		if c.Forgejo.Token == "" {
			return fmt.Errorf("forgejo.token is required")
		}
	case "gitea":
		if c.Gitea.Token == "" {
			return fmt.Errorf("gitea.token is required")
		}
	case "gitlab":
		if c.GitLab.Token == "" {
			return fmt.Errorf("gitlab.token is required")
		}
	case "woodpecker":
		if c.Woodpecker.AgentSecret == "" {
			return fmt.Errorf("woodpecker.agent_secret is required")
		}
	}

	// Generate a random webhook secret if not explicitly set and tunnel is active
	if c.Webhook.Secret == "" && c.Webhook.Tunnel != "none" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating webhook secret: %w", err)
		}
		c.Webhook.Secret = hex.EncodeToString(b)
	}

	return nil
}

func (c *Config) validateGitHub() error {
	// Fall back to GITHUB_TOKEN env var if no token is configured
	if c.GitHub.Token == "" {
		c.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 {
		return fmt.Errorf("github.token or github.app_id is required (or set GITHUB_TOKEN env var)")
	}
	if c.GitHub.AppID != 0 {
		if c.GitHub.InstallationID == 0 {
			return fmt.Errorf("github.installation_id is required when using github.app_id")
		}
		if c.GitHub.PrivateKeyPath == "" {
			return fmt.Errorf("github.private_key_path is required when using github.app_id")
		}
	}
	if c.GitHub.Owner == "" {
		return fmt.Errorf("github.owner is required")
	}
	// repos is optional — if empty, ephemerd registers org-level runners
	return nil
}

func (c *Config) Logger() *slog.Logger {
	var level slog.Level
	switch c.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	// On Windows, slog's \n doesn't include \r, causing stair-step output
	// in PowerShell/cmd.exe terminals. Wrap stderr to fix line endings.
	var w io.Writer = os.Stderr
	if goruntime.GOOS == "windows" {
		w = &crlfWriter{w: os.Stderr}
	}

	var handler slog.Handler
	if c.Log.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	return slog.New(handler)
}

// crlfWriter wraps a writer to replace bare \n with \r\n for Windows terminals.
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	replaced := bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	_, err := c.w.Write(replaced)
	return len(p), err
}
