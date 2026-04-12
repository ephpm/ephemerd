//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func installService(binPath, dataDir string) error {
	if _, err := os.Stat("/etc/systemd/system"); err != nil {
		fmt.Println("  systemd not found, skipping service installation")
		return nil
	}

	unit := fmt.Sprintf(`[Unit]
Description=ephemerd - Ephemeral GitHub Actions Runner Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve --data-dir %s
Restart=on-failure
RestartSec=5
EnvironmentFile=-/etc/default/ephemerd
KillMode=mixed
TimeoutStopSec=300

[Install]
WantedBy=multi-user.target
`, binPath, dataDir)

	if err := os.WriteFile("/etc/systemd/system/ephemerd.service", []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}
	fmt.Println("  service: ephemerd.service")

	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %s", string(out))
	}

	// Create env file for GITHUB_TOKEN
	envFile := "/etc/default/ephemerd"
	if _, err := os.Stat(envFile); err != nil {
		envContent := "# Set your GitHub token here\n# GITHUB_TOKEN=ghp_your_token_here\n"
		if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
			fmt.Printf("  warning: could not create %s: %v\n", envFile, err)
		} else {
			fmt.Printf("  env:     %s\n", envFile)
		}
	}

	return nil
}

func printNextSteps(dataDir string) {
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Edit %s/config.toml (set github.owner)\n", dataDir)
	fmt.Println("    2. Set GITHUB_TOKEN in /etc/default/ephemerd")
	fmt.Println("    3. sudo systemctl start ephemerd")
	fmt.Println("    4. sudo systemctl enable ephemerd")
}
