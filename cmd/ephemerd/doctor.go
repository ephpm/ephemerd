package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/networking"
	rtpkg "github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/urfave/cli/v3"
)

// doctorCleanup is the decision about whether doctor's cleanup phase may run.
type doctorCleanup struct {
	// Run reports whether the destructive cleanup may proceed.
	Run bool
	// Reason explains the refusal to the operator when Run is false.
	Reason string
}

// decideDoctorCleanup decides whether doctor may run its cleanup phase.
//
// Cleanup is not a tidy-up: it deletes the control socket and PID file, and
// on Windows it force-stops and REMOVES every ephemerd-* Hyper-V VM,
// unregisters every ephemerd-* WSL distro, and empties <data>/vm. Every one
// of those belongs to a RUNNING daemon, so doing it live destroys the node's
// Linux job capacity mid-job and leaves the daemon talking to a socket that
// no longer exists — and it used to happen on a bare `ephemerd doctor`, with
// no running-daemon check and no prompt.
//
// So: a running daemon means no cleanup, unless the operator says --force.
// Pure, so the guard is testable without a daemon.
func decideDoctorCleanup(daemonUp, force bool) doctorCleanup {
	if daemonUp && !force {
		return doctorCleanup{
			Run: false,
			Reason: "the ephemerd daemon is running — cleanup would delete live state " +
				"(control socket, PID file, running VMs). Stop it first ('ephemerd stop'), " +
				"or pass --force to clean anyway.",
		}
	}
	return doctorCleanup{Run: true}
}

// privilegeHint returns the "you may not have enough privilege" line for the
// cleanup phase, or "" when none applies.
//
// os.Geteuid() returns -1 on Windows, which is != 0, so the unguarded euid
// test printed "run with sudo" on every single Windows run — advice that is
// both wrong and impossible to follow there.
func privilegeHint(goos string, euid int) string {
	if goos == "windows" {
		return "(run from an elevated prompt for full cleanup — ephemerd state is owned by SYSTEM)"
	}
	if euid != 0 {
		return "(run with sudo for full cleanup — ephemerd data is owned by root)"
	}
	return ""
}

// platformCleanupPrompt describes what platformCleanup is about to destroy,
// so the confirmation names the actual damage instead of asking "are you
// sure?" about an unspecified action.
func platformCleanupPrompt(goos, dataDir string) string {
	switch goos {
	case "windows":
		return fmt.Sprintf(""+
			"  About to remove ALL ephemerd-* Hyper-V VMs (force-stopped, then deleted),\n"+
			"  unregister ALL ephemerd-* WSL distros, and delete %s (except embed/).\n"+
			"  On a node running the Linux VM sidecar this destroys Linux job capacity.\n"+
			"  Proceed? [y/N] ", filepath.Join(dataDir, "vm"))
	case "darwin":
		return fmt.Sprintf(""+
			"  About to remove stale macOS VM clones under %s.\n"+
			"  Proceed? [y/N] ", filepath.Join(dataDir, "vm", "macos", "jobs"))
	default:
		return fmt.Sprintf(""+
			"  About to remove stale CNI state, the DNS config under %s, and the\n"+
			"  ephemerd0 bridge if present.\n"+
			"  Proceed? [y/N] ", dataDir)
	}
}

func doctorCmd() *cli.Command {
	var (
		checkOnly bool
		cleanOnly bool
		force     bool
		assumeYes bool
	)
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check system readiness and clean up stale state",
		Description: "Checks always run. Cleanup only runs when the daemon is stopped — it deletes\n" +
			"the control socket, the PID file and (on Windows) every ephemerd-* VM and WSL\n" +
			"distro, none of which is safe to do underneath a running daemon.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "check",
				Usage:       "run checks only, don't clean up",
				Destination: &checkOnly,
			},
			&cli.BoolFlag{
				Name:        "clean",
				Usage:       "clean up only, skip checks",
				Destination: &cleanOnly,
			},
			&cli.BoolFlag{
				Name:        "force",
				Usage:       "clean up even while the daemon is running (DESTROYS live state: running VMs, control socket)",
				Destination: &force,
			},
			&cli.BoolFlag{
				Name:        "yes",
				Aliases:     []string{"y"},
				Usage:       "skip the confirmation prompt before removing VMs/WSL distros",
				Destination: &assumeYes,
			},
		},
		Action: func(ctx context.Context, _ *cli.Command) error {
			dataDir := configDir

			passed := 0
			warned := 0
			failed := 0

			pass := func(msg string) {
				fmt.Printf("  ✓ %s\n", msg)
				passed++
			}
			warn := func(msg string) {
				fmt.Printf("  ⚠ %s\n", msg)
				warned++
			}
			fail := func(msg string) {
				fmt.Printf("  ✗ %s\n", msg)
				failed++
			}

			if !cleanOnly {
				fmt.Println("System checks:")
				fmt.Println()

				// --- Cross-platform checks ---

				// Config file
				cfgPath := filepath.Join(dataDir, "config.toml")
				if _, err := os.Stat(cfgPath); err != nil {
					// Also check common alternative locations
					altPaths := []string{
						"/etc/ephemerd/config.toml",
						filepath.Join(dataDir, "ephemerd.toml"),
					}
					found := false
					for _, p := range altPaths {
						if _, err := os.Stat(p); err == nil {
							cfgPath = p
							found = true
							break
						}
					}
					if !found {
						warn(fmt.Sprintf("no config file found (checked %s)", cfgPath))
					}
				}
				if _, err := os.Stat(cfgPath); err == nil {
					cfg, err := config.Load(cfgPath)
					if err != nil {
						fail(fmt.Sprintf("config file invalid: %v", err))
					} else {
						pass(fmt.Sprintf("config file valid (%s)", cfgPath))
						_ = cfg
					}
				}

				// GitHub token
				token := os.Getenv("GITHUB_TOKEN")
				if token != "" {
					pass("GITHUB_TOKEN is set")
				} else {
					warn("GITHUB_TOKEN not set (required unless using GitHub App auth in config)")
				}

				// Data directory
				if err := os.MkdirAll(dataDir, 0o755); err != nil {
					fail(fmt.Sprintf("cannot create data directory %s: %v", dataDir, err))
				} else {
					pass(fmt.Sprintf("data directory writable (%s)", dataDir))
				}

				// Disk space
				checkDiskSpace(dataDir, pass, warn, fail)

				// Embedded assets
				checkEmbeddedAssets(pass, warn, fail)

				// Platform-specific checks
				fmt.Println()
				fmt.Printf("Platform checks (%s/%s):\n", runtime.GOOS, runtime.GOARCH)
				fmt.Println()
				platformChecks(pass, warn, fail)

				fmt.Println()
			}

			if !checkOnly {
				// Everything below this point deletes state the running
				// daemon owns, so ask the daemon whether it is there first.
				if decision := decideDoctorCleanup(daemonRunning(ctx), force); !decision.Run {
					fmt.Println("Cleanup: skipped")
					fmt.Printf("  ⚠ %s\n", decision.Reason)
					fmt.Println()
					fmt.Printf("Results: %d passed, %d warnings, %d failed\n", passed, warned, failed)
					if failed > 0 {
						return fmt.Errorf("%d check(s) failed", failed)
					}
					return nil
				}

				fmt.Println("Cleanup:")
				if hint := privilegeHint(runtime.GOOS, os.Geteuid()); hint != "" {
					fmt.Printf("  %s\n", hint)
				}
				fmt.Println()

				log := slog.Default()

				// Clean stale network state
				if runtime.GOOS == "linux" {
					networking.CleanStaleBridge(log)
					pass("cleaned stale network bridge (if any)")
				}

				// Clean stale socket
				socketPath := scheduler.SocketPath(dataDir)
				if _, err := os.Stat(socketPath); err == nil {
					if err := os.Remove(socketPath); err != nil {
						warn(fmt.Sprintf("could not remove stale socket %s: %v", socketPath, err))
					} else {
						pass("removed stale control socket")
					}
				} else {
					pass("no stale control socket")
				}

				// Clean stale PID file
				pidFile := filepath.Join(dataDir, "ephemerd.pid")
				if _, err := os.Stat(pidFile); err == nil {
					if err := os.Remove(pidFile); err != nil {
						warn(fmt.Sprintf("could not remove stale PID file: %v", err))
					} else {
						pass("removed stale PID file")
					}
				} else {
					pass("no stale PID file")
				}

				// Clean old job logs
				logDir := filepath.Join(dataDir, "logs")
				rtpkg.CleanOldLogs(logDir, 7*24*time.Hour, log)
				pass("cleaned old job logs (>7 days)")

				// Platform-specific cleanup is the destructive half — on
				// Windows it deletes VMs and WSL distros outright — so it
				// gets its own confirmation, in the same style as
				// `cache clear`. A non-interactive shell reads EOF and
				// declines, which is the safe answer.
				if assumeYes || confirm(platformCleanupPrompt(runtime.GOOS, dataDir)) {
					platformCleanup(dataDir, pass, warn, fail)
				} else {
					fmt.Println("  skipped platform cleanup (pass --yes to run it unattended)")
				}

				fmt.Println()
			}

			// Summary
			fmt.Printf("Results: %d passed, %d warnings, %d failed\n", passed, warned, failed)
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
}
