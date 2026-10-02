//go:build windows

package main

import "log/slog"

// resolveJobProxyEnv declines to advertise cache proxies on Windows.
//
// Container jobs run inside a Linux VM (or Hyper-V containers), so the
// advertised CNI bridge gateway is unreachable from them — the same defect
// proven on macOS, where it left every cache at 0 bytes. Windows is worse than
// useless though: a job handed an unreachable GOPROXY HANGS rather than failing
// over, so this has long been suppressed wholesale rather than corrected.
//
// Suppression stays until the Hyper-V switch address a VM can reach the host on
// is determined ON METAL and probed, exactly as the darwin path now does.
// Guessing it would reintroduce hung builds, which is strictly worse than the
// bandwidth this costs.
func resolveJobProxyEnv(_ []string, log *slog.Logger) []string {
	if log != nil {
		log.Info("cache proxies not advertised to jobs on windows; the host address reachable from job VMs is not yet determined (bandwidth cost, not a failure)")
	}
	return nil
}
