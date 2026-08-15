package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/urfave/cli/v3"
)

// uninstallPlan renders what `ephemerd uninstall` is about to destroy, for
// the confirmation prompt. Separated from the command so the wording is
// testable: the data directory must be named when it is going to be deleted,
// and must NOT be threatened when --keep-data is set.
func uninstallPlan(goos, dataDir string, keepData bool) string {
	var b strings.Builder
	b.WriteString("About to uninstall ephemerd from this host:\n")
	b.WriteString("  - stop and remove the ephemerd service\n")
	b.WriteString("  - remove the ephemerd binary\n")
	if goos == "windows" {
		b.WriteString("  - force-stop and delete ALL ephemerd-* Hyper-V VMs and WSL distros\n")
	} else {
		b.WriteString("  - remove leftover runtime state (network bridges, VM clones, CNI state)\n")
	}
	if keepData {
		b.WriteString(fmt.Sprintf("  - KEEP the data directory: %s\n", dataDir))
	} else {
		b.WriteString(fmt.Sprintf("  - DELETE the entire data directory: %s\n", dataDir))
		b.WriteString("    (config, logs, container images, job history — this is not recoverable)\n")
	}
	return b.String()
}

func uninstallCmd() *cli.Command {
	var (
		keepData  bool
		assumeYes bool
	)
	return &cli.Command{
		Name:  "uninstall",
		Usage: "Remove ephemerd binary, service, and optionally all data",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "keep-data",
				Usage:       "keep the data directory (config, logs, container state)",
				Destination: &keepData,
			},
			&cli.BoolFlag{
				Name:        "yes",
				Aliases:     []string{"y"},
				Usage:       "skip the confirmation prompt",
				Destination: &assumeYes,
			},
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			dataDir := configDir

			// This is the most destructive command in the CLI and it used to
			// run on a bare `ephemerd uninstall` with no prompt at all, while
			// `cache clear` — which removes strictly less — asks. Ask here
			// too, in the same style, with --yes to skip for automation.
			if !assumeYes {
				fmt.Print(uninstallPlan(runtime.GOOS, dataDir, keepData))
				if !confirm("Proceed? [y/N] ") {
					fmt.Println("Aborted.")
					return nil
				}
				fmt.Println()
			}

			fmt.Println("Uninstalling ephemerd...")
			fmt.Println()

			// Stop and remove the service BEFORE touching runtime state.
			// Cleanup deletes the control socket and the VMs the daemon is
			// using, so doing it first (as this used to) pulls the rug out
			// from under a daemon that is still running jobs.
			switch runtime.GOOS {
			case "linux":
				uninstallSystemd()
			case "darwin":
				uninstallLaunchd()
			case "windows":
				uninstallWindowsService()
			}
			fmt.Println()

			// Now that the daemon is stopped, remove leftover runtime state
			// (containers, network bridges, WSL distros, VM clones).
			fmt.Println("Cleaning up runtime state...")
			cleanupRuntime(dataDir)
			fmt.Println()

			// Remove the binary
			exe, err := os.Executable()
			if err != nil {
				fmt.Printf("  could not determine binary path: %v\n", err)
			} else {
				// Resolve symlinks
				exe, err = filepath.EvalSymlinks(exe)
				if err != nil {
					fmt.Printf("  could not resolve binary path: %v\n", err)
				} else {
					if runtime.GOOS == "windows" {
						// Can't delete a running binary on Windows — schedule for removal
						fmt.Printf("  binary: %s (delete manually after exit)\n", exe)
					} else {
						if err := os.Remove(exe); err != nil {
							fmt.Printf("  could not remove binary %s: %v\n", exe, err)
						} else {
							fmt.Printf("  removed binary: %s\n", exe)
						}
					}
				}
			}

			// Remove data directory
			if keepData {
				fmt.Printf("  keeping data directory: %s\n", dataDir)
			} else {
				if err := os.RemoveAll(dataDir); err != nil {
					fmt.Printf("  could not remove data directory %s: %v\n", dataDir, err)
				} else {
					fmt.Printf("  removed data directory: %s\n", dataDir)
				}
			}

			// Remove env file
			if !keepData {
				for _, envFile := range []string{"/etc/default/ephemerd", "/etc/sysconfig/ephemerd"} {
					if err := os.Remove(envFile); err == nil {
						fmt.Printf("  removed env file: %s\n", envFile)
					}
				}
			}

			fmt.Println()
			fmt.Println("ephemerd has been uninstalled.")
			return nil
		},
	}
}

// cleanupRuntime runs the same cleanup as `ephemerd doctor --clean` to remove
// stale containers, network bridges, WSL distros, CNI state, etc. before
// removing the data directory.
func cleanupRuntime(dataDir string) {
	info := func(msg string) { fmt.Printf("  %s\n", msg) }
	noop := func(string) {}

	// Remove stale control socket
	socketPath := filepath.Join(dataDir, "ephemerd.sock")
	if err := os.Remove(socketPath); err == nil {
		info("removed stale control socket")
	}

	// Remove stale PID file
	pidFile := filepath.Join(dataDir, "ephemerd.pid")
	if err := os.Remove(pidFile); err == nil {
		info("removed stale PID file")
	}

	// Platform-specific cleanup (network bridges, WSL distros, VM clones, CNI state)
	platformCleanup(dataDir, info, info, noop)
}

func uninstallSystemd() {
	// Stop the service
	if out, err := exec.Command("systemctl", "stop", "ephemerd").CombinedOutput(); err != nil {
		fmt.Printf("  note: could not stop service: %s\n", string(out))
	} else {
		fmt.Println("  stopped ephemerd service")
	}

	// Disable the service
	if out, err := exec.Command("systemctl", "disable", "ephemerd").CombinedOutput(); err != nil {
		fmt.Printf("  note: could not disable service: %s\n", string(out))
	} else {
		fmt.Println("  disabled ephemerd service")
	}

	// Remove the unit file
	unitFile := "/etc/systemd/system/ephemerd.service"
	if err := os.Remove(unitFile); err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("  could not remove %s: %v\n", unitFile, err)
		}
	} else {
		fmt.Printf("  removed %s\n", unitFile)
	}

	// Reload systemd
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		fmt.Printf("  note: daemon-reload failed: %s\n", string(out))
	}
}

func uninstallLaunchd() {
	plist := "/Library/LaunchDaemons/dev.ephpm.ephemerd.plist"

	// Unload the service
	if out, err := exec.Command("launchctl", "unload", plist).CombinedOutput(); err != nil {
		fmt.Printf("  note: could not unload service: %s\n", string(out))
	} else {
		fmt.Println("  unloaded launchd service")
	}

	// Remove the plist
	if err := os.Remove(plist); err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("  could not remove %s: %v\n", plist, err)
		}
	} else {
		fmt.Printf("  removed %s\n", plist)
	}
}

func uninstallWindowsService() {
	// Stop the service
	if out, err := exec.Command("sc.exe", "stop", "ephemerd").CombinedOutput(); err != nil {
		fmt.Printf("  note: could not stop service: %s\n", string(out))
	} else {
		fmt.Println("  stopped ephemerd service")
	}

	// Delete the service
	if out, err := exec.Command("sc.exe", "delete", "ephemerd").CombinedOutput(); err != nil {
		fmt.Printf("  note: could not delete service: %s\n", string(out))
	} else {
		fmt.Println("  removed Windows service")
	}

}
