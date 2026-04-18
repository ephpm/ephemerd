//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

	// macOS VM disk image check
	commonImagePaths := []string{
		"/var/lib/ephemerd/vm/macos/base.img",
		os.ExpandEnv("$HOME/.ephemerd/vm/macos/base.img"),
	}
	found := false
	for _, p := range commonImagePaths {
		if info, err := os.Stat(p); err == nil {
			sizeGB := float64(info.Size()) / (1024 * 1024 * 1024)
			pass(fmt.Sprintf("macOS VM disk image found (%s, %.1f GB)", p, sizeGB))
			found = true
			break
		}
	}
	if !found {
		pass("no macOS VM disk image yet — ephemerd will download the Apple IPSW and install on first boot (~30 min, one-time)")
	}

	// VM capacity guidance
	hostCPUs := runtime.NumCPU()
	hostMem := getHostMemoryGB()
	// Linux VM always takes 1 slot (1 CPU, 1 GB default)
	macVMCPUs := 2  // default
	macVMMem := 2   // default GB
	maxMacVMs := hostCPUs/macVMCPUs - 1 // -1 for Linux VM
	if maxMacVMs < 1 {
		maxMacVMs = 1
	}
	memLimit := (int(hostMem) - 2) / macVMMem // -2 GB for Linux VM + host overhead
	if memLimit < maxMacVMs {
		maxMacVMs = memLimit
	}
	if maxMacVMs < 1 {
		maxMacVMs = 1
	}

	pass(fmt.Sprintf("host: %d CPUs, %.0f GB RAM", hostCPUs, hostMem))
	if maxMacVMs <= 1 {
		warn(fmt.Sprintf("this host can run ~%d macOS VM at a time (1 Linux VM + %d macOS VM = %d/%d CPUs, %d/%d GB RAM)",
			maxMacVMs, maxMacVMs,
			1+maxMacVMs*macVMCPUs, hostCPUs,
			1+maxMacVMs*macVMMem, int(hostMem)))
		warn("macOS jobs will queue and run one at a time — consider a host with more CPUs/RAM for parallel macOS jobs")
	} else {
		pass(fmt.Sprintf("can run up to %d concurrent macOS VMs (1 Linux VM @ 1 CPU + %d macOS VMs @ %d CPUs each)",
			maxMacVMs, maxMacVMs, macVMCPUs))
	}
	pass("tip: tune with [vm.macos] cpus, memory_mb, and max_concurrent in config.toml")
}

func getHostMemoryGB() float64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").CombinedOutput()
	if err != nil {
		return 0
	}
	var bytes uint64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &bytes)
	return float64(bytes) / (1024 * 1024 * 1024)
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
	clonesDir := fmt.Sprintf("%s/vm/macos/jobs", dataDir)
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
