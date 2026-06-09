package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	runtime_ "runtime"
	"strconv"
	"syscall"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/artifacts"
	"github.com/ephpm/ephemerd/pkg/buildkit"
	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/dind"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/providers/forgejo"
	"github.com/ephpm/ephemerd/pkg/providers/gitea"
	githubProv "github.com/ephpm/ephemerd/pkg/providers/github"
	"github.com/ephpm/ephemerd/pkg/proxies"
	goproxy "github.com/ephpm/ephemerd/pkg/proxies/go"
	"github.com/ephpm/ephemerd/pkg/runner"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/tunnel"
	"github.com/ephpm/ephemerd/pkg/vm"
	"github.com/moby/sys/reexec"
	"github.com/urfave/cli/v3"
)

var (
	version   = "dev"
	configDir string
)

func main() {
	// BuildKit mounts our binary into Windows build containers and re-execs
	// it with argv[0]="get-user-info" to resolve user SIDs. The handler is
	// registered via the getuserinfo init() above; reexec.Init dispatches
	// when argv[0] matches and returns true so we exit instead of starting
	// the daemon.
	if reexec.Init() {
		return
	}

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
			configCheckCmd(),
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
			&cli.StringFlag{
				Name:  "images-dir",
				Usage: "directory of OCI image tarballs (*.tar) to copy into <data-dir>/images/ on startup",
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
			return serve(ctx, cmd.String("config"), cmd.String("images-dir"), uint32(cmd.Uint("containerd-tcp-port")), cmd.String("containerd-tcp-addr"), cmd.Bool("containerd-only"), cmd.Bool("dind"))
		},
	}
}

func serve(ctx context.Context, configFile, imagesDirFlag string, containerdTCPPort uint32, containerdTCPAddr string, containerdOnly bool, dindFlag bool) error {
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

	// Stage any caller-supplied OCI image tarballs into <data-dir>/images/.
	// Runtime.ImportImages then picks them up at boot. Same-size files are
	// skipped so re-running serve doesn't re-copy multi-GB tarballs.
	if err := copyTarballs(imagesDirFlag, joinPath(configDir, "images"), log); err != nil {
		return fmt.Errorf("staging images from --images-dir: %w", err)
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
	ctrdClient, waitDispatch, cleanup, err := startContainerRuntime(configDir, log, cfg.VM.Linux.Enabled, containerdTCPPort, containerdTCPAddr, cfg.Dind.Enabled, cfg.VM.Linux.CPUs, cfg.VM.Linux.MemoryMB, cfg.VM.Linux.DiskSizeGB)
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

		// Initialize embedded BuildKit when --dind is on. Without this,
		// dind.Server gets BuildKit=nil and POST /build falls through to the
		// buildah path; built images then live in containers/storage instead
		// of the "buildkit" containerd namespace, and POST /images/.../push
		// can't find them. Mirrors the non-containerd-only branch below.
		var bk *buildkit.Server
		if cfg.Dind.Enabled {
			bkCfg := buildkit.Config{
				DataDir:             joinPath(configDir, "buildkit"),
				ContainerdAddress:   containerd.SocketPath(configDir),
				ContainerdNamespace: "buildkit",
				Network:             net,
				Log:                 log.With("component", "buildkit"),
			}
			bk, err = buildkit.NewServer(ctx, bkCfg)
			if err != nil {
				log.Warn("buildkit init failed in worker mode; docker build will fall back",
					"error", err)
				bk = nil
			} else {
				defer func() {
					if err := bk.Close(); err != nil {
						log.Warn("closing buildkit server", "error", err)
					}
				}()
				log.Info("buildkit ready (worker mode)",
					"data_dir", bkCfg.DataDir,
					"namespace", bkCfg.ContainerdNamespace)
			}
		}

		rt, err := runtime.New(runtime.Config{
			Client:              ctrdClient,
			RunnerDir:           rm.Dir(),
			RunnerMount:         rm.ContainerDir(),
			LogDir:              joinPath(configDir, "logs"),
			DataDir:             configDir,
			DindEnabled:         cfg.Dind.Enabled,
			DindAllowPrivileged: cfg.Dind.ResolvedAllowPrivileged(),
			Rlimits:             cfg.Runtime.Rlimits.Resolved(),
			Network:             net,
			WindowsMemoryBytes:  cfg.Runner.Windows.MemoryBytes(),
			WindowsCPUs:         cfg.Runner.Windows.CPUCount(),
			BuildKit:            bk,
			Log:                 log,
		})
		if err != nil {
			return fmt.Errorf("creating runtime: %w", err)
		}
		if err := rt.CleanOrphans(ctx); err != nil {
			log.Warn("failed to clean orphan containers", "error", err)
		}

		// Clean up dind per-job namespaces left by jobs that didn't shut
		// down cleanly on the previous boot (DeadlineExceeded, SIGKILL,
		// host reboot, etc.). Server.Stop's CleanupJobNamespace covers the
		// graceful path; this catches everything else. Without this, every
		// ungraceful exit accumulates ~1 GB of pinned image content and the
		// namespace metadata bucket — we observed 73 leaked namespaces on
		// a host that filled its 100 GB VHDX over a couple of days.
		cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 5*time.Minute)
		dind.CleanupStaleDindNamespaces(cleanupCtx, rt.Client(), log)
		cancelCleanup()

		// Periodic per-repo image cache pruner. Each cache namespace
		// (ephemerd-dind-cache-<provider>-<repo>) is scanned every
		// CachePruneInterval, and any image record whose last-accessed
		// label is older than CacheMaxAge gets dropped — containerd's
		// content GC reclaims the unreferenced blobs. Empty cache
		// namespaces get removed entirely.
		if interval := cfg.Dind.DindCachePruneInterval(); interval > 0 && cfg.Dind.DindCacheMaxAge() > 0 {
			go runDindCachePruner(ctx, rt.Client(), interval, cfg.Dind.DindCacheMaxAge(), log)
		}

		dispatchPort := int(containerdTCPPort) + 1
		ds, dispatchCleanup := scheduler.StartDispatchServer(scheduler.DispatchServerConfig{
			Port:          dispatchPort,
			Runtime:       rt,
			Log:           log,
			StatsInterval: cfg.Metrics.ParsedContainerStatsInterval(),
			// No per-container caps configured for Linux today; samplers
			// surface the kernel-reported limit when present.
		})
		defer dispatchCleanup()
		// Wire the cgroup sampler hooks so every started container shows
		// up on the dispatch stats stream the host subscribes to.
		if onStart, onDestroy := buildDispatchSamplerHooks(ds, log.With("component", "dispatch-sampler")); onStart != nil {
			rt.SetTaskHooks(onStart, onDestroy)
		}

		// Debug exec server on dispatch_port+1 — lets the Windows host poke
		// into any container in the VM (e.g. exec'ing into kindest/node to
		// inspect iptables / lsmod / pod logs). No-op on non-Linux.
		debugCleanup := startWorkerDebugExec(ctx, int(containerdTCPPort)+2, rt.Client(), log)
		defer debugCleanup()

		log.Info("worker mode ready", "containerd_port", containerdTCPPort, "dispatch_port", dispatchPort, "dind", cfg.Dind.Enabled)
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

	// Start the shared embedded BuildKit solver. One solver serves every
	// job's `docker build` calls through pkg/dind. Only enabled when dind
	// is enabled and on platforms buildkit supports (linux, windows).
	// macOS jobs run in the Linux VM which has its own ephemerd + buildkit.
	log.Info("buildkit gate", "dind_enabled", cfg.Dind.Enabled, "goos", runtime_.GOOS)
	var bk *buildkit.Server
	if cfg.Dind.Enabled && runtime_.GOOS != "darwin" {
		bkCfg := buildkit.Config{
			DataDir:             joinPath(configDir, "buildkit"),
			ContainerdAddress:   containerd.SocketPath(configDir),
			ContainerdNamespace: "buildkit",
			Network:             net,
			Log:                 log.With("component", "buildkit"),
		}
		bk, err = buildkit.NewServer(ctx, bkCfg)
		if err != nil {
			log.Warn("buildkit init failed; docker build will fall back to platform default",
				"error", err)
			bk = nil
		} else {
			defer func() {
				if err := bk.Close(); err != nil {
					log.Warn("closing buildkit server", "error", err)
				}
			}()
			log.Info("buildkit ready",
				"data_dir", bkCfg.DataDir,
				"containerd", bkCfg.ContainerdAddress,
				"namespace", bkCfg.ContainerdNamespace)
		}
	}

	// Create runtime (container lifecycle manager).
	// On Darwin the Linux VM sees the host's DataDir at /mnt/ephemerd.
	containerDataDir := configDir
	if runtime_.GOOS == "darwin" {
		containerDataDir = "/mnt/ephemerd"
	}
	rt, err := runtime.New(runtime.Config{
		Client:              ctrdClient,
		RunnerDir:           rm.Dir(),
		RunnerMount:         rm.ContainerDir(),
		DefaultImage:        cfg.Runner.DefaultImage,
		ImagesDir:           joinPath(configDir, "images"),
		LogDir:              joinPath(configDir, "logs"),
		DataDir:             configDir,
		ContainerDataDir:    containerDataDir,
		DindEnabled:         cfg.Dind.Enabled,
		DindAllowPrivileged: cfg.Dind.ResolvedAllowPrivileged(),
		CacheProxyEnv:       cacheProxyEnvVars,
		Rlimits:             cfg.Runtime.Rlimits.Resolved(),
		Network:             net,
		WindowsMemoryBytes:  cfg.Runner.Windows.MemoryBytes(),
		WindowsCPUs:         cfg.Runner.Windows.CPUCount(),
		BuildKit:            bk,
		Log:                 log,
	})
	if err != nil {
		return fmt.Errorf("creating runtime: %w", err)
	}
	if err := rt.CleanOrphans(ctx); err != nil {
		log.Warn("failed to clean orphan containers", "error", err)
	}

	// Host-local per-container sampler registry. Only used for native
	// containers on the host (Windows host or Linux host). In-VM Linux
	// containers are sampled by the in-VM ephemerd and pushed back via
	// the dispatch stream — see ConsumeContainerStats wiring below.
	var hostSamplerRegistry *metrics.SamplerRegistry
	if cfg.Metrics.Enabled {
		hostSamplerRegistry = metrics.NewSamplerRegistry(cfg.Metrics.ParsedContainerStatsInterval(), log.With("component", "host-sampler"))
		hostSamplerRegistry.Start(ctx)
		defer hostSamplerRegistry.Stop()
		if onStart, onDestroy := buildHostSamplerHooks(hostSamplerRegistry, log.With("component", "host-sampler"), cfg.Runner.Windows.CPUCount(), cfg.Runner.Windows.MemoryBytes()); onStart != nil {
			rt.SetTaskHooks(onStart, onDestroy)
		}
	}

	// Create CI providers (one or more of GitHub, Forgejo, Gitea, etc.)
	activeProviders, providerCleanup, err := initProviders(cfg, log)
	if err != nil {
		return fmt.Errorf("creating providers: %w", err)
	}
	defer providerCleanup()

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

	// Create artifact extractor for macOS VM jobs. On macOS hosts, this
	// lets a job's `container: { image: ... }` pull OCI images and extract
	// their layers into the shared data directory (available inside macOS
	// VMs via virtio-fs).
	artifactExtractor := artifacts.NewExtractor(ctrdClient, log)

	// Wait for Linux dispatch client if the VM is booting in the background.
	linuxDispatcher, _ := waitDispatch()
	if linuxDispatcher != nil {
		log.Info("Linux job dispatch enabled")
	}
	if cfg.Dind.Enabled {
		log.Info("DinD enabled — containers will have /var/run/docker.sock")
	}

	// Set up webhook tunnel if configured
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

	// Start scheduler (ties CI provider jobs to container lifecycle)
	sched := scheduler.New(scheduler.Config{
		Runtime:            rt,
		Providers:          activeProviders,
		Artifacts:          artifactExtractor,
		LinuxDispatcher:    linuxDispatcher,
		DataDir:            configDir,
		MaxConcurrent:      cfg.Runner.MaxConcurrent,
		MaxMacOSVMs:        cfg.VM.MacOS.MaxConcurrent,
		Labels:             cfg.Runner.ExtraLabels,
		PollInterval:       pollInterval(cfg),
		WebhookPort:        cfg.Webhook.Port,
		WebhookSecret:      cfg.Webhook.Secret,
		TLSCert:            cfg.Webhook.TLSCert,
		TLSKey:             cfg.Webhook.TLSKey,
		Tunnel:             tunnelProvider,
		TunnelMaxRetries:   cfg.Webhook.TunnelMaxRetries,
		JobTimeout:         cfg.Runner.ParsedJobTimeout(),
		ShutdownTimeout:    cfg.Runner.ParsedShutdownTimeout(),
		LogRetention:       cfg.Log.LogRetentionDuration(),
		RunnerImageForRepo: cfg.Runner.ImageForRepoOS,
		Log:                log,
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

		// When we have a Linux dispatcher (Windows host with VM, macOS
		// host with Vz Linux VM), subscribe to the in-VM container stats
		// stream and feed batches into the host's metrics registry under
		// the linux-vm runtime label. Runs for the daemon's lifetime;
		// reconnects on transient stream errors.
		if linuxDispatcher != nil {
			interval := cfg.Metrics.ParsedContainerStatsInterval()
			go func() {
				if err := linuxDispatcher.ConsumeContainerStats(ctx, uint32(interval.Seconds()), metrics.RuntimeLinuxVM, log.With("component", "container-stats-consumer")); err != nil && ctx.Err() == nil {
					log.Warn("container stats consumer exited", "error", err)
				}
			}()
		}
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

// initProviders constructs all configured CI providers.
// Multiple providers can be active simultaneously (e.g., GitHub + Forgejo).
// Returns the providers, a cleanup function, and any error.
func initProviders(cfg *config.Config, log *slog.Logger) ([]providers.Provider, func(), error) {
	var active []providers.Provider
	var cleanups []func()

	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}

	// GitHub: configured when owner or token is set
	if cfg.GitHub.Owner != "" || cfg.GitHub.Token != "" {
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
				cleanup()
				return nil, nil, fmt.Errorf("initializing github app auth: %w", err)
			}
			ghCfg.AppAuth = appAuth
			cleanups = append(cleanups, appAuth.Stop)
		}
		ghClient, err := github.New(ghCfg)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("creating github client: %w", err)
		}
		active = append(active, githubProv.New(ghClient, log,
			cfg.GitHub.DefaultImageFor("linux"),
			cfg.GitHub.DefaultImageFor("windows")))
		log.Info("provider enabled", "provider", "github", "owner", cfg.GitHub.Owner)
	}

	// Forgejo: configured when instance_url is set
	if cfg.Forgejo.InstanceURL != "" {
		cfg.Dind.Enabled = true // Forgejo runner needs Docker API for job containers
		p, err := forgejo.New(forgejo.Config{
			InstanceURL:  cfg.Forgejo.InstanceURL,
			Token:        cfg.Forgejo.Token,
			Owner:        cfg.Forgejo.Owner,
			Repos:        cfg.Forgejo.Repos,
			Labels:       cfg.Forgejo.Labels,
			DefaultImage: cfg.Forgejo.DefaultImage,
			LinuxImage:   cfg.Forgejo.DefaultImageLinux,
			WindowsImage: cfg.Forgejo.DefaultImageWindows,
			JobImage:     cfg.Forgejo.JobImage,
			Log:          log,
		})
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("creating forgejo provider: %w", err)
		}
		active = append(active, p)
		log.Info("provider enabled", "provider", "forgejo", "instance", cfg.Forgejo.InstanceURL)
	}

	// Gitea: configured when instance_url is set
	if cfg.Gitea.InstanceURL != "" {
		cfg.Dind.Enabled = true // Gitea runner needs Docker API for job containers
		p, err := gitea.New(gitea.Config{
			InstanceURL:  cfg.Gitea.InstanceURL,
			Token:        cfg.Gitea.Token,
			Owner:        cfg.Gitea.Owner,
			Repos:        cfg.Gitea.Repos,
			Labels:       cfg.Gitea.Labels,
			DefaultImage: cfg.Gitea.DefaultImage,
			LinuxImage:   cfg.Gitea.DefaultImageLinux,
			WindowsImage: cfg.Gitea.DefaultImageWindows,
			JobImage:     cfg.Gitea.JobImage,
			Log:          log,
		})
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("creating gitea provider: %w", err)
		}
		active = append(active, p)
		log.Info("provider enabled", "provider", "gitea", "instance", cfg.Gitea.InstanceURL)
	}

	if len(active) == 0 {
		return nil, nil, fmt.Errorf("no providers configured — set [github], [forgejo], or another provider section in config")
	}

	return active, cleanup, nil
}

// pollInterval returns the poll interval for the configured provider.
// runDindCachePruner runs the per-repo image cache pruner on a fixed
// interval until ctx is canceled. Called in worker mode so each Linux VM
// keeps its dind image cache bounded. Errors from a single pass are
// logged and the loop continues — the next tick retries.
func runDindCachePruner(ctx context.Context, c *containerdclient.Client, interval, maxAge time.Duration, log *slog.Logger) {
	log = log.With("component", "dind-cache-pruner", "interval", interval, "max_age", maxAge)
	log.Info("starting dind cache pruner")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("dind cache pruner stopping")
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			if err := dind.CachePrune(passCtx, c, maxAge, log); err != nil {
				log.Warn("dind cache prune pass failed", "error", err)
			}
			cancel()
		}
	}
}

func pollInterval(cfg *config.Config) time.Duration {
	switch cfg.Provider() {
	case "github":
		return cfg.GitHub.ParsedPollInterval()
	default:
		return 30 * time.Second
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
