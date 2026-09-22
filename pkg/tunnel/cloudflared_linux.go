//go:build linux

package tunnel

import (
	"io"
	"os/exec"
	"syscall"
)

// applyPdeathsig binds the subprocess's lifetime to the parent thread.
// The kernel sends SIGTERM to the child the instant the parent thread
// exits — this survives SIGKILL of ephemerd, panics, or any exit path
// that skips deferred cleanup.
func applyPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
	// Also run in its own process group so any children cloudflared spawns
	// stay attached to it (and thus get reaped when it does).
	cmd.SysProcAttr.Setpgid = true
}

// bindChildLifetime is a no-op on Linux: Pdeathsig above already binds the
// child to this process at the kernel level, and it applies from the moment
// the child is created rather than after Start.
func bindChildLifetime(_ *exec.Cmd) (io.Closer, error) { return nil, nil }
