package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/urfave/cli/v3"
)

func uninstallCmd() *cli.Command {
	var keepData bool
	return &cli.Command{
		Name:  "uninstall",
		Usage: "Remove ephemerd binary, service, and optionally all data",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "keep-data",
				Usage:       "keep the data directory (config, logs, container state)",
				Destination: &keepData,
			},
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			dataDir := configDir

			fmt.Println("Uninstalling ephemerd...")
			fmt.Println()

			// Run doctor cleanup first to remove stale runtime state
			// (containers, network bridges, WSL distros, etc.)
			fmt.Println("Cleaning up runtime state...")
			cleanupRuntime(dataDir)
			fmt.Println()

			// Stop and remove the service
			switch runtime.GOOS {
			case "linux":
				uninstallSystemd()
			case "darwin":
				uninstallLaunchd()
			case "windows":
				uninstallWindowsService()
			}

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
