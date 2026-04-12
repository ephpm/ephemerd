//go:build darwin

package main

import (
	"fmt"
	"os"
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

func printNextSteps(dataDir string) {
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Edit %s/config.toml (set github.owner)\n", dataDir)
	fmt.Println("    2. Set GITHUB_TOKEN in the launchd plist or /etc/default/ephemerd")
	fmt.Println("    3. sudo launchctl load /Library/LaunchDaemons/dev.ephpm.ephemerd.plist")
}
