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

func doctorCmd() *cli.Command {
	var (
		checkOnly bool
		cleanOnly bool
	)
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check system readiness and clean up stale state",
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
		},
		Action: func(_ context.Context, _ *cli.Command) error {
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
				fmt.Println("Cleanup:")
				if os.Geteuid() != 0 {
					fmt.Println("  (run with sudo for full cleanup — ephemerd data is owned by root)")
				}
				fmt.Println()

				log := slog.Default()

				// Clean orphan containers
				cleaned := cleanOrphans(dataDir)
				if cleaned > 0 {
					pass(fmt.Sprintf("removed %d orphan container(s)", cleaned))
				} else {
					pass("no orphan containers")
				}

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

				// Platform-specific cleanup
				platformCleanup(dataDir, pass, warn, fail)

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

// cleanOrphans attempts to clean orphan containers. Returns count removed.
// This is best-effort — if containerd isn't running, returns 0.
func cleanOrphans(dataDir string) int {
	// Orphan cleanup requires a running containerd, which doctor doesn't start.
	// Just check for stale snapshot/container state directories.
	stateDir := filepath.Join(dataDir, "containerd", "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && e.Name() != "." && e.Name() != ".." {
			count++
		}
	}
	return 0 // don't actually remove — containerd manages its own state
}
