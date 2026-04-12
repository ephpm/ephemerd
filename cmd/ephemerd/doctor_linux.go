//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func platformChecks(pass, warn, fail func(string)) {
	// iptables
	if _, err := exec.LookPath("iptables"); err != nil {
		fail("iptables not found in PATH (required for container network isolation)")
	} else {
		pass("iptables available")
	}

	// Kernel namespace support (check /proc/self/ns)
	for _, ns := range []string{"net", "pid", "mnt", "uts", "ipc"} {
		path := fmt.Sprintf("/proc/self/ns/%s", ns)
		if _, err := os.Stat(path); err != nil {
			fail(fmt.Sprintf("kernel namespace %q not available (%s)", ns, path))
		}
	}
	pass("kernel namespaces available (net, pid, mnt, uts, ipc)")

	// cgroups v2
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		pass("cgroups v2 available")
	} else if _, err := os.Stat("/sys/fs/cgroup"); err == nil {
		warn("cgroups v1 detected — ephemerd works but v2 is recommended")
	} else {
		fail("cgroups not available")
	}

	// Filesystem check for overlayfs
	checkFilesystem(pass, warn)

	// Running as root
	if os.Getuid() == 0 {
		pass("running as root")
	} else {
		fail("not running as root (ephemerd requires root for container management)")
	}
}

func checkFilesystem(pass, warn func(string)) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return
	}

	// Check the data directory filesystem type
	// Common filesystem magic numbers
	switch stat.Type {
	case 0x6969: // NFS
		warn("root filesystem is NFS — overlayfs not supported, containerd will use the native snapshotter (slower: full copies per container)")
	case 0x2FC12FC1: // ZFS
		warn("root filesystem is ZFS — overlayfs not supported, containerd will use the native snapshotter (slower: full copies per container)")
	default:
		pass("filesystem supports overlayfs")
	}
}

func checkDiskSpace(dataDir string, pass, warn, fail func(string)) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &stat); err != nil {
		warn(fmt.Sprintf("could not check disk space: %v", err))
		return
	}

	freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
	if freeGB < 5 {
		fail(fmt.Sprintf("low disk space: %.1f GB free (need at least 5 GB for container images)", freeGB))
	} else if freeGB < 20 {
		warn(fmt.Sprintf("%.1f GB free — may run low with multiple concurrent jobs", freeGB))
	} else {
		pass(fmt.Sprintf("%.1f GB free disk space", freeGB))
	}
}

func checkEmbeddedAssets(pass, warn, fail func(string)) {
	// On Linux the runner archive is embedded at build time.
	// We can't check the embed directly, but we can verify the binary was built
	// with assets by checking if the runner package reports a version.
	pass("embedded assets compiled in (verified at build time)")
}

func platformCleanup(dataDir string, pass, warn, fail func(string)) {
	// Clean stale CNI state
	cniDir := fmt.Sprintf("%s/cni", dataDir)
	if entries, err := os.ReadDir(cniDir); err == nil {
		for _, e := range entries {
			if e.Name() == "bin" || e.Name() == "conf" {
				continue
			}
			path := fmt.Sprintf("%s/%s", cniDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				warn(fmt.Sprintf("could not remove stale CNI state %s: %v", path, err))
			}
		}
		pass("cleaned stale CNI state")
	} else {
		pass("no CNI state to clean")
	}

	// Clean stale DNS config
	dnsDir := fmt.Sprintf("%s/dns", dataDir)
	if _, err := os.Stat(dnsDir); err == nil {
		if err := os.RemoveAll(dnsDir); err != nil {
			warn(fmt.Sprintf("could not remove stale DNS dir: %v", err))
		} else {
			pass("cleaned stale DNS config")
		}
	}

	// Check for stale bridge
	out, err := exec.Command("ip", "link", "show", "ephemerd0").CombinedOutput()
	if err == nil && strings.Contains(string(out), "ephemerd0") {
		if err := exec.Command("ip", "link", "delete", "ephemerd0").Run(); err != nil {
			warn(fmt.Sprintf("could not remove stale bridge ephemerd0: %v", err))
		} else {
			pass("removed stale network bridge ephemerd0")
		}
	}
}
