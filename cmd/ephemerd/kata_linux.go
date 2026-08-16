//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/ephpm/ephemerd/pkg/config"
)

// kataShimBinary is the shim containerd execs when a container names the
// io.containerd.kata.v2 runtime handler. containerd resolves it from PATH,
// which pkg/containerd/server.go seeds with <data-dir>/bin plus the
// daemon's inherited PATH — so a packaged install under /opt/kata with a
// symlink into /usr/local/bin is found.
const kataShimBinary = "containerd-shim-kata-v2"

// checkKataPrereqs fails startup when [runner.linux] runtime = "kata" is
// configured but the host cannot actually run Kata containers.
//
// This is fatal rather than a warning on purpose. The whole point of the
// knob is an isolation guarantee: each job gets its own kernel. If the
// shim or /dev/kvm is missing, the alternative to failing is creating
// containers that quietly land on runc — untrusted CI code sharing the
// host kernel while the operator believes it is VM-isolated. A daemon
// that refuses to start is loud, immediately actionable, and cannot be
// mistaken for a working deployment; a warning in a log nobody reads
// cannot make that claim.
func checkKataPrereqs(cfg *config.Config, log *slog.Logger) error {
	if cfg.Runner.Linux.ResolvedRuntime() != config.LinuxRuntimeKata {
		return nil
	}

	shimPath, err := exec.LookPath(kataShimBinary)
	if err != nil {
		return fmt.Errorf(`runner.linux.runtime = "kata" but %s was not found in PATH `+
			`(install Kata Containers and make the shim reachable, e.g. `+
			`ln -s /opt/kata/bin/%s /usr/local/bin/%s): %w`,
			kataShimBinary, kataShimBinary, kataShimBinary, err)
	}

	// /dev/kvm must exist *and* be openable — on a nested-virt guest the
	// node can be missing KVM entirely, and a present-but-unopenable node
	// (permissions, or an exhausted hypervisor) fails just as hard at the
	// first job rather than at startup.
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		hint := "this host has no usable KVM device"
		if os.IsNotExist(err) {
			hint = "/dev/kvm does not exist — on a VM this usually means nested " +
				"virtualization is not enabled for this guest"
		} else if os.IsPermission(err) {
			hint = "/dev/kvm exists but is not openable — check that ephemerd runs " +
				"as root or is in the kvm group"
		}
		return fmt.Errorf(`runner.linux.runtime = "kata" but %s: %w`, hint, err)
	}
	_ = f.Close()

	// dind is supported under Kata, on a different transport: the Docker API
	// is served over TCP on the bridge gateway and handed to the job as
	// DOCKER_HOST, because a bind-mounted unix socket carries no endpoint
	// across the VM boundary. Log it — an operator reading "kata + dind"
	// should be able to see which transport was chosen without reading code,
	// since it is the difference between a working docker.sock and one that
	// refuses every call.
	if cfg.Dind.Enabled {
		log.Info("dind will use the TCP transport under kata",
			"reason", "a bind-mounted unix socket has no listening endpoint inside a guest kernel")
	}

	log.Info("kata runtime preflight passed",
		"shim", shimPath,
		"runtime", cfg.Runner.Linux.ContainerdRuntime(),
		"dind_enabled", cfg.Dind.Enabled,
		"kata_version", kataVersion())
	return nil
}

// kataVersion best-effort reports the installed Kata version for the
// startup log. Never fails the preflight — it is diagnostics, not a gate.
func kataVersion() string {
	for _, bin := range []string{"kata-runtime", "/opt/kata/bin/kata-runtime"} {
		out, err := exec.Command(bin, "--version").Output()
		if err != nil {
			continue
		}
		if line, _, ok := strings.Cut(string(out), "\n"); ok {
			return strings.TrimSpace(line)
		}
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}
