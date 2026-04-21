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
	goproxy "github.com/ephpm/ephemerd/pkg/proxies/go"
	"github.com/ephpm/ephemerd/pkg/proxies"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/providers"
	forgejoprovider "github.com/ephpm/ephemerd/pkg/providers/forgejo"
	giteaprovider "github.com/ephpm/ephemerd/pkg/providers/gitea"
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
	// When running as a Windows Service, the SCM invokes the binary directly.
	// Detect this and run the service handler instead of the CLI.
	if runAsWindowsService() {
		return
	}

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
			startCmd(),
			stopCmd(),
			restartCmd(),
			logsCmd(),
			statusCmd(),
			drainCmd(),
			jobsCmd(),
			imagesCmd(),
			configCheckCmd(),
			ctrctlCmd(),
			crictlCmd(),
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

	// When running as a Windows Service, route log output to the Event Log.
	if w := getServiceLogWriter(); w != nil {
		cfg.Log.Writer = w
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

	// Determine extra gateway ports for firewall (e.g., module proxy)
	var gatewayPorts []int
	modProxyPort := cfg.ModuleProxy.Port
	if modProxyPort == 0 {
		modProxyPort = 8082
	}
	if cfg.ModuleProxy.Enabled {
		gatewayPorts = append(gatewayPorts, modProxyPort)
	}

	// Initialize container networking
	net, err := networking.New(networking.Config{
		DataDir:      configDir,
		Subnet:       cfg.Network.Subnet,
		MTU:          cfg.Network.MTU,
		CNIBinDir:    cm.Dir(),
		GatewayPorts: gatewayPorts,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("initializing networking: %w", err)
	}

	// Install firewall rules to block container access to private networks
	if err := net.InstallFirewallRules(); err != nil {
		log.Warn("failed to install firewall rules (containers may access LAN)", "error", err)
	}
	defer net.Cleanup()

	// Start Go module caching proxy if enabled
	var cacheProxies []proxies.CacheProxy
	if cfg.ModuleProxy.Enabled {
		upstream := cfg.ModuleProxy.Upstream
		if upstream == "" {
			upstream = "https://proxy.golang.org"
		}
		cleanup := cfg.ModuleProxy.Cleanup
		if !cleanup {
			cleanup = true
		}

		goProxy := goproxy.New(goproxy.Config{
			CacheDir:   joinPath(configDir, "cache", "gomod"),
			Upstream:   upstream,
			ListenAddr: fmt.Sprintf("%s:%d", net.GatewayIP(), modProxyPort),
			Cleanup:    cleanup,
			Log:        log,
		})
		if err := goProxy.Start(); err != nil {
			log.Warn("failed to start Go module proxy, continuing without it", "error", err)
		} else {
			cacheProxies = append(cacheProxies, goProxy)
			defer func() {
				if err := goProxy.Stop(); err != nil {
					log.Warn("error stopping Go module proxy", "error", err)
				}
			}()
		}
	}

	// Collect env vars from all cache proxies for injection into containers.
	var cacheProxyEnvVars []string
	for _, cp := range cacheProxies {
		cacheProxyEnvVars = append(cacheProxyEnvVars, cp.EnvVars()...)
	}

	// Create provider based on config. Must happen before runtime creation
	// because forge providers auto-enable dind.
	var gh *github.Client
	var forgeProvider providers.Poll

	switch cfg.Provider() {
	case "gitea":
		cfg.Dind.Enabled = true
		p, err := giteaprovider.New(giteaprovider.Config{
			InstanceURL: cfg.Gitea.InstanceURL,
			Token:       cfg.Gitea.Token,
			Owner:       cfg.Gitea.Owner,
			Repos:       cfg.Gitea.Repos,
			Labels:      cfg.Gitea.Labels,
			JobImage:    cfg.Gitea.JobImage,
			Log:         log,
		})
		if err != nil {
			return fmt.Errorf("creating gitea provider: %w", err)
		}
		forgeProvider = p
		log.Info("using Gitea provider", "instance", cfg.Gitea.InstanceURL)

	case "forgejo":
		cfg.Dind.Enabled = true
		p, err := forgejoprovider.New(forgejoprovider.Config{
			InstanceURL: cfg.Forgejo.InstanceURL,
			Token:       cfg.Forgejo.Token,
			Owner:       cfg.Forgejo.Owner,
			Repos:       cfg.Forgejo.Repos,
			Labels:      cfg.Forgejo.Labels,
			JobImage:    cfg.Forgejo.JobImage,
			Log:         log,
		})
		if err != nil {
			return fmt.Errorf("creating forgejo provider: %w", err)
		}
		forgeProvider = p
		log.Info("using Forgejo provider", "instance", cfg.Forgejo.InstanceURL)

	default: // "github"
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
		var err error
		gh, err = github.New(ghCfg)
		if err != nil {
			return fmt.Errorf("creating github client: %w", err)
		}
	}

	// Create runtime (container lifecycle manager).
	// On Darwin the Linux VM sees the host's DataDir at /mnt/ephemerd.
	containerDataDir := configDir
	if runtime_.GOOS == "darwin" {
		containerDataDir = "/mnt/ephemerd"
	}
	rt, err := runtime.New(runtime.Config{
		Client:           ctrdClient,
		RunnerDir:        rm.Dir(),
		RunnerMount:      rm.ContainerDir(),
		DefaultImage:     cfg.Runner.DefaultImage,
		ImagesDir:        joinPath(configDir, "images"),
		LogDir:           joinPath(configDir, "logs"),
		DataDir:          configDir,
		ContainerDataDir: containerDataDir,
		DindEnabled:      cfg.Dind.Enabled,
		CacheProxyEnv:    cacheProxyEnvVars,
		Network:          net,
		Log:              log,
	})
	if err != nil {
		return fmt.Errorf("creating runtime: %w", err)
	}
	if err := rt.CleanOrphans(ctx); err != nil {
		log.Warn("failed to clean orphan containers", "error", err)
	}

	// Import pre-downloaded OCI image tarballs in the background so the
	// scheduler starts immediately. Large images like servercore take
	// minutes to unpack — jobs that don't need the imported image can
	// proceed in the meantime.
	go func() {
		deferredImages, importErr := rt.ImportImages(ctx)
		if importErr != nil {
			log.Warn("failed to import pre-downloaded images", "error", importErr)
		}

		// Import deferred Linux images into the VM's containerd.
		// waitDispatch blocks until the VM is ready, which may already
		// be done by the time we get here.
		if len(deferredImages) > 0 {
			_, vmClient := waitDispatch()
			if vmClient != nil {
				runtime.ImportImagesTo(ctx, vmClient, deferredImages, "overlayfs", log)
			}
		}
	}()

	// Create artifact extractor for macOS VM jobs.
	artifactExtractor := artifacts.NewExtractor(ctrdClient, log)

	// Wait for Linux dispatch client if the VM is booting in the background.
	linuxDispatcher, _ := waitDispatch()
	if linuxDispatcher != nil {
		log.Info("Linux job dispatch enabled")
	}

	// Set up webhook tunnel (GitHub mode only)
	var tunnelProvider tunnel.Provider
	if forgeProvider == nil && cfg.Webhook.Tunnel != "none" {
		var err error
		tunnelProvider, err = tunnel.New(cfg.Webhook.Tunnel, cfg.Webhook.NgrokAuthtoken, cfg.Webhook.TunnelURL)
		if err != nil {
			return fmt.Errorf("creating tunnel provider: %w", err)
		}
		log.Info("webhook tunnel configured", "provider", cfg.Webhook.Tunnel)
	} else if forgeProvider == nil {
		log.Info("polling mode enabled (tunnel disabled)")
	}

	// Start scheduler
	sched := scheduler.New(scheduler.Config{
		Runtime:          rt,
		GitHub:           gh,
		Provider:         forgeProvider,
		Artifacts:        artifactExtractor,
		LinuxDispatcher:  linuxDispatcher,
		DataDir:          configDir,
		MaxConcurrent:    cfg.Runner.MaxConcurrent,
		MaxMacOSVMs:      cfg.VM.MacOS.MaxConcurrent,
		Labels:           cfg.Runner.ExtraLabels,
		PollInterval:     cfg.GitHub.ParsedPollInterval(),
		WebhookPort:      cfg.Webhook.Port,
		WebhookSecret:    cfg.Webhook.Secret,
		TLSCert:          cfg.Webhook.TLSCert,
		TLSKey:           cfg.Webhook.TLSKey,
		Tunnel:           tunnelProvider,
		TunnelMaxRetries: cfg.Webhook.TunnelMaxRetries,
		JobTimeout:       cfg.Runner.ParsedJobTimeout(),
		ShutdownTimeout:  cfg.Runner.ParsedShutdownTimeout(),
		LogRetention:     cfg.Log.LogRetentionDuration(),
		Log:              log,
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

	// Pull the macOS base image (Tart OCI) in the background so the
	// scheduler can start accepting Linux jobs immediately.
	// Skipped when cross_platform = false (e.g. Gitea/Forgejo).
	if runtime_.GOOS == "darwin" && cfg.VM.CrossPlatformEnabled() {
		sshSigner, sshPubKey, err := vm.GenerateEphemeralSSHKey()
		if err != nil {
			return fmt.Errorf("generating ephemeral SSH key: %w", err)
		}
		log.Info("generated ephemeral SSH key for macOS VM access (in-memory only, rotates on restart)")

		go func() {
			files, err := vm.EnsureMacOSVMDisk(ctx, configDir, vm.MacOSInstallOptions{
				CustomDiskImage: cfg.VM.MacOS.DiskImage,
			}, log)
			if err != nil {
				log.Error("macOS VM disk provisioning failed — macOS jobs will be unavailable", "error", err)
				return
			}
			sched.SetMacOSVMConfig(&vm.MacOSVMConfig{
				DataDir:   configDir,
				DiskImage: files.DiskImage,
				SSHSigner: sshSigner,
				SSHPubKey: sshPubKey,
				CPUs:      cfg.VM.MacOS.CPUs,
				MemoryMB:  cfg.VM.MacOS.MemoryMB,
				Log:       log,
			})
			log.Info("macOS VM support ready", "disk_image", files.DiskImage)
		}()
	}

	log.Info("ephemerd ready", "provider", cfg.Provider(), "max_concurrent", cfg.Runner.MaxConcurrent)

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

// crictlCmd exposes the upstream crictl CLI against ephemerd's embedded
// containerd CRI socket. The crictl library is linked in-process; no external
// binary is required. See docs/arch/crictl.md.
func crictlCmd() *cli.Command {
	return &cli.Command{
		Name:            "crictl",
		Usage:           "Access the embedded containerd CRI (in-process crictl)",
		Description:     "Runs crictl commands against ephemerd's embedded containerd CRI endpoint.\nAll arguments after 'crictl' are passed directly to crictl (e.g. ps, images, info, exec).",
		SkipFlagParsing: true,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			socketPath := containerd.SocketPath(configDir)
			return containerd.ExecCrictl(socketPath, cmd.Args().Slice())
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
