//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func platformChecks(pass, warn, fail func(string)) {
	// WSL2
	wslPath, err := exec.LookPath("wsl.exe")
	if err != nil {
		warn("WSL not found — Linux jobs will not be available on this host")
	} else {
		pass(fmt.Sprintf("WSL available (%s)", wslPath))

		// Check WSL version — output is UTF-16 on Windows, so also check --status
		out, err := exec.Command("wsl.exe", "--status").CombinedOutput()
		if err != nil {
			// Fallback: try --version
			out, err = exec.Command("wsl.exe", "--version").CombinedOutput()
		}
		if err != nil {
			warn("could not determine WSL version")
		} else {
			output := cleanUTF16(string(out))
			if strings.Contains(output, "2") || strings.Contains(strings.ToLower(output), "wsl2") || strings.Contains(strings.ToLower(output), "wsl version") {
				pass("WSL2 detected")
			} else {
				warn("WSL version unclear — ensure WSL2 is the default (wsl --set-default-version 2)")
			}
		}
	}

	// Hyper-V — check via PowerShell
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-Hypervisor).State").CombinedOutput()
	if err != nil {
		warn("could not check Hyper-V status")
	} else {
		state := strings.TrimSpace(string(out))
		if state == "Enabled" {
			pass("Hyper-V hypervisor enabled")
		} else {
			warn(fmt.Sprintf("Hyper-V hypervisor state: %s — Windows container isolation requires Hyper-V", state))
		}
	}

	// Windows build version
	out, err = exec.Command("cmd", "/c", "ver").CombinedOutput()
	if err == nil {
		pass(fmt.Sprintf("Windows version: %s", strings.TrimSpace(string(out))))
	}

	// Containers feature
	out, err = exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-WindowsOptionalFeature -Online -FeatureName Containers).State").CombinedOutput()
	if err == nil {
		state := strings.TrimSpace(string(out))
		if state == "Enabled" {
			pass("Windows Containers feature enabled")
		} else {
			warn("Windows Containers feature not enabled — run: Enable-WindowsOptionalFeature -Online -FeatureName Containers")
		}
	}
}

func checkDiskSpace(dataDir string, pass, warn, fail func(string)) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	dir, err := syscall.UTF16PtrFromString(dataDir)
	if err != nil {
		warn(fmt.Sprintf("could not check disk space: %v", err))
		return
	}

	var freeBytesAvailable uint64
	ret, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if ret == 0 {
		warn(fmt.Sprintf("could not check disk space: %v", err))
		return
	}

	freeGB := float64(freeBytesAvailable) / (1024 * 1024 * 1024)
	if freeGB < 10 {
		fail(fmt.Sprintf("low disk space: %.1f GB free (need at least 10 GB for Windows container images)", freeGB))
	} else if freeGB < 30 {
		warn(fmt.Sprintf("%.1f GB free — Windows container images are large, may run low", freeGB))
	} else {
		pass(fmt.Sprintf("%.1f GB free disk space", freeGB))
	}
}

func checkEmbeddedAssets(pass, warn, fail func(string)) {
	pass("embedded assets compiled in (verified at build time)")
}

// cleanUTF16 strips null bytes and BOM from Windows command output.
func cleanUTF16(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.TrimPrefix(s, "\xfe\xff")
	s = strings.TrimPrefix(s, "\xff\xfe")
	return strings.TrimSpace(s)
}

func platformCleanup(dataDir string, pass, warn, fail func(string)) {
	// Check for stale WSL distros
	out, err := exec.Command("wsl.exe", "--list", "--quiet").CombinedOutput()
	if err != nil {
		return
	}

	lines := strings.Split(cleanUTF16(string(out)), "\n")
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "ephemerd-") {
			out, err := exec.Command("wsl.exe", "--unregister", name).CombinedOutput()
			if err != nil {
				warn(fmt.Sprintf("could not remove stale WSL distro %s: %s", name, cleanUTF16(string(out))))
			} else {
				pass(fmt.Sprintf("removed stale WSL distro: %s", name))
			}
		}
	}

	// Clean stale VM directories
	vmDir := filepath.Join(dataDir, "vm")
	if entries, err := os.ReadDir(vmDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if e.Name() == "embed" {
				continue // keep embedded assets
			}
			path := filepath.Join(vmDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				warn(fmt.Sprintf("could not remove stale VM dir %s: %v", path, err))
			} else {
				pass(fmt.Sprintf("cleaned stale VM directory: %s", e.Name()))
			}
		}
	}
}
