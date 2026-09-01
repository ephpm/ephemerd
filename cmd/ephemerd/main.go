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
	"github.com/ephpm/ephemerd/pkg/cacheprune"
	"github.com/ephpm/ephemerd/pkg/cni"
	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/dind"
	"github.com/ephpm/ephemerd/pkg/github"
	"github.com/ephpm/ephemerd/pkg/imagegc"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/providers/forgejo"
	"github.com/ephpm/ephemerd/pkg/providers/gitea"
	githubProv "github.com/ephpm/ephemerd/pkg/providers/github"
	"github.com/ephpm/ephemerd/pkg/proxies"
	cargoproxy "github.com/ephpm/ephemerd/pkg/proxies/cargo"
	goproxy "github.com/ephpm/ephemerd/pkg/proxies/go"
	"github.com/ephpm/ephemerd/pkg/registrymirror"
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
			uncordonCmd(),
			upgradeCmd(),
			jobsCmd(),
			cacheCmd(),
			configCheckCmd(),
			crictlCmd(),
			doctorCmd(),
			installCmd(),
			uninstallCmd(),
			restartHelperCmd(),
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
			&cli.BoolFlag{
				Name:  "dind-allow-privileged",
				Usage: "allow privileged sibling containers (overrides config). Set on the in-VM ephemerd from the host's dind.allow_privileged.",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return serve(ctx, cmd.String("config"), cmd.String("images-dir"), uint32(cmd.Uint("containerd-tcp-port")), cmd.String("containerd-tcp-addr"), cmd.Bool("containerd-only"), cmd.Bool("dind"), cmd.Bool("dind-allow-privileged"))
		},
	}
}

func serve(ctx context.Context, configFile, imagesDirFlag string, containerdTCPPort uint32, containerdTCPAddr string, containerdOnly bool, dindFlag, dindAllowPrivilegedFlag bool) error {
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
	// CLI --dind-allow-privileged flag overrides config file. Used by the
	// in-VM ephemerd: the host plumbs its own dind.allow_privileged across
	// the VM boundary via this flag because the in-VM daemon has its own
	// (defaulted) config file.
	if dindAllowPrivilegedFlag {
		t := true
		cfg.Dind.AllowPrivileged = &t
	}

	// When running as a Windows Service, route log output to the Event Log.
	if w := getServiceLogWriter(); w != nil {
		cfg.Log.Writer = w
	}

	log := cfg.Logger()
	log.Info("starting ephemerd", "version", version, "data_dir", configDir)

	// Resolve the registry-mirror policy once and share it with every pull
	// path (runtime, each per-job dind server, the macOS artifact
	// extractor). Nil when unconfigured, and every consumer is nil-safe, so
	// a node without a mirror pulls exactly as it did before.
	//
	// This is constructed in BOTH the containerd-only (in-VM worker) branch
	// and the full-scheduler branch below, off the same cfg. On a Windows
	// host the config.toml is staged into the Linux VM's initrd at
	// /etc/ephemerd/config.toml on every boot, so a [registry_mirror] block
	// reaches the in-VM ephemerd with no extra plumbing (see
	// docs/arch/host-config-initrd.md). The macOS Vz VM does not yet receive
	// the host config — see docs/guides/registry-cache.md.
	registryMirror := registrymirror.New(cfg.RegistryMirror, log)

	// Refuse to start if the operator asked for VM-isolated Linux jobs but
	// this host can't deliver them. Falling back to runc here would run
	// untrusted CI code on the host kernel while the config says otherwise
	// — a silent downgrade of an isolation guarantee, which is strictly
	// worse than not starting.
	if err := checkKataPrereqs(cfg, log); err != nil {
		return err
	}

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

	// Ensure the host<->VM dispatch channel has a shared bearer token before
	// booting any VM. On a host that dispatches Linux jobs into a VM (Windows
	// Hyper-V / macOS Vz) the token both authenticates the host client and
	// rides into the guest inside config.toml, where the in-VM dispatch server
	// reads the identical value. Skipped in containerd-only (in-VM) mode: that
	// daemon consumes the token delivered in config.toml and must never rewrite
	// the shared config file. A one-time persist so the value is stable across
	// restarts and reaches the VM.
	if !containerdOnly {
		if err := cfg.EnsureDispatchToken(configFile, scheduler.GenerateDispatchToken); err != nil {
			return fmt.Errorf("ensuring dispatch token: %w", err)
		}
	}

	// Start container runtime.
	// On Linux/Windows: embedded containerd runs in-process.
	// On macOS: boot a Linux VM via Virtualization.framework, containerd runs inside it.
	ctrdClient, waitDispatch, cleanup, err := startContainerRuntime(configDir, log, cfg.VM.Linux.ResolvedEnabled(), containerdTCPPort, containerdTCPAddr, cfg.Dind.Enabled, cfg.Dind.ResolvedAllowPrivileged(), cfg.VM.Linux.CPUs, cfg.VM.Linux.MemoryMB, cfg.VM.Linux.DiskSizeGB, cfg.Dispatch.Token)
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

		// Control ports the in-VM daemon exposes on the bridge gateway that
		// containers must never reach: containerd (TCP), the unauthenticated
		// dispatch gRPC server (containerd+1), and the debug exec server
		// (containerd+2). See startWorkerDebugExec / StartDispatchServer above.
		controlPorts := []int{int(containerdTCPPort), int(containerdTCPPort) + 1, int(containerdTCPPort) + 2}

		net, err := networking.New(networking.Config{
			DataDir:           configDir,
			Subnet:            cfg.Network.Subnet,
			MTU:               cfg.Network.MTU,
			CNIBinDir:         cm.Dir(),
			ControlPorts:      controlPorts,
			L2BridgeEgress:    cfg.Network.L2BridgeEgress,
			HostNIC:           cfg.Network.HostNIC,
			IPPool:            cfg.Network.IPPool,
			Gateway:           cfg.Network.Gateway,
			PublicDNS:         cfg.Network.PublicDNS,
			ExtraAllowedCIDRs: cfg.Network.ExtraAllowedDestinations,
			AllowHostAccess:   needsHostAccess(cfg),
			Log:               log,
		})
		if err != nil {
			return fmt.Errorf("initializing networking: %w", err)
		}
		if err := enforceFirewallInstall(runtime_.GOOS, net.InstallFirewallRules, log); err != nil {
			return err
		}
		defer net.Cleanup()

		// Initialize embedded BuildKit when --dind is on. Without this,
		// dind.Server gets BuildKit=nil and POST /build is fail-closed: the
		// router returns HTTP 501 ("there is no buildah fallback") rather than
		// silently picking a different builder. Builds fail loudly on this node
		// — they never run unfirewalled — so BuildKit init must succeed here for
		// docker build to work at all. Mirrors the non-containerd-only branch
		// below.
		var bk *buildkit.Server
		if cfg.Dind.Enabled {
			bkCfg := buildkit.Config{
				DataDir:             joinPath(configDir, "buildkit"),
				ContainerdAddress:   containerd.SocketPath(configDir),
				ContainerdNamespace: buildkitNamespace,
				Network:             net,
				// Put build RUN steps on the firewalled CNI bridge instead of
				// the host netns (see buildkit.Config.CNIConfigPath).
				CNIConfigPath:  networking.CNIConfListPath(configDir),
				CNIBinDir:      cm.Dir(),
				DNSNameservers: networking.DefaultPublicDNS, // build containers resolve via public DNS over NAT egress — same as job containers; NO resolver runs on the bridge gateway (#180's wrong target)
				GC:             buildkitGCConfig(cfg),
				Log:            log.With("component", "buildkit"),
			}
			bk, err = buildkit.NewServer(ctx, bkCfg)
			if err != nil {
				log.Warn("buildkit init failed in worker mode; docker build will fail closed with HTTP 501 on this node",
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

		imageGC := newImageGC(cfg, ctrdClient, configDir, log)

		rt, err := runtime.New(runtime.Config{
			Client:              ctrdClient,
			RunnerDir:           rm.Dir(),
			RunnerMount:         rm.ContainerDir(),
			LogDir:              joinPath(configDir, "logs"),
			DataDir:             configDir,
			DindEnabled:         cfg.Dind.Enabled,
			DindAllowPrivileged: cfg.Dind.ResolvedAllowPrivileged(),
			Rlimits:             cfg.Runtime.Rlimits.Resolved(),
			AllowNewPrivileges:  cfg.Runtime.ResolvedAllowNewPrivileges(),
			LinuxRuntime:        cfg.Runner.Linux.ContainerdRuntime(),
			Network:             net,
			WindowsMemoryBytes:  cfg.Runner.Windows.MemoryBytes(),
			WindowsCPUs:         cfg.Runner.Windows.CPUCount(),
			BuildKit:            bk,
			ImageGC:             imageGC,
			RegistryMirror:      registryMirror,

			OrphanContainerReapEnabled: cfg.Runtime.ResolvedOrphanContainerReap(),
			OrphanContainerGrace:       cfg.Runtime.ResolvedOrphanContainerGrace(),
			Log:                        log,
		})
		if err != nil {
			return fmt.Errorf("creating runtime: %w", err)
		}
		if err := rt.CleanOrphans(ctx); err != nil {
			log.Warn("failed to clean orphan containers", "error", err)
		}

		// Periodic orphan sweep + disk-pressure image collection. Both
		// used to be startup-only (or, for images in this namespace,
		// absent entirely), so a VM that stayed up for days accumulated
		// every image it ever pulled.
		if interval := cfg.ImageGC.ImageGCCheckInterval(); interval > 0 {
			go runNodeDiskSweeper(ctx, imageGC, rt, ctrdClient, interval, newBrokenChainRepair(cfg), log)
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

		// Periodic per-repo image cache reaper. Every CachePruneInterval
		// each ephemerd-dind-cache-<provider>-<repo> namespace left with
		// no image records is removed entirely. When the optional
		// CacheMaxAge backstop is set, records idle longer than it are
		// evicted first — but bounding cache size is the disk-pressure
		// collector's job now (see runNodeDiskSweeper), not this loop's.
		if interval := cfg.Dind.DindCachePruneInterval(); interval > 0 {
			go runDindCachePruner(ctx, rt.Client(), interval, cfg.Dind.DindCacheMaxAge(), log)
		}

		dispatchPort := int(containerdTCPPort) + 1
		// The shared dispatch token rode into this VM inside config.toml (the
		// host minted + persisted it before boot). Require it on every RPC so
		// nothing else on the VM network — including job containers — can drive
		// CreateJob/WaitJob/DestroyJob. An empty token here means the host
		// delivered a config without one; StartDispatchServer logs loudly and
		// runs unauthenticated in that (misconfigured) case rather than
		// silently wedging dispatch.
		if cfg.Dispatch.Token == "" {
			log.Warn("dispatch: no shared token in config — the dispatch gRPC server will start UNAUTHENTICATED; ensure the host persisted [dispatch].token into the config delivered to this VM")
		}
		ds, dispatchCleanup := scheduler.StartDispatchServer(scheduler.DispatchServerConfig{
			Port:          dispatchPort,
			Runtime:       rt,
			Log:           log,
			StatsInterval: cfg.Metrics.ParsedContainerStatsInterval(),
			Token:         cfg.Dispatch.Token,
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
	cargoProxyPort := cfg.CargoProxy.Port
	if cargoProxyPort == 0 {
		cargoProxyPort = 8083
	}
	if cfg.CargoProxy.Enabled {
		gatewayPorts = append(gatewayPorts, cargoProxyPort)
	}
	// Language package caches (npm, pip, pub) — see pkgproxies.go.
	gatewayPorts = append(gatewayPorts, pkgProxyPorts(cfg)...)

	// Initialize container networking
	net, err := networking.New(networking.Config{
		DataDir:           configDir,
		Subnet:            cfg.Network.Subnet,
		MTU:               cfg.Network.MTU,
		CNIBinDir:         cm.Dir(),
		GatewayPorts:      gatewayPorts,
		L2BridgeEgress:    cfg.Network.L2BridgeEgress,
		HostNIC:           cfg.Network.HostNIC,
		IPPool:            cfg.Network.IPPool,
		Gateway:           cfg.Network.Gateway,
		PublicDNS:         cfg.Network.PublicDNS,
		ExtraAllowedCIDRs: cfg.Network.ExtraAllowedDestinations,
		AllowHostAccess:   needsHostAccess(cfg),
		Log:               log,
	})
	if err != nil {
		return fmt.Errorf("initializing networking: %w", err)
	}

	// Install firewall rules to block container access to private networks.
	// Fail closed on Linux: without the RFC1918 egress fence and the
	// EPHEMERD-INPUT host-protection chain there is no container containment,
	// so refuse to start rather than dispatch untrusted jobs unprotected.
	if err := enforceFirewallInstall(runtime_.GOOS, net.InstallFirewallRules, log); err != nil {
		return err
	}
	defer net.Cleanup()

	// Start Go module caching proxy if enabled
	var cacheProxies []proxies.CacheProxy
	if cfg.ModuleProxy.Enabled {
		upstream := cfg.ModuleProxy.Upstream
		if upstream == "" {
			upstream = "https://proxy.golang.org"
		}
		goProxy := goproxy.New(goproxy.Config{
			CacheDir:      joinPath(configDir, "cache", "gomod"),
			Upstream:      upstream,
			ListenAddr:    fmt.Sprintf("%s:%d", net.GatewayIP(), modProxyPort),
			Cleanup:       cfg.ModuleProxy.CleanupEnabled(),
			MaxCacheBytes: cfg.ModuleProxy.ModuleProxyMaxCacheBytes(),
			PruneInterval: cfg.ModuleProxy.ModuleProxyPruneInterval(),
			Log:           log,
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

	// Start Cargo/crates caching proxy if enabled
	if cfg.CargoProxy.Enabled {
		cargoProxy := cargoproxy.New(cargoproxy.Config{
			CacheDir: joinPath(configDir, "cache", "cargo"),
			// Config dir sits OUTSIDE the cache so `ephemerd cache clear
			// cargo` cannot delete the file mounted into running jobs.
			ConfDir:        joinPath(configDir, "cargo"),
			IndexUpstream:  cfg.CargoProxy.Upstream,
			RustupUpstream: cfg.CargoProxy.RustupUpstream,
			ListenAddr:     fmt.Sprintf("%s:%d", net.GatewayIP(), cargoProxyPort),
			IndexTTL:       cfg.CargoProxy.IndexTTL,
			Cleanup:        cfg.CargoProxy.CleanupEnabled(),
			Log:            log,
		})
		if err := cargoProxy.Start(); err != nil {
			log.Warn("failed to start Cargo proxy, continuing without it", "error", err)
		} else {
			cacheProxies = append(cacheProxies, cargoProxy)
			defer func() {
				if err := cargoProxy.Stop(); err != nil {
					log.Warn("error stopping Cargo proxy", "error", err)
				}
			}()
		}
	}

	// Start the language package caches (npm, pip, pub). Only those that
	// start AND answer a health probe are returned, so an unhealthy cache is
	// never advertised to a job — see startPkgProxies for the fail-open story.
	pkgProxies, stopPkgProxies := startPkgProxies(cfg, configDir, net, log)
	defer stopPkgProxies()
	cacheProxies = append(cacheProxies, pkgProxies...)

	// Collect env vars and mounts from all cache proxies for injection into
	// containers. Only proxies that actually STARTED are in cacheProxies, so
	// a failed proxy is never advertised to a job — that is the outer
	// fail-open: jobs go straight to the upstream registry instead.
	var cacheProxyEnvVars []string
	var cacheProxyMounts []proxies.Mount
	for _, cp := range cacheProxies {
		cacheProxyEnvVars = append(cacheProxyEnvVars, cp.EnvVars()...)
		if mp, ok := cp.(proxies.MountProvider); ok {
			cacheProxyMounts = append(cacheProxyMounts, mp.Mounts()...)
		}
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
			ContainerdNamespace: buildkitNamespace,
			Network:             net,
			// Put build RUN steps on the firewalled CNI bridge instead of the
			// host netns (see buildkit.Config.CNIConfigPath). Linux-only; the
			// Windows worker ignores it and uses the HCN NAT path via Network.
			CNIConfigPath:  networking.CNIConfListPath(configDir),
			CNIBinDir:      cm.Dir(),
			DNSNameservers: networking.DefaultPublicDNS, // build containers resolve via public DNS over NAT egress — same as job containers; NO resolver runs on the bridge gateway (#180's wrong target)
			GC:             buildkitGCConfig(cfg),
			Log:            log.With("component", "buildkit"),
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
	imageGC := newImageGC(cfg, ctrdClient, configDir, log)

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
		CacheProxyMounts:    cacheProxyMounts,
		Rlimits:             cfg.Runtime.Rlimits.Resolved(),
		AllowNewPrivileges:  cfg.Runtime.ResolvedAllowNewPrivileges(),
		LinuxRuntime:        cfg.Runner.Linux.ContainerdRuntime(),
		Network:             net,
		WindowsMemoryBytes:  cfg.Runner.Windows.MemoryBytes(),
		WindowsCPUs:         cfg.Runner.Windows.CPUCount(),
		BuildKit:            bk,
		ImageGC:             imageGC,
		RegistryMirror:      registryMirror,

		OrphanContainerReapEnabled: cfg.Runtime.ResolvedOrphanContainerReap(),
		OrphanContainerGrace:       cfg.Runtime.ResolvedOrphanContainerGrace(),
		Log:                        log,
	})
	if err != nil {
		return fmt.Errorf("creating runtime: %w", err)
	}
	if err := rt.CleanOrphans(ctx); err != nil {
		log.Warn("failed to clean orphan containers", "error", err)
	}

	// Periodic orphan sweep + disk-pressure image collection. CleanOrphans
	// above only runs at startup and cannot run again while jobs are in
	// flight (it deletes every container in the namespace); SweepOrphans is
	// the job-safe subset and runs on this timer instead.
	if interval := cfg.ImageGC.ImageGCCheckInterval(); interval > 0 {
		go runNodeDiskSweeper(ctx, imageGC, rt, ctrdClient, interval, newBrokenChainRepair(cfg), log)
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
	artifactExtractor := artifacts.NewExtractor(ctrdClient, registryMirror, log)

	// Wait for Linux dispatch client if the VM is booting in the background.
	linuxDispatcher, _ := waitDispatch()
	if linuxDispatcher != nil {
		log.Info("Linux job dispatch enabled")
	}
	if cfg.Dind.Enabled {
		log.Info("DinD enabled — containers will have /var/run/docker.sock")
	}

	// Set up webhook tunnel if configured.
	var tunnelProvider tunnel.Provider
	switch cfg.Webhook.Tunnel {
	case "none":
		// No ephemerd-managed tunnel. If a webhook secret is set the scheduler
		// still serves the /webhook/<provider> receiver (ingress arrives some
		// other way) and disables polling; otherwise it falls back to polling.
		if cfg.Webhook.Secret != "" {
			log.Info("webhook mode enabled (external ingress, no managed tunnel)", "port", cfg.Webhook.Port)
		} else {
			log.Info("polling mode enabled (no tunnel, no webhook secret)")
		}
	case "external":
		// A tunnel exists but is managed OUTSIDE ephemerd — e.g. a Cloudflare
		// tunnel running on another host that forwards a public hostname to
		// this port. ephemerd serves the webhook receiver and disables polling,
		// but never creates a tunnel: that ingress is owned externally.
		// Requires a secret (validated in config) so signatures can be
		// verified. When external_url is also set, the scheduler auto-registers
		// each tracked repo's webhook to <external_url>/webhook/<provider> on
		// startup (idempotently); otherwise the operator adds hooks by hand.
		log.Info("webhook mode enabled (external tunnel, ingress managed outside ephemerd)", "port", cfg.Webhook.Port)
		if cfg.Webhook.ExternalURL != "" {
			log.Info("external webhook auto-registration enabled", "external_url", cfg.Webhook.ExternalURL)
		}
	default:
		var err error
		tunnelProvider, err = tunnel.New(tunnel.Options{
			Provider:            cfg.Webhook.Tunnel,
			NgrokAuthtoken:      cfg.Webhook.NgrokAuthtoken,
			LocalTunnelBaseURL:  cfg.Webhook.TunnelURL,
			CloudflaredToken:    cfg.Webhook.CloudflaredToken,
			CloudflaredHostname: cfg.Webhook.CloudflaredHostname,
			CloudflaredVersion:  cfg.Webhook.CloudflaredVersion,
			CloudflaredDataDir:  configDir,
			CloudflaredPort:     cfg.Webhook.Port,
		})
		if err != nil {
			return fmt.Errorf("creating tunnel provider: %w", err)
		}
		log.Info("webhook tunnel configured", "provider", cfg.Webhook.Tunnel)
	}

	// Build the claim retry queue config. Wired here (not in Load) so
	// the RateHint closure can bind to the actual GitHub provider
	// instance: the retry queue uses the last-observed rate snapshot to
	// snap the next attempt just past the reset window when the API
	// budget is exhausted.
	retryCfg := scheduler.RetryConfig{
		Enabled:  cfg.Runner.ClaimRetryEnabled(),
		Schedule: cfg.Runner.ClaimRetrySchedule(),
		MaxAge:   cfg.Runner.ClaimRetryMaxAge(),
		Jitter:   cfg.Runner.ClaimRetryJitter(),
		RateHint: buildRateHint(activeProviders),
	}
	if retryCfg.Enabled {
		log.Info("claim retry queue enabled",
			"max_age", retryCfg.MaxAge,
			"schedule", retryCfg.Schedule,
			"jitter", retryCfg.Jitter,
			"rate_aware", retryCfg.RateHint != nil)
	}

	// Orphaned-runner sweep: destroys dispatched runners that were never
	// assigned a job within the grace window. GitHub schedules JIT
	// runners onto any queued job with matching labels, so runner
	// teardown is keyed to observed assignments; the sweep is the
	// cleanup path for runners whose intended job ran elsewhere.
	orphanCfg := scheduler.OrphanSweepConfig{
		Enabled: cfg.Runner.OrphanSweepEnabled(),
		Grace:   cfg.Runner.OrphanSweepGrace(),
	}
	if orphanCfg.Enabled {
		log.Info("orphaned-runner sweep enabled", "grace", orphanCfg.Grace)
	}

	// Start scheduler (ties CI provider jobs to container lifecycle)
	sched := scheduler.New(scheduler.Config{
		Runtime:           rt,
		Providers:         activeProviders,
		Artifacts:         artifactExtractor,
		LinuxDispatcher:   linuxDispatcher,
		LinuxJobsDisabled: !cfg.VM.Linux.ResolvedEnabled() && runtime_.GOOS == "darwin",
		CachePruner: &cacheprune.Pruner{
			Client:            ctrdClient,
			BuildKit:          bk,
			BuildKitNamespace: buildkitNamespace,
			Policy:            buildkitGCConfig(cfg),
			ImageGC:           imageGC,
			Log:               log.With("component", "cache-prune"),
		},
		DataDir:               configDir,
		Version:               version,
		MaxConcurrent:         cfg.Runner.MaxConcurrent,
		MaxMacOSVMs:           cfg.VM.MacOS.MaxConcurrent,
		MacOSProvisionTimeout: cfg.VM.MacOS.ParsedProvisionTimeout(),
		Labels:                cfg.Runner.ExtraLabels,
		PollInterval:          pollInterval(cfg),
		ReconcileInterval:     cfg.Webhook.ResolvedReconcileInterval(),
		WebhookPort:           cfg.Webhook.Port,
		WebhookSecret:         cfg.Webhook.Secret,
		TLSCert:               cfg.Webhook.TLSCert,
		TLSKey:                cfg.Webhook.TLSKey,
		Tunnel:                tunnelProvider,
		TunnelMaxRetries:      cfg.Webhook.TunnelMaxRetries,
		ExternalURL:           cfg.Webhook.ExternalURL,
		JobTimeout:            cfg.Runner.ParsedJobTimeout(),
		ShutdownTimeout:       cfg.Runner.ParsedShutdownTimeout(),
		LogRetention:          cfg.Log.LogRetentionDuration(),
		RunnerImageForRepo:    cfg.Runner.ImageForRepoOS,
		Retry:                 retryCfg,
		OrphanSweep:           orphanCfg,
		Log:                   log,
	})

	// Start metrics server if enabled
	if cfg.Metrics.Enabled {
		metricsCleanup := metrics.Serve(ctx, metrics.ServerConfig{
			Port:     cfg.Metrics.Port,
			Path:     cfg.Metrics.Path,
			TLSCert:  cfg.Metrics.TLSCert,
			TLSKey:   cfg.Metrics.TLSKey,
			BindAddr: cfg.Metrics.BindAddr,
			Log:      log,
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

	// GitHub: one provider per configured target. Each target has its own
	// owner + auth (App or PAT), so ephemerd can serve several owners at once
	// (e.g. an org via a GitHub App and a personal account via a PAT).
	for _, ghc := range cfg.GitHubTargets() {
		ghCfg := github.Config{
			Token:          ghc.Token,
			Owner:          ghc.Owner,
			Repos:          ghc.Repos,
			Log:            log,
			PoolMode:       cfg.Webhook.Pool,
			AllowedRepos:   ghc.DispatchPolicy.AllowedRepos,
			RequiredLabels: ghc.DispatchPolicy.RequiredLabels,
		}
		if ghc.AppID != 0 {
			appAuth, err := github.NewAppAuth(
				ghc.AppID,
				ghc.InstallationID,
				ghc.PrivateKeyPath,
				log,
			)
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("initializing github app auth for %s: %w", ghc.Owner, err)
			}
			ghCfg.AppAuth = appAuth
			cleanups = append(cleanups, appAuth.Stop)
		}
		ghClient, err := github.New(ghCfg)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("creating github client for %s: %w", ghc.Owner, err)
		}
		active = append(active, githubProv.New(ghClient, log,
			ghc.DefaultImageFor("linux"),
			ghc.DefaultImageFor("windows")))
		log.Info("provider enabled", "provider", "github", "owner", ghc.Owner)
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

// newImageGC builds the disk-pressure image collector for a node, or nil
// when collection is disabled.
//
// Scope is both the main runtime namespace (which had no image GC at all —
// every runner image and every workflow `container:` image ever pulled was
// retained forever, extracted layers and all, until the disk filled) and
// the per-repo dind cache namespaces.
//
// dataDir is measured rather than the containerd subdirectory: they are on
// the same filesystem, and dataDir is guaranteed to exist by the time we
// get here, so the statfs cannot fail on a fresh node.
func newImageGC(cfg *config.Config, c *containerdclient.Client, dataDir string, log *slog.Logger) *imagegc.Collector {
	if !cfg.ImageGC.ImageGCEnabled() {
		log.Info("image gc disabled by config")
		return nil
	}
	pinned := cfg.PinnedRunnerImages()
	gc := imagegc.New(imagegc.Config{
		Client: c,
		Path:   dataDir,
		Thresholds: imagegc.Thresholds{
			HighUsedPercent: cfg.ImageGC.ImageGCHighWatermarkPercent(),
			LowUsedPercent:  cfg.ImageGC.ImageGCLowWatermarkPercent(),
			MinFreeBytes:    cfg.ImageGC.ImageGCMinFreeBytes(),
			TargetFreeBytes: cfg.ImageGC.ImageGCTargetFreeBytes(),
		},
		// buildkitNamespace holds the largest reclaimable pile on a
		// build-heavy node; the runtime namespace and the per-repo dind
		// caches are the smaller two.
		Namespaces:        []string{runtime.Namespace, buildkitNamespace},
		NamespacePrefixes: []string{dind.DindCacheNamespacePrefix},
		PinnedImages:      pinned,
		// A live job's BuildKit export records are referenced by no
		// container — protect them by name prefix instead.
		LiveJobPrefixes: func(id string) []string { return []string{dind.BuildScopePrefix(id)} },
		MaxAge:          cfg.ImageGC.ImageGCMaxAge(),
		Log:             log,
	})
	if gc == nil {
		return nil
	}
	log.Info("image gc enabled",
		"path", dataDir,
		"high_watermark_percent", cfg.ImageGC.ImageGCHighWatermarkPercent(),
		"low_watermark_percent", cfg.ImageGC.ImageGCLowWatermarkPercent(),
		"min_free_gb", cfg.ImageGC.ImageGCMinFreeBytes()/(1024*1024*1024),
		"target_free_gb", cfg.ImageGC.ImageGCTargetFreeBytes()/(1024*1024*1024),
		"max_age", cfg.ImageGC.ImageGCMaxAge(),
		"pinned_images", pinned)
	return gc
}

// buildkitNamespace is the containerd namespace the embedded BuildKit
// solver stores build results and cache records in. It must match the
// ContainerdNamespace passed to buildkit.NewServer.
const buildkitNamespace = "buildkit"

// buildkitGCConfig renders the [buildkit] table into the solver's cache GC
// policy. Without this the worker gets an empty policy and BuildKit never
// collects anything.
func buildkitGCConfig(cfg *config.Config) buildkit.GCConfig {
	return buildkit.GCConfig{
		Disabled:              !cfg.BuildKit.BuildKitGCEnabled(),
		ReservedBytes:         cfg.BuildKit.BuildKitGCReservedBytes(),
		MaxUsedBytes:          cfg.BuildKit.BuildKitGCMaxUsedBytes(),
		MinFreeBytes:          cfg.BuildKit.BuildKitGCMinFreeBytes(),
		KeepDuration:          cfg.BuildKit.BuildKitGCKeepDuration(),
		EphemeralKeepDuration: cfg.BuildKit.BuildKitGCEphemeralKeepDuration(),
		EphemeralMaxUsedBytes: cfg.BuildKit.BuildKitGCEphemeralMaxUsedBytes(),
	}
}

// runNodeDiskSweeper is the periodic half of node disk hygiene, on one
// timer: an orphan sweep, a sweep of dead jobs' BuildKit export records,
// and a disk-pressure image collection pass.
//
// All three were previously startup-only or absent, which is why long-lived
// daemons accumulated build records, snapshots and images for their entire
// uptime. Each pass is cheap on an idle node — a container list, a couple
// of directory reads and one statfs — and the image collection is a no-op
// unless a watermark is crossed.
//
// Errors from one pass are logged and the loop continues; the next tick
// retries.
func runNodeDiskSweeper(ctx context.Context, gc *imagegc.Collector, rt *runtime.Runtime, c *containerdclient.Client, interval time.Duration, repair brokenChainRepair, log *slog.Logger) {
	log = log.With("component", "node-disk-sweeper", "interval", interval)
	log.Info("starting node disk sweeper")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("node disk sweeper stopping")
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if rt != nil {
				if err := rt.SweepOrphans(passCtx); err != nil {
					log.Warn("orphan sweep failed", "error", err)
				}
				// Reap leftover job containers whose task is dead: their
				// pinned writable snapshot is invisible to SweepOrphans
				// (the container still exists, so its id stays "live") and
				// to image GC (it counts as a running container). See
				// runtime.ReapDeadContainers.
				if err := rt.ReapDeadContainers(passCtx); err != nil {
					log.Warn("dead-container reap failed", "error", err)
				}
			}
			sweepDeadBuildRecords(passCtx, c, log)
			// Correctness repair, not capacity policy — so it runs on
			// every tick whether or not image GC is enabled, and before
			// the collector, which would otherwise plan against records
			// that cannot be used anyway. See #149.
			dind.SweepBrokenImageChains(passCtx, c, repair.namespaces, repair.snapshotter, repair.pinned, log)
			if gc != nil {
				if _, err := gc.Collect(passCtx); err != nil {
					log.Warn("image gc pass failed", "error", err)
				}
			}
			cancel()
		}
	}
}

// brokenChainRepair parameterises the sweeper's image ↔ snapshot repair pass:
// which namespaces to scan, which snapshotter holds the layers, and which
// refs must never be evicted even when their chain looks broken.
type brokenChainRepair struct {
	namespaces  []string
	snapshotter string
	pinned      []string
}

// newBrokenChainRepair describes the repair pass for this node.
//
// Scope matches the image GC's, plus the per-job dind namespaces: a job
// namespace that outlives its job (one did on the production node — see #149)
// can hold broken records just as easily, and nothing else revisits it.
func newBrokenChainRepair(cfg *config.Config) brokenChainRepair {
	return brokenChainRepair{
		namespaces:  []string{runtime.Namespace, buildkitNamespace},
		snapshotter: buildkit.DefaultSnapshotter(),
		pinned:      cfg.PinnedRunnerImages(),
	}
}

// sweepDeadBuildRecords removes job-scoped BuildKit export records in the
// shared buildkit namespace whose job no longer exists. dind.Server.Stop
// handles the graceful path; this catches jobs lost to SIGKILL, a host
// reboot, or a daemon that predates per-job cleanup.
func sweepDeadBuildRecords(ctx context.Context, c *containerdclient.Client, log *slog.Logger) {
	if c == nil {
		return
	}
	live, _, err := imagegc.RunningContainers(ctx, c, log)
	if err != nil {
		log.Warn("build record sweep: listing containers", "error", err)
		return
	}
	if _, err := dind.PruneDeadBuildRecords(ctx, c, buildkitNamespace, live, log); err != nil {
		log.Warn("build record sweep failed", "error", err)
	}
}

// pollInterval returns the poll interval for the configured provider.
// runDindCachePruner runs the per-repo image cache pruner on a fixed
// interval until ctx is canceled. Called in worker mode so each Linux VM
// keeps its dind image cache bounded. Errors from a single pass are
// logged and the loop continues — the next tick retries.
//
// maxAge is the OPTIONAL age backstop and defaults to 0 (disabled) — see
// config.DindConfig.CacheMaxAge. With it disabled this loop still runs, to
// reap cache namespaces left with no image records; the actual bounding of
// cache size is now the disk-pressure collector's job.
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

// buildRateHint returns a rate-hint closure that the scheduler's retry
// queue uses to bias backoff toward the next GitHub rate-limit reset.
// Returns nil when no rate-aware provider is present; the retry queue
// honors nil and falls through to the plain jittered backoff ladder.
//
// Currently only the GitHub provider surfaces rate data. If additional
// providers grow rate tracking, extend this to combine them (typically:
// use the earliest available reset time).
func buildRateHint(active []providers.Provider) func() (int64, time.Time, time.Time) {
	for _, p := range active {
		gp, ok := p.(*githubProv.Provider)
		if !ok {
			continue
		}
		return func() (int64, time.Time, time.Time) {
			remaining, _, reset, updated := gp.RateSnapshot()
			return remaining, reset, updated
		}
	}
	return nil
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

// enforceFirewallInstall installs the container firewall and decides whether a
// failure is fatal.
//
// On Linux the EPHEMERD-FORWARD RFC1918 egress fence and the EPHEMERD-INPUT
// host-protection chain are the ONLY thing containing an untrusted job
// container from the host's LAN and control plane. If they fail to install,
// dispatching jobs anyway is a silent, total loss of containment — a public
// fork PR would run with no egress fence and no host-input protection at all.
// So on Linux we fail closed: return the error so serve() refuses to start
// dispatching and exits non-zero, which systemd surfaces (Restart=on-failure
// then retries, leaving the node cordoned rather than running jobs bare). This
// mirrors checkKataPrereqs, which likewise refuses to start when an isolation
// guarantee cannot be met — a silent downgrade is strictly worse than not
// starting.
//
// Non-Linux hosts keep the prior warn-and-continue behavior. The Linux
// RFC1918/EPHEMERD-INPUT chains do not exist on Windows/macOS: there the Linux
// job firewall is installed by the in-VM worker (itself Linux, and made fail
// closed by this same helper), while this call installs the host platform's own
// rules (Windows netsh/L2Bridge; a darwin no-op) whose failure modes are out of
// scope for this Linux-specific containment guarantee.
//
// goos is passed in (rather than read from runtime.GOOS) purely so the
// fail-closed decision is unit-testable without a platform build tag.
func enforceFirewallInstall(goos string, install func() error, log *slog.Logger) error {
	if err := install(); err != nil {
		if goos == "linux" {
			return fmt.Errorf("installing container firewall rules: %w — refusing to run jobs without container egress/host containment (fail closed)", err)
		}
		log.Warn("failed to install firewall rules (containers may access LAN)", "error", err)
	}
	return nil
}

// needsHostAccess reports whether ephemerd runs anything that job containers
// must be able to reach over the network on the host address:
//
//   - dind: the per-job Docker API listener, which on Windows is a TCP listener
//     on the host address handed to the job as DOCKER_HOST.
//   - the Go module proxy: bound to the same address and injected as GOPROXY.
//   - the Cargo proxy: likewise, and unlike GOPROXY a Cargo source replacement
//     has no fallback, so a container that cannot reach it fails the build.
//
// It only affects the Windows L2Bridge egress path, where the ACL ladder
// otherwise blocks the host along with the rest of RFC1918 — with none of
// these features enabled the strictest posture (host unreachable) applies. On
// NAT and on Linux the gateway is already reachable and this changes nothing.
func needsHostAccess(cfg *config.Config) bool {
	return cfg.Dind.Enabled || cfg.ModuleProxy.Enabled || cfg.CargoProxy.Enabled
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
