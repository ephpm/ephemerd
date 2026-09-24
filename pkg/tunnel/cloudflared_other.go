//go:build !linux && !windows

package tunnel

import (
	"io"
	"os/exec"
)

// applyPdeathsig is a no-op here — the Pdeathsig field isn't part of
// SysProcAttr on these platforms. The graceful Close() path is the sole
// shutdown mechanism.
func applyPdeathsig(_ *exec.Cmd) {}

// bindChildLifetime is a no-op here. Linux uses Pdeathsig and Windows uses a
// Job Object; neither has an equivalent on the remaining platforms (darwin is
// the one that matters, and it runs ephemerd under launchd rather than as a
// tunnel host), so the graceful path stands alone.
func bindChildLifetime(_ *exec.Cmd) (io.Closer, error) { return nil, nil }
