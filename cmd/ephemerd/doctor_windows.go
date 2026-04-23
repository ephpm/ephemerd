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
	// Hyper-V — required for both Windows containers and the Linux VM
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-Hypervisor).State").CombinedOutput()
	if err != nil {
		warn("could not check Hyper-V status")
	} else {
		state := strings.TrimSpace(string(out))
		if state == "Enabled" {
			pass("Hyper-V hypervisor enabled")
		} else {
			fail(fmt.Sprintf("Hyper-V hypervisor state: %s — required for Windows containers and Linux VM", state))
		}
	}

	// Hyper-V management tools (PowerShell module)
	out, err = exec.Command("powershell", "-NoProfile", "-Command",
		"Get-Command Get-VM -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "Get-VM" {
		warn("Hyper-V PowerShell module not available — Linux VM IP discovery may fail")
	} else {
		pass("Hyper-V PowerShell management tools available")
	}

	// Default Switch (used for Linux VM networking)
	out, err = exec.Command("powershell", "-NoProfile", "-Command",
		"Get-VMSwitch -Name 'Default Switch' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		warn("Hyper-V Default Switch not found — Linux VM networking may fail")
	} else {
		pass("Hyper-V Default Switch available")
	}

	// WSL2 (optional — only needed for interactive 'ephemerd run' on Windows)
	wslPath, err := exec.LookPath("wsl.exe")
	if err != nil {
		pass("WSL not found (optional — only needed for 'ephemerd run')")
	} else {
		pass(fmt.Sprintf("WSL available for interactive use (%s)", wslPath))
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
			fail("Windows Containers feature not enabled — run: Enable-WindowsOptionalFeature -Online -FeatureName Containers -NoRestart; then reboot")
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
	// Check for stale Hyper-V VMs (created by ephemerd)
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-VM -Name 'ephemerd-*' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name").CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			stopOut, stopErr := exec.Command("powershell", "-NoProfile", "-Command",
				fmt.Sprintf("Stop-VM -Name '%s' -Force -TurnOff -ErrorAction SilentlyContinue; Remove-VM -Name '%s' -Force", name, name)).CombinedOutput()
			if stopErr != nil {
				warn(fmt.Sprintf("could not remove stale VM %s: %s", name, strings.TrimSpace(string(stopOut))))
			} else {
				pass(fmt.Sprintf("removed stale Hyper-V VM: %s", name))
			}
		}
	}

	// Check for stale WSL distros (from older ephemerd or 'ephemerd run')
	out, err = exec.Command("wsl.exe", "--list", "--quiet").CombinedOutput()
	if err == nil {
		lines := strings.Split(cleanUTF16(string(out)), "\n")
		for _, line := range lines {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			if strings.HasPrefix(name, "ephemerd-") {
				unregOut, unregErr := exec.Command("wsl.exe", "--unregister", name).CombinedOutput()
				if unregErr != nil {
					warn(fmt.Sprintf("could not remove stale WSL distro %s: %s", name, cleanUTF16(string(unregOut))))
				} else {
					pass(fmt.Sprintf("removed stale WSL distro: %s", name))
				}
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
