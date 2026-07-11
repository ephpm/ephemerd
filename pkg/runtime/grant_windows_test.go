//go:build windows

package runtime

import "testing"

// TestGrantSIDs locks in the well-known SIDs used for per-job runner-directory
// DACLs. WIN-1 was a regression where these grants used Everyone (S-1-1-0),
// exposing every job's checkout/artifacts/JIT config to all local principals.
// The fix is to grant the Hyper-V VM group directly (the same principal
// hcsshim's GrantVmGroupAccess uses), plus SYSTEM+Administrators on the parent.
func TestGrantSIDs(t *testing.T) {
	cases := map[string]string{
		"vmGroupSID": vmGroupSID,
		"systemSID":  systemSID,
		"adminsSID":  adminsSID,
	}
	want := map[string]string{
		"vmGroupSID": "S-1-5-83-0",   // NT VIRTUAL MACHINE\Virtual Machines
		"systemSID":  "S-1-5-18",     // NT AUTHORITY\SYSTEM
		"adminsSID":  "S-1-5-32-544", // BUILTIN\Administrators
	}
	for name, got := range cases {
		if got != want[name] {
			t.Errorf("%s = %q, want %q", name, got, want[name])
		}
	}

	// The over-broad Everyone SID must not have crept back into any grant
	// constant used for per-job directories.
	for name, got := range cases {
		if got == "S-1-1-0" {
			t.Errorf("%s grants Everyone (S-1-1-0); per-job dirs must be principal-scoped", name)
		}
	}
}
