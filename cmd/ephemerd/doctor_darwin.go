//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func platformChecks(pass, warn, fail func(string)) {
	// macOS version
	out, err := exec.Command("sw_vers", "-productVersion").CombinedOutput()
	if err == nil {
		version := strings.TrimSpace(string(out))
		pass(fmt.Sprintf("macOS %s", version))
	}

	// Architecture
	out, err = exec.Command("uname", "-m").CombinedOutput()
	if err == nil {
		arch := strings.TrimSpace(string(out))
		if arch == "arm64" {
			pass("Apple Silicon (arm64) — Virtualization.framework available")
		} else {
			warn(fmt.Sprintf("architecture: %s — Virtualization.framework requires Apple Silicon", arch))
		}
	}

	// Virtualization.framework entitlement check
	// The binary needs com.apple.security.virtualization entitlement for VMs
	exe, err := os.Executable()
	if err == nil {
		out, err := exec.Command("codesign", "-d", "--entitlements", "-", exe).CombinedOutput()
		if err != nil {
			warn("binary is not code-signed — macOS VMs require virtualization entitlement")
		} else if strings.Contains(string(out), "com.apple.security.virtualization") {
			pass("virtualization entitlement present")
		} else {
			warn("binary is signed but missing com.apple.security.virtualization entitlement — macOS VM jobs will not work")
		}
	}

	// Check for macOS VM base image (if configured)
	// This is config-dependent, so just check the common location
	commonImagePaths := []string{
		"/var/lib/ephemerd/vm/macos/base.ipsw",
		os.ExpandEnv("$HOME/.ephemerd/vm/macos/base.ipsw"),
	}
	found := false
	for _, p := range commonImagePaths {
		if _, err := os.Stat(p); err == nil {
			pass(fmt.Sprintf("macOS VM base image found (%s)", p))
			found = true
			break
		}
	}
	if !found {
		warn("no macOS VM base image found — macOS-native jobs require a base image (see docs)")
	}
}

func checkDiskSpace(dataDir string, pass, warn, fail func(string)) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &stat); err != nil {
		warn(fmt.Sprintf("could not check disk space: %v", err))
		return
	}

	freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
	if freeGB < 10 {
		fail(fmt.Sprintf("low disk space: %.1f GB free (need at least 10 GB for macOS VM images)", freeGB))
	} else if freeGB < 30 {
		warn(fmt.Sprintf("%.1f GB free — macOS VM images are large, may run low", freeGB))
	} else {
		pass(fmt.Sprintf("%.1f GB free disk space", freeGB))
	}
}

func checkEmbeddedAssets(pass, warn, fail func(string)) {
	pass("embedded assets compiled in (verified at build time)")
}

func platformCleanup(dataDir string, pass, warn, fail func(string)) {
	// Clean stale VM clone directories
	clonesDir := fmt.Sprintf("%s/vm/macos/clones", dataDir)
	if entries, err := os.ReadDir(clonesDir); err == nil {
		removed := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := fmt.Sprintf("%s/%s", clonesDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				warn(fmt.Sprintf("could not remove stale VM clone %s: %v", e.Name(), err))
			} else {
				removed++
			}
		}
		if removed > 0 {
			pass(fmt.Sprintf("removed %d stale macOS VM clone(s)", removed))
		} else {
			pass("no stale macOS VM clones")
		}
	}
}
