package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/registrymirror"
	"github.com/ephpm/ephemerd/pkg/vm"
	"github.com/ephpm/ephemerd/pkg/workflow"
	"github.com/urfave/cli/v3"
)

func runCmd() *cli.Command {
	return &cli.Command{
		Name:      "run",
		Usage:     "Run a GitHub Actions workflow locally",
		ArgsUsage: "[workflow-file]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "job",
				Aliases: []string{"j"},
				Usage:   "run a specific job by name",
			},
			&cli.StringFlag{
				Name:  "image",
				Usage: "container image to use (default: from service config or built-in)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runWorkflow(ctx, cmd.Args().First(), cmd.String("job"), cmd.String("image"))
		},
	}
}

func runWorkflow(ctx context.Context, workflowPath string, jobFilter string, imageFlag string) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Find the workflow file
	if workflowPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting working directory: %w", err)
		}
		found, err := workflow.FindWorkflow(cwd)
		if err != nil {
			return err
		}
		workflowPath = found
	}

	// Parse the workflow
	wf, err := workflow.Parse(workflowPath)
	if err != nil {
		return err
	}

	name := wf.Name
	if name == "" {
		name = workflowPath
	}
	fmt.Printf("Workflow: %s\n", name)

	// Select which job(s) to run
	var jobName string
	var job workflow.Job

	if jobFilter != "" {
		j, ok := wf.Jobs[jobFilter]
		if !ok {
			available := make([]string, 0, len(wf.Jobs))
			for k := range wf.Jobs {
				available = append(available, k)
			}
			return fmt.Errorf("job %q not found (available: %v)", jobFilter, available)
		}
		jobName = jobFilter
		job = j
	} else {
		// Run the first job (YAML map iteration order is non-deterministic,
		// but for a single-job workflow this is fine)
		for k, v := range wf.Jobs {
			jobName = k
			job = v
			break
		}
	}

	// Resolve repo directory (the directory containing .github/)
	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Detect target platform and delegate Linux jobs to WSL on Windows
	platform := workflow.DetectPlatform(job.RunsOn)

	if platform == workflow.PlatformLinux && runtime.GOOS == "windows" {
		fmt.Printf("==> Job: %s (linux on windows — delegating to WSL)\n", jobName)

		distro, err := vm.NewRunDistro(ctx, vm.RunDistroConfig{
			DataDir: configDir,
			Log:     log,
		})
		if err != nil {
			return fmt.Errorf("setting up WSL run distro: %w", err)
		}
		defer distro.Destroy()

		absWorkflow, err := filepath.Abs(workflowPath)
		if err != nil {
			return fmt.Errorf("resolving workflow path: %w", err)
		}

		exitCode, err := distro.Run(ctx, vm.RunInWSLConfig{
			WorkflowPath: absWorkflow,
			JobFilter:    jobFilter,
			RepoDir:      repoDir,
		})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return fmt.Errorf("WSL ephemerd exited with code %d", exitCode)
		}
		return nil
	}

	// Use an isolated temp directory so the run command doesn't conflict with
	// the ephemerd service (BoltDB is single-writer and they'd share the same
	// data directory and named pipe otherwise).
	tmpDir, err := os.MkdirTemp("", "ephemerd-run-*")
	if err != nil {
		return fmt.Errorf("creating temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			log.Warn("failed to clean up temp directory", "dir", tmpDir, "error", err)
		}
	}()

	// On Windows, derive a unique named pipe so we don't collide with the
	// service's \\.\pipe\ephemerd-containerd. On Unix the socket lives
	// inside DataDir which is already unique.
	var socketPath string
	if runtime.GOOS == "windows" {
		socketPath = `\\.\pipe\ephemerd-run-` + filepath.Base(tmpDir)
	}

	// Load the service config once. A local run must use the SAME container
	// network the host is configured for — building its own would put the job
	// on an unfiltered network on a host that was deliberately configured to
	// filter, and on Windows would also install the NAT-era netsh rules that
	// block RFC1918 host-wide. A missing or unreadable config is not fatal:
	// the run falls back to the built-in default network.
	cfg := loadRunConfig()

	runner := &workflow.Runner{
		DataDir:    tmpDir,
		SocketPath: socketPath,
		Image:      resolveRunImage(imageFlag, platform, cfg),
		Network:    runNetworkOptions(cfg, log),
		// Honor the node's [registry_mirror] block for local runs too, so
		// `ephemerd run` pulls from the same LAN cache the daemon uses.
		// cfg is nil when config.toml is missing or unreadable.
		RegistryMirror: runRegistryMirror(cfg, log),
		Log:            log,
	}

	return runner.RunJob(ctx, jobName, job, repoDir)
}

// loadRunConfig reads the service config for a local run. A missing or
// malformed config.toml is not an error here — the run simply falls back to the
// built-in defaults for both image and network — so it returns nil rather than
// failing the command.
func loadRunConfig() *config.Config {
	cfg, err := config.Load(filepath.Join(configDir, "config.toml"))
	if err != nil {
		return nil
	}
	return cfg
}

// runRegistryMirror builds the pull-through mirror policy for a local run
// from the host's [registry_mirror] block. Returns nil — meaning "pull from
// the origin registry" — when there is no readable config or no mirror is
// configured.
func runRegistryMirror(cfg *config.Config, log *slog.Logger) *registrymirror.Mirror {
	if cfg == nil {
		return nil
	}
	return registrymirror.New(cfg.RegistryMirror, log)
}

// runNetworkOptions maps the host's [network] config onto a local run and warns
// when the job will land on the default, unfilterable container network.
//
// On Windows the default is an HNS NAT network on the Hyper-V vSwitch. Container
// egress there CANNOT be filtered in software — WFP, the Hyper-V firewall, VFP
// on a NAT switch and netsh have all been ruled out on metal, because WinNAT's
// translation path never presents the packet to an inspectable filtering layer.
// A job on that network can reach anything the host can, including the LAN and
// any management planes on it. network.l2bridge_egress is the only path that
// actually enforces. See docs/guides/security.md ("Network Firewall").
func runNetworkOptions(cfg *config.Config, log *slog.Logger) workflow.NetworkOptions {
	if cfg == nil {
		warnUnfilteredRunNetwork(log, "no readable config.toml")
		return workflow.NetworkOptions{}
	}

	opts := workflow.NetworkOptions{
		Subnet:            cfg.Network.Subnet,
		MTU:               cfg.Network.MTU,
		L2BridgeEgress:    cfg.Network.L2BridgeEgress,
		HostNIC:           cfg.Network.HostNIC,
		IPPool:            cfg.Network.IPPool,
		Gateway:           cfg.Network.Gateway,
		PublicDNS:         cfg.Network.PublicDNS,
		ExtraAllowedCIDRs: cfg.Network.ExtraAllowedDestinations,
		AllowHostAccess:   needsHostAccess(cfg),
	}

	if runtime.GOOS == "windows" && !opts.L2BridgeEgress {
		warnUnfilteredRunNetwork(log, "network.l2bridge_egress is not enabled")
	}
	return opts
}

func warnUnfilteredRunNetwork(log *slog.Logger, reason string) {
	if runtime.GOOS != "windows" {
		return
	}
	log.Warn("this job's container egress is NOT filtered: it runs on the default Hyper-V vSwitch (HNS NAT), "+
		"where container egress cannot be filtered in software — the job can reach your LAN and anything on it. "+
		"Set network.l2bridge_egress (requires a WIRED adapter and a reserved network.ip_pool) to enforce egress",
		"reason", reason)
}

// resolveRunImage determines the container image for a run job.
// Priority: --image flag → service config.toml → empty (caller applies the
// built-in default — see workflow.Runner.RunJob, which substitutes
// defaultImage when this returns "").
func resolveRunImage(flagValue string, platform workflow.TargetPlatform, cfg *config.Config) string {
	if flagValue != "" {
		return flagValue
	}
	if cfg == nil {
		return ""
	}
	return cfg.GitHub.DefaultImageFor(platform.String())
}
