//go:build !linux

package tunnel

import "os/exec"

// applyPdeathsig is a no-op off Linux — the Pdeathsig field isn't part of
// SysProcAttr on other platforms. The graceful Close() path is the sole
// shutdown mechanism there.
func applyPdeathsig(_ *exec.Cmd) {}
