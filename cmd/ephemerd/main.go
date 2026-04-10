package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/urfave/cli/v3"
)

var (
	version   = "dev"
	configDir string
)

func main() {
	app := &cli.Command{
		Name:    "ephemerd",
		Usage:   "Ephemeral GitHub Actions runner daemon",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "data-dir",
				Value:       defaultDataDir(),
				Usage:       "data directory for ephemerd state",
				Destination: &configDir,
			},
		},
		Commands: []*cli.Command{
			serveCmd(),
			runCmd(),
			statusCmd(),
			drainCmd(),
			jobsCmd(),
			imagesCmd(),
			configCheckCmd(),
			ctrctlCmd(),
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func serveCmd() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Start the ephemerd daemon",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "path to config file (default: <data-dir>/config.toml)",
			},
			&cli.UintFlag{
				Name:  "containerd-tcp-port",
				Usage: "also expose containerd on a TCP port (used by WSL host integration)",
			},
			&cli.BoolFlag{
				Name:  "containerd-only",
				Usage: "only run containerd (no scheduler, GitHub polling, or runner extraction)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return serve(ctx, cmd.String("config"), uint32(cmd.Uint("containerd-tcp-port")), cmd.Bool("containerd-only"))
		},
	}
}

func serve(ctx context.Context, configFile string, containerdTCPPort uint32, containerdOnly bool) error {
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

	// Ensure data directory exists
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("creating data directory %s: %w", configDir, err)
	}

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
	ctrdClient, cleanup, err := startContainerRuntime(configDir, log, cfg.VM.Linux.Enabled, containerdTCPPort, configFile, cfg.GitHub.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("starting container runtime: %w", err)
	}
	defer cleanup()

	log.Info("container runtime ready")

	// In containerd-only mode, just keep containerd running until shutdown.
	// Used by the WSL Linux VM — the Windows host handles scheduling.
	if containerdOnly {
		log.Info("containerd-only mode, waiting for shutdown signal")
		<-ctx.Done()
		return nil
	}

	// Extract embedded GitHub Actions runner
	rm := runner.New(configDir, log)
	if err := rm.Extract(); err != nil {
		return fmt.Errorf("extracting runner: %w", err)
	}

	// Extract embedded CNI plugins
	cm := cni.New(configDir, log)
	if err := cm.Extract(); err != nil {
		return fmt.Errorf("extracting CNI plugins: %w", err)
	}

	// Initialize container networking
	net, err := networking.New(networking.Config{
		DataDir:   configDir,
		Subnet:    cfg.Network.Subnet,
		MTU:       cfg.Network.MTU,
		CNIBinDir: cm.Dir(),
		Log:       log,
	})
	if err != nil {
		return fmt.Errorf("initializing networking: %w", err)
	}

	// Install firewall rules to block container access to private networks
	if err := net.InstallFirewallRules(); err != nil {
		log.Warn("failed to install firewall rules (containers may access LAN)", "error", err)
	}
	defer net.Cleanup()

	// Create runtime (container lifecycle manager)
	rt, err := runtime.New(runtime.Config{
		Client:       ctrdClient,
		RunnerDir:    rm.Dir(),
		RunnerMount:  rm.ContainerDir(),
		DefaultImage: cfg.Runner.DefaultImage,
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
	ghCfg := github.Config{
		Token: cfg.GitHub.Token,
		Owner: cfg.GitHub.Owner,
		Repos: cfg.GitHub.Repos,
		Log:   log,
	}
	if cfg.GitHub.AppID != 0 {
		appAuth, err := github.NewAppAuth(
			cfg.GitHub.AppID,
			cfg.GitHub.InstallationID,
			cfg.GitHub.PrivateKeyPath,
			log,
		)
		if err != nil {
			return fmt.Errorf("initializing github app auth: %w", err)
		}
		defer appAuth.Stop()
		ghCfg.AppAuth = appAuth
	}
	gh, err := github.New(ghCfg)
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
func ctrctlCmd() *cli.Command {
	return &cli.Command{
		Name:            "ctrctl",
		Usage:           "Access the embedded containerd (passthrough to ctr)",
		Description:     "Runs ctr commands against ephemerd's embedded containerd instance.\nAll arguments after 'ctrctl' are passed directly to ctr.",
		SkipFlagParsing: true,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			socketPath := containerd.SocketPath(configDir)
			return containerd.ExecCtr(socketPath, cmd.Args().Slice())
		},
	}
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
