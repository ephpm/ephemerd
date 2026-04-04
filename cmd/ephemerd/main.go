package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	configDir string
)

func main() {
	root := &cobra.Command{
		Use:     "ephemerd",
		Short:   "Ephemeral GitHub Actions runner daemon",
		Version: version,
	}

	root.PersistentFlags().StringVar(&configDir, "data-dir", defaultDataDir(), "data directory for ephemerd state")

	root.AddCommand(
		serveCmd(),
		ctrctlCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the ephemerd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve(cmd.Context(), configFile)
		},
	}

	cmd.Flags().StringVarP(&configFile, "config", "c", "", "path to config file (default: <data-dir>/config.toml)")

	return cmd
}

func serve(ctx context.Context, configFile string) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load configuration
	if configFile == "" {
		configFile = filepath(configDir, "config.toml")
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	log := cfg.Logger()
	log.Info("starting ephemerd", "version", version, "data_dir", configDir)

	// Start embedded containerd
	ctrd, err := containerd.New(containerd.Config{
		DataDir: configDir,
		Log:     log,
	})
	if err != nil {
		return fmt.Errorf("starting containerd: %w", err)
	}
	defer ctrd.Stop()

	log.Info("containerd started")

	// Create runtime (container lifecycle manager)
	rt, err := runtime.New(runtime.Config{
		Client:       ctrd.Client(),
		DefaultImage: cfg.Runner.DefaultImage,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("creating runtime: %w", err)
	}

	// Create GitHub client
	gh, err := github.New(github.Config{
		Token: cfg.GitHub.Token,
		Owner: cfg.GitHub.Owner,
		Repos: cfg.GitHub.Repos,
		Log:   log,
	})
	if err != nil {
		return fmt.Errorf("creating github client: %w", err)
	}

	// Start scheduler (ties GitHub jobs to container lifecycle)
	sched := scheduler.New(scheduler.Config{
		Runtime:       rt,
		GitHub:        gh,
		MaxConcurrent: cfg.Runner.MaxConcurrent,
		Labels:        cfg.Runner.ExtraLabels,
		Log:           log,
	})

	log.Info("ephemerd ready", "repos", cfg.GitHub.Repos, "max_concurrent", cfg.Runner.MaxConcurrent)

	return sched.Run(ctx)
}

// ctrctlCmd provides direct access to the embedded containerd for debugging.
// Similar to rke2's "rke2 crictl" passthrough.
func ctrctlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "ctrctl",
		Short:              "Access the embedded containerd (passthrough to ctr)",
		Long:               "Runs ctr commands against ephemerd's embedded containerd instance.\nAll arguments after 'ctrctl' are passed directly to ctr.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			socketPath := containerd.SocketPath(configDir)
			return containerd.ExecCtr(socketPath, args)
		},
	}

	return cmd
}

func filepath(parts ...string) string {
	result := parts[0]
	for _, p := range parts[1:] {
		result = result + string(os.PathSeparator) + p
	}
	return result
}

func defaultDataDir() string {
	if os.Getenv("EPHEMERD_DATA_DIR") != "" {
		return os.Getenv("EPHEMERD_DATA_DIR")
	}
	if isWindows() {
		return `C:\ProgramData\ephemerd`
	}
	return "/var/lib/ephemerd"
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}
