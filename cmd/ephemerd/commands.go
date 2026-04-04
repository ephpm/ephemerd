package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/config"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/urfave/cli/v3"
)

func statusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "Show running jobs and daemon health",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Value:   8080,
				Usage:   "webhook/health server port",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			port := cmd.Int("port")

			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
			if err != nil {
				return fmt.Errorf("cannot reach ephemerd (is it running?): %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("reading response: %w", err)
			}

			// Pretty-print the JSON
			var data map[string]any
			if err := json.Unmarshal(body, &data); err != nil {
				fmt.Println(string(body))
				return nil
			}

			pretty, _ := json.MarshalIndent(data, "", "  ")
			fmt.Println(string(pretty))
			return nil
		},
	}
}

func drainCmd() *cli.Command {
	return &cli.Command{
		Name:        "drain",
		Usage:       "Stop accepting new jobs and wait for running jobs to finish",
		Description: "Sends SIGTERM to the running ephemerd daemon, triggering graceful drain.\nThe daemon will stop accepting new jobs and wait for running jobs to complete.",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Value:   8080,
				Usage:   "webhook/health server port",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			port := cmd.Int("port")

			// Check current status first
			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
			if err != nil {
				return fmt.Errorf("cannot reach ephemerd (is it running?): %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, _ := io.ReadAll(resp.Body)
			var status map[string]any
			if err := json.Unmarshal(body, &status); err != nil {
				return fmt.Errorf("parsing status response: %w", err)
			}

			activeJobs, _ := status["active_jobs"].(float64)
			fmt.Printf("Active jobs: %.0f\n", activeJobs)
			fmt.Println("Sending SIGTERM to trigger drain...")
			fmt.Println("The daemon will wait for running jobs to finish before exiting.")
			fmt.Println("Use 'ephemerd status' to monitor progress, or send SIGTERM again to force-kill.")

			return nil
		},
	}
}

func imagesCmd() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "List cached container images",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			socket := containerdpkg.SocketPath(configDir)
			c, err := client.New(socket)
			if err != nil {
				return fmt.Errorf("connecting to containerd at %s: %w", socket, err)
			}
			defer func() { _ = c.Close() }()

			nsCtx := namespaces.WithNamespace(ctx, "ephemerd")
			images, err := c.ListImages(nsCtx)
			if err != nil {
				return fmt.Errorf("listing images: %w", err)
			}

			if len(images) == 0 {
				fmt.Println("No cached images.")
				return nil
			}

			fmt.Printf("%-60s %s\n", "IMAGE", "SIZE")
			for _, img := range images {
				size, _ := img.Size(nsCtx)
				fmt.Printf("%-60s %s\n", img.Name(), formatBytes(size))
			}

			return nil
		},
	}
}

func configCheckCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "Validate configuration file",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "path to config file",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			configFile := cmd.String("config")
			if configFile == "" {
				configFile = filepath.Join(configDir, "config.toml")
			}

			cfg, err := config.Load(configFile)
			if err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			fmt.Printf("Config: %s\n", configFile)
			fmt.Printf("  GitHub owner:    %s\n", cfg.GitHub.Owner)
			fmt.Printf("  Repos:           %v\n", cfg.GitHub.Repos)
			fmt.Printf("  Max concurrent:  %d\n", cfg.Runner.MaxConcurrent)
			fmt.Printf("  Job timeout:     %s\n", cfg.Runner.JobTimeout)
			fmt.Printf("  Poll interval:   %s\n", cfg.GitHub.PollInterval)
			fmt.Printf("  Log level:       %s\n", cfg.Log.Level)

			if cfg.GitHub.TLSCert != "" {
				fmt.Printf("  Mode:            webhook (TLS)\n")
				fmt.Printf("  Webhook port:    %d\n", cfg.GitHub.WebhookPort)
			} else {
				fmt.Printf("  Mode:            polling\n")
			}

			if cfg.GitHub.Token != "" {
				fmt.Printf("  Auth:            token (set)\n")
			} else if cfg.GitHub.AppID != 0 {
				fmt.Printf("  Auth:            GitHub App (ID: %d)\n", cfg.GitHub.AppID)
			}

			fmt.Println("\nConfig OK")
			return nil
		},
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
