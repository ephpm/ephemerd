//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func installService(binPath, dataDir string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>dev.ephpm.ephemerd</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
        <string>--data-dir</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/ephemerd.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/ephemerd.log</string>
</dict>
</plist>
`, binPath, dataDir)

	plistPath := "/Library/LaunchDaemons/dev.ephpm.ephemerd.plist"
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}
	fmt.Printf("  service: %s\n", plistPath)

	return nil
}

// postInstallBinary re-signs the binary after copying on macOS.
func postInstallBinary(path string) error {
	return codesignBinary(path)
}

// codesignBinary ad-hoc signs the binary with the virtualization entitlement.
// On Apple Silicon, copying a signed binary invalidates its signature — the
// kernel will SIGKILL the process on launch. Re-signing after copy is required.
func codesignBinary(path string) error {
	// The entitlements plist is not available at runtime (it's a build-time
	// asset), so we create a minimal one in a temp file.
	entitlements := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.virtualization</key>
    <true/>
</dict>
</plist>
`
	tmp, err := os.CreateTemp("", "ephemerd-entitlements-*.plist")
	if err != nil {
		return fmt.Errorf("creating temp entitlements: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(entitlements); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			fmt.Printf("  warning: error closing temp file: %v\n", cerr)
		}
		return fmt.Errorf("writing entitlements: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp entitlements: %w", err)
	}

	fmt.Printf("  codesigning %s...\n", path)
	cmd := exec.Command("codesign", "--force", "--sign", "-", "--entitlements", tmp.Name(), path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printNextSteps(dataDir string) {
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Edit %s/config.toml (set github.owner)\n", dataDir)
	fmt.Println("    2. Set GITHUB_TOKEN in the launchd plist or /etc/default/ephemerd")
	fmt.Println("    3. sudo launchctl load /Library/LaunchDaemons/dev.ephpm.ephemerd.plist")
}
