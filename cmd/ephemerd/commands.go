package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/config"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/spf13/cobra"
)

func statusCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show running jobs and daemon health",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
			if err != nil {
				return fmt.Errorf("cannot reach ephemerd (is it running?): %w", err)
			}
			defer resp.Body.Close()

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

	cmd.Flags().IntVarP(&port, "port", "p", 8080, "webhook/health server port")
	return cmd
}

func drainCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Stop accepting new jobs and wait for running jobs to finish",
		Long:  "Sends SIGTERM to the running ephemerd daemon, triggering graceful drain.\nThe daemon will stop accepting new jobs and wait for running jobs to complete.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check current status first
			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
			if err != nil {
				return fmt.Errorf("cannot reach ephemerd (is it running?): %w", err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			var status map[string]any
			json.Unmarshal(body, &status)

			activeJobs, _ := status["active_jobs"].(float64)
			fmt.Printf("Active jobs: %.0f\n", activeJobs)
			fmt.Println("Sending SIGTERM to trigger drain...")
			fmt.Println("The daemon will wait for running jobs to finish before exiting.")
			fmt.Println("Use 'ephemerd status' to monitor progress, or send SIGTERM again to force-kill.")

			return nil
		},
	}

	cmd.Flags().IntVarP(&port, "port", "p", 8080, "webhook/health server port")
	return cmd
}

func imagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "List cached container images",
		RunE: func(cmd *cobra.Command, args []string) error {
			socket := containerdpkg.SocketPath(configDir)
			c, err := client.New(socket)
			if err != nil {
				return fmt.Errorf("connecting to containerd at %s: %w", socket, err)
			}
			defer c.Close()

			ctx := namespaces.WithNamespace(cmd.Context(), "ephemerd")
			images, err := c.ListImages(ctx)
			if err != nil {
				return fmt.Errorf("listing images: %w", err)
			}

			if len(images) == 0 {
				fmt.Println("No cached images.")
				return nil
			}

			fmt.Printf("%-60s %s\n", "IMAGE", "SIZE")
			for _, img := range images {
				size, _ := img.Size(ctx)
				fmt.Printf("%-60s %s\n", img.Name(), formatBytes(size))
			}

			return nil
		},
	}

	return cmd
}

func configCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Validate configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			configFile, _ := cmd.Flags().GetString("config")
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
			fmt.Printf("  Default image:   %s\n", cfg.Runner.DefaultImage)
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

	cmd.Flags().StringP("config", "c", "", "path to config file")
	return cmd
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
