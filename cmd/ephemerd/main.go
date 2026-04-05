package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runner"
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
		statusCmd(),
		drainCmd(),
		imagesCmd(),
		configCheckCmd(),
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
		configFile = joinPath(configDir, "config.toml")
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	log := cfg.Logger()
	log.Info("starting ephemerd", "version", version, "data_dir", configDir)

	// Write PID file for drain command
	pidFile := joinPath(configDir, "ephemerd.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		log.Warn("failed to write pid file", "path", pidFile, "error", err)
	} else {
		defer func() { _ = os.Remove(pidFile) }()
	}

	// Start container runtime.
	// On Linux/Windows: embedded containerd runs in-process.
	// On macOS: boot a Linux VM via Virtualization.framework, containerd runs inside it.
	ctrdClient, cleanup, err := startContainerRuntime(configDir, log)
	if err != nil {
		return fmt.Errorf("starting container runtime: %w", err)
	}
	defer cleanup()

	log.Info("container runtime ready")

	// Extract embedded GitHub Actions runner
	rm := runner.New(configDir, log)
	if err := rm.Extract(); err != nil {
		return fmt.Errorf("extracting runner: %w", err)
	}

	// Initialize container networking
	net, err := networking.New(networking.Config{
		DataDir: configDir,
		Log:     log,
	})
	if err != nil {
		return fmt.Errorf("initializing networking: %w", err)
	}

	// Install firewall rules to block container access to private networks
	if err := net.InstallFirewallRules(); err != nil {
		log.Warn("failed to install firewall rules (containers may access LAN)", "error", err)
	}
	defer net.RemoveFirewallRules()

	// Create runtime (container lifecycle manager)
	rt, err := runtime.New(runtime.Config{
		Client:       ctrdClient,
		RunnerDir:    rm.Dir(),
		RunnerMount:  rm.ContainerDir(),
		LogDir:       joinPath(configDir, "logs"),
		Network:      net,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("creating runtime: %w", err)
	}

	// Clean up orphan containers from any previous crash
	if err := rt.CleanOrphans(ctx); err != nil {
		log.Warn("failed to clean orphan containers", "error", err)
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

	// Create artifact extractor for macOS VM jobs. On macOS hosts, this
	// allows EPHEMERD_IMAGE to pull OCI images and extract their layers
	// into the shared data directory (available inside macOS VMs via virtio-fs).
	artifactExtractor := artifacts.NewExtractor(ctrdClient, log)

	// Start scheduler (ties GitHub jobs to container lifecycle)
	sched := scheduler.New(scheduler.Config{
		Runtime:         rt,
		GitHub:          gh,
		Artifacts:       artifactExtractor,
		DataDir:         configDir,
		MaxConcurrent:   cfg.Runner.MaxConcurrent,
		Labels:          cfg.Runner.ExtraLabels,
		PollInterval:    cfg.GitHub.ParsedPollInterval(),
		WebhookPort:     cfg.GitHub.WebhookPort,
		WebhookSecret:   cfg.GitHub.WebhookSecret,
		TLSCert:         cfg.GitHub.TLSCert,
		TLSKey:          cfg.GitHub.TLSKey,
		JobTimeout:      cfg.Runner.ParsedJobTimeout(),
		ShutdownTimeout: cfg.Runner.ParsedShutdownTimeout(),
		Log:             log,
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

func joinPath(parts ...string) string {
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
