package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/urfave/cli/v3"
)

func installCmd() *cli.Command {
	var noService bool
	var configFile string
	return &cli.Command{
		Name:  "install",
		Usage: "Install ephemerd as a system service",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "no-service",
				Usage:       "skip service installation (just copy binary and create config)",
				Destination: &noService,
			},
			&cli.StringFlag{
				Name:        "config",
				Aliases:     []string{"c"},
				Usage:       "path to config.toml to install into the data directory",
				Destination: &configFile,
			},
		},
		Action: func(_ context.Context, _ *cli.Command) error {
			dataDir := configDir

			fmt.Println()
			fmt.Println("  Installing ephemerd...")
			fmt.Println()

			// 1. Copy binary to install location
			installDir := defaultInstallDir()
			if err := installBinary(installDir); err != nil {
				return fmt.Errorf("installing binary: %w", err)
			}
			fmt.Printf("  binary:  %s\n", filepath.Join(installDir, binaryName()))

			// 2. Create data directory and config
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				return fmt.Errorf("creating data directory: %w", err)
			}
			fmt.Printf("  datadir: %s\n", dataDir)

			configPath := filepath.Join(dataDir, "config.toml")
			if configFile != "" {
				if err := copyFile(configFile, configPath); err != nil {
					return fmt.Errorf("copying config: %w", err)
				}
				fmt.Printf("  config:  %s (from %s)\n", configPath, configFile)
			} else if err := createDefaultConfig(configPath); err != nil {
				return fmt.Errorf("creating config: %w", err)
			}

			// 3. Install system service
			if !noService {
				binPath := filepath.Join(installDir, binaryName())
				if err := installService(binPath, dataDir); err != nil {
					fmt.Printf("  warning: could not install service: %v\n", err)
				}
			}

			fmt.Println()
			fmt.Println("  Installed!")
			fmt.Println()
			printNextSteps(dataDir)
			fmt.Println()

			return nil
		},
	}
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "ephemerd.exe"
	}
	return "ephemerd"
}

func defaultInstallDir() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Program Files\ephemerd`
	default:
		return "/usr/local/bin"
	}
}

// installBinary copies the running executable to the install directory.
// Skips if already running from that location.
func installBinary(installDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	dest := filepath.Join(installDir, binaryName())

	// Already in the right place
	if filepath.Clean(exe) == filepath.Clean(dest) {
		fmt.Printf("  binary already at %s\n", dest)
		return nil
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return fmt.Errorf("creating install directory: %w", err)
	}

	src, err := os.Open(exe)
	if err != nil {
		return fmt.Errorf("opening source binary: %w", err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			fmt.Printf("  warning: error closing source binary: %v\n", err)
		}
	}()

	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating destination binary: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		if cerr := dst.Close(); cerr != nil {
			fmt.Printf("  warning: error closing destination: %v\n", cerr)
		}
		return fmt.Errorf("copying binary: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("closing destination binary: %w", err)
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			fmt.Printf("  warning: error closing source: %v\n", err)
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		if cerr := out.Close(); cerr != nil {
			fmt.Printf("  warning: error closing dest: %v\n", cerr)
		}
		return err
	}
	return out.Close()
}

func createDefaultConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("  config:  %s (already exists, skipping)\n", path)
		return nil
	}

	config := `[github]
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional — omit for org-level runners

[runner]
max_concurrent = 4

[log]
level = "info"
`
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return err
	}
	fmt.Printf("  config:  %s\n", path)
	return nil
}
