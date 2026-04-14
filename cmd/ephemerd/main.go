package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	runtime_ "runtime"
	"strconv"
	"syscall"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/tunnel"
	"github.com/ephpm/ephemerd/pkg/vm"
	"github.com/urfave/cli/v3"
)

var (
	version   = "dev"
	configDir string
)

func main() {
	app := &cli.Command{
		Name:           "ephemerd",
		Usage:          "Ephemeral GitHub Actions runner daemon",
		Version:        version,
		DefaultCommand: "serve",
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
			doctorCmd(),
			installCmd(),
			uninstallCmd(),
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
			&cli.StringFlag{
				Name:  "containerd-tcp-addr",
				Value: "127.0.0.1",
				Usage: "bind address for the containerd TCP listener (use 0.0.0.0 when host lives outside the network namespace)",
			},
			&cli.BoolFlag{
				Name:  "containerd-only",
				Usage: "only run containerd (no scheduler, GitHub polling, or runner extraction)",
			},
			&cli.BoolFlag{
				Name:  "dind",
				Usage: "mount a fake Docker socket into each container (passed to WSL worker)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return serve(ctx, cmd.String("config"), uint32(cmd.Uint("containerd-tcp-port")), cmd.String("containerd-tcp-addr"), cmd.Bool("containerd-only"), cmd.Bool("dind"))
		},
	}
}

func serve(ctx context.Context, configFile string, containerdTCPPort uint32, containerdTCPAddr string, containerdOnly bool, dindFlag bool) error {
	// Check if another instance is already running.
	if cc, err := dialControl(ctx); err == nil {
		if resp, err := cc.Status(ctx, &apiv1.StatusRequest{}); err == nil {
			if closeErr := cc.Close(); closeErr != nil {
				return fmt.Errorf("closing control connection: %w", closeErr)
			}
			return fmt.Errorf("ephemerd is already running (status: %s, active jobs: %d, uptime: %s)",
				resp.Status, resp.ActiveJobs, resp.Uptime)
		}
		if closeErr := cc.Close(); closeErr != nil {
			return fmt.Errorf("closing control connection: %w", closeErr)
		}
	}

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

	// CLI --dind flag overrides config file
	if dindFlag {
		cfg.Dind.Enabled = true
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
	ctrdClient, waitDispatch, cleanup, err := startContainerRuntime(configDir, log, cfg.VM.Linux.Enabled, containerdTCPPort, containerdTCPAddr, cfg.Dind.Enabled)
	if err != nil {
		return fmt.Errorf("starting container runtime: %w", err)
	}
	defer cleanup()

	log.Info("container runtime ready")

	// In containerd-only mode, run containerd + dispatch worker (no scheduler).
	// Used by the WSL Linux VM — the Windows host dispatches jobs via gRPC.
	if containerdOnly {
		rm := runner.New(configDir, log)
		if err := rm.Extract(); err != nil {
			return fmt.Errorf("extracting runner: %w", err)
		}

		cm := cni.New(configDir, log)
		if err := cm.Extract(); err != nil {
			return fmt.Errorf("extracting CNI plugins: %w", err)
		}

		// Delete stale CNI bridge from a previous WSL boot. All WSL2 distros
		// share one Linux kernel, so the bridge persists across distro instances.
		// Without this, networking.New() picks a new random subnet that conflicts
		// with the existing bridge's IP.
		networking.CleanStaleBridge(log)

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
		if err := net.InstallFirewallRules(); err != nil {
			log.Warn("failed to install firewall rules", "error", err)
		}
		defer net.Cleanup()

		rt, err := runtime.New(runtime.Config{
			Client:      ctrdClient,
			RunnerDir:   rm.Dir(),
			RunnerMount: rm.ContainerDir(),
			LogDir:      joinPath(configDir, "logs"),
			DataDir:     configDir,
			DindEnabled: cfg.Dind.Enabled,
			Network:     net,
			Log:         log,
		})
		if err != nil {
			return fmt.Errorf("creating runtime: %w", err)
		}
		if err := rt.CleanOrphans(ctx); err != nil {
			log.Warn("failed to clean orphan containers", "error", err)
		}

		dispatchPort := int(containerdTCPPort) + 1
		dispatchCleanup := scheduler.StartDispatchServer(dispatchPort, rt, log)
		defer dispatchCleanup()

		log.Info("worker mode ready", "containerd_port", containerdTCPPort, "dispatch_port", dispatchPort)
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
	// On Darwin the Linux VM sees the host's DataDir at /mnt/ephemerd
	// (virtio-fs share tag "ephemerd"). Bind-mount sources pointed at the
	// DataDir need to be translated to that VM-side path.
	containerDataDir := configDir
	if runtime_.GOOS == "darwin" {
		containerDataDir = "/mnt/ephemerd"
	}
	rt, err := runtime.New(runtime.Config{
		Client:           ctrdClient,
		RunnerDir:        rm.Dir(),
		RunnerMount:      rm.ContainerDir(),
		DefaultImage:     cfg.Runner.DefaultImage,
		LogDir:           joinPath(configDir, "logs"),
		DataDir:          configDir,
		ContainerDataDir: containerDataDir,
		DindEnabled:      cfg.Dind.Enabled,
		Network:          net,
		Log:              log,
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

	// Configure macOS VM support if enabled
	var macOSVMConfig *vm.MacOSVMConfig
	if cfg.VM.MacOS.Enabled {
		macOSVMConfig = &vm.MacOSVMConfig{
			DataDir:   configDir,
			BaseImage: cfg.VM.MacOS.BaseImage,
			CPUs:      cfg.VM.MacOS.CPUs,
			MemoryMB:  cfg.VM.MacOS.MemoryMB,
			Log:       log,
		}
		log.Info("macOS VM support enabled", "base_image", cfg.VM.MacOS.BaseImage)
	}

	// Wait for Linux dispatch client if WSL VM is booting in the background.
	// All setup above (runner, CNI, networking, GitHub) runs in parallel with
	// the WSL boot, so this typically doesn't add much delay.
	linuxDispatcher := waitDispatch()
	if linuxDispatcher != nil {
		log.Info("Linux job dispatch enabled via WSL")
	}

	// Set up webhook tunnel (default: localtunnel, set tunnel = "none" for polling)
	var tunnelProvider tunnel.Provider
	if cfg.Webhook.Tunnel != "none" {
		var err error
		tunnelProvider, err = tunnel.New(cfg.Webhook.Tunnel, cfg.Webhook.NgrokAuthtoken, cfg.Webhook.TunnelURL)
		if err != nil {
			return fmt.Errorf("creating tunnel provider: %w", err)
		}
		log.Info("webhook tunnel configured", "provider", cfg.Webhook.Tunnel)
	} else {
		log.Info("polling mode enabled (tunnel disabled)")
	}

	// Start scheduler (ties GitHub jobs to container lifecycle)
	sched := scheduler.New(scheduler.Config{
		Runtime:         rt,
		GitHub:          gh,
		Artifacts:       artifactExtractor,
		LinuxDispatcher: linuxDispatcher,
		MacOSVMConfig:   macOSVMConfig,
		DataDir:         configDir,
		MaxConcurrent:   cfg.Runner.MaxConcurrent,
		Labels:          cfg.Runner.ExtraLabels,
		PollInterval:    cfg.GitHub.ParsedPollInterval(),
		WebhookPort:     cfg.Webhook.Port,
		WebhookSecret:   cfg.Webhook.Secret,
		TLSCert:         cfg.Webhook.TLSCert,
		TLSKey:          cfg.Webhook.TLSKey,
		Tunnel:            tunnelProvider,
		TunnelMaxRetries:  cfg.Webhook.TunnelMaxRetries,
		JobTimeout:        cfg.Runner.ParsedJobTimeout(),
		ShutdownTimeout: cfg.Runner.ParsedShutdownTimeout(),
		LogRetention:    cfg.Log.LogRetentionDuration(),
		Log:             log,
	})

	// Start metrics server if enabled
	if cfg.Metrics.Enabled {
		metricsCleanup := metrics.Serve(ctx, metrics.ServerConfig{
			Port:    cfg.Metrics.Port,
			Path:    cfg.Metrics.Path,
			TLSCert: cfg.Metrics.TLSCert,
			TLSKey:  cfg.Metrics.TLSKey,
			Log:     log,
		})
		defer metricsCleanup()
	}

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
