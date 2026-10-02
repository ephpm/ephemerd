//go:build !windows

package tunnel

import "log/slog"

// reapStrayCloudflared is a no-op off Windows. Linux binds children with
// Pdeathsig, which the kernel enforces from process creation onward, so a
// cloudflared cannot outlive its ephemerd in the first place and there is
// never anything to sweep. darwin runs under launchd and does not host a
// tunnel.
func reapStrayCloudflared(_ string, _ *slog.Logger) {}
