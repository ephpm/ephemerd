//go:build windows

package scheduler

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Well-known SIDs for the control-socket DACL.
const (
	systemSID = "S-1-5-18"     // NT AUTHORITY\SYSTEM (the daemon's account)
	adminsSID = "S-1-5-32-544" // BUILTIN\Administrators
)

// secureControlSocket applies a real NTFS ACL to the AF_UNIX control socket
// file. On Windows, os.Chmod maps POSIX mode bits to at most a read-only
// attribute — it does NOT produce a restrictive DACL for a socket file, so
// access is otherwise governed by the inherited, world-traversable
// C:\ProgramData ACL. Any local user could then open the socket and KillJob or
// stream job logs. We instead break inheritance and grant full control to only
// SYSTEM (the daemon) and Administrators, mirroring the containerd control-pipe
// and HvSocket security descriptors used elsewhere in the daemon.
func secureControlSocket(path string) error {
	// /inheritance:r drops inherited ACEs (removing ProgramData's permissive
	// grants); the explicit grants restore access for the two principals that
	// legitimately administer the daemon.
	cmd := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant", "*"+systemSID+":(F)",
		"/grant", "*"+adminsSID+":(F)",
		"/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("icacls lock-down control socket %s: %w: %s", path, err, string(out))
	}
	return nil
}
