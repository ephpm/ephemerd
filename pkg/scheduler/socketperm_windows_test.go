//go:build windows

package scheduler

import "testing"

// TestControlSocketSIDs locks in the principals granted on the Windows control
// socket. WIN-6: os.Chmod is a near-no-op for an AF_UNIX socket file on Windows,
// so the socket must instead be locked to SYSTEM (the daemon) and
// Administrators only — nothing broader — or a local user could KillJob and
// stream job-secret logs.
func TestControlSocketSIDs(t *testing.T) {
	if systemSID != "S-1-5-18" {
		t.Errorf("systemSID = %q, want S-1-5-18 (NT AUTHORITY\\SYSTEM)", systemSID)
	}
	if adminsSID != "S-1-5-32-544" {
		t.Errorf("adminsSID = %q, want S-1-5-32-544 (BUILTIN\\Administrators)", adminsSID)
	}
	for name, got := range map[string]string{"systemSID": systemSID, "adminsSID": adminsSID} {
		if got == "S-1-1-0" {
			t.Errorf("%s grants Everyone (S-1-1-0); control socket must stay privileged-only", name)
		}
	}
}
