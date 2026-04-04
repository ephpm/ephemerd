package config

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub     GitHubConfig     `toml:"github"`
	Containerd ContainerdConfig `toml:"containerd"`
	Runner     RunnerConfig     `toml:"runner"`
	Log        LogConfig        `toml:"log"`
}

type ContainerdConfig struct {
	// Reserved for future containerd-specific settings (e.g. snapshotter overrides)
}

type GitHubConfig struct {
	// Authentication: either a PAT or GitHub App
	Token          string `toml:"token"`
	AppID          int64  `toml:"app_id"`
	PrivateKeyPath string `toml:"private_key_path"`

	// Which org/user and repos to register runners for
	Owner string   `toml:"owner"`
	Repos []string `toml:"repos"`

	// Webhook listener for workflow_job events
	WebhookPort   int    `toml:"webhook_port"`
	WebhookSecret string `toml:"webhook_secret"`
}

type RunnerConfig struct {
	DefaultImage  string   `toml:"default_image"`
	MaxConcurrent int      `toml:"max_concurrent"`
	ExtraLabels   []string `toml:"extra_labels"`
	JobTimeout    string   `toml:"job_timeout"`
}

type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"` // "text" or "json"
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Runner: RunnerConfig{
			MaxConcurrent: 4,
			JobTimeout:    "2h",
		},
		GitHub: GitHubConfig{
			WebhookPort: 8080,
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
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 {
		return fmt.Errorf("github.token or github.app_id is required")
	}
	if c.GitHub.Owner == "" {
		return fmt.Errorf("github.owner is required")
	}
	if len(c.GitHub.Repos) == 0 {
		return fmt.Errorf("github.repos must have at least one entry")
	}
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

	var handler slog.Handler
	if c.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	return slog.New(handler)
}
