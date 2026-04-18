// Command ghtoken mints a GitHub App installation token from ephemerd's
// config and prints it to stdout. Useful for ad-hoc API calls (e.g.
// triggering workflow_dispatch) on machines where `gh auth login` isn't
// practical.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/github"
)

func main() {
	configPath := os.Getenv("EPHEMERD_CONFIG")
	if configPath == "" {
		dataDir := os.Getenv("EPHEMERD_DATA_DIR")
		if dataDir == "" {
			dataDir = "/var/lib/ephemerd"
		}
		configPath = dataDir + "/config.toml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config %s: %v\n", configPath, err)
		os.Exit(1)
	}

	if cfg.GitHub.AppID == 0 {
		fmt.Fprintln(os.Stderr, "error: github.app_id not set in config — ghtoken requires GitHub App auth")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.PrivateKeyPath, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer auth.Stop()

	fmt.Print(auth.Token())
}
