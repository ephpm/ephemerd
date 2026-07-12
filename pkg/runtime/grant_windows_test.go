//go:build windows

package runtime

import (
	"strings"
	"testing"
)

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

// argAfterGrant returns the ACL string granted to sid within an icacls arg
// slice, i.e. the token following the "/grant" that names sid. Returns "" if
// sid is not granted.
func argAfterGrant(args []string, sid string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "/grant" && strings.HasPrefix(args[i+1], "*"+sid+":") {
			return strings.TrimPrefix(args[i+1], "*"+sid+":")
		}
	}
	return ""
}

// TestTraverseGrantArgs pins the runners-parent lock-down policy. The daemon
// (SYSTEM) creates per-job dirs under this parent at Create() and removes orphan
// job-* dirs here at startup, so /inheritance:r (which drops the inherited
// SYSTEM full-control ACE) MUST be paired with an explicit write grant for
// SYSTEM — otherwise every Windows job that needs a runner mount fails to
// provision. Regression guard for exactly that: SYSTEM/Admins get inheritable
// Full, the VM group gets traverse-only.
func TestTraverseGrantArgs(t *testing.T) {
	args := traverseGrantArgs(`C:\ProgramData\ephemerd\runners`)

	// Inheritance must be broken to shed the world-readable ProgramData ACL.
	found := false
	for _, a := range args {
		if a == "/inheritance:r" {
			found = true
		}
	}
	if !found {
		t.Error("traverseGrantArgs must include /inheritance:r to drop the inherited ProgramData ACL")
	}

	// SYSTEM and Administrators must retain WRITE (Full), inheritable, or the
	// daemon cannot create/clean per-job subdirectories after inheritance is
	// broken. (RX) here would be the WIN-1 regression.
	for _, sid := range []string{systemSID, adminsSID} {
		acl := argAfterGrant(args, sid)
		if acl != "(OI)(CI)F" {
			t.Errorf("grant for %s = %q, want (OI)(CI)F (daemon must keep inheritable write on the parent)", sid, acl)
		}
		if acl == "(RX)" {
			t.Errorf("grant for %s is read-only — daemon would fail to create per-job dirs", sid)
		}
	}

	// The VM group only needs traverse, and must NOT be inheritable (each job
	// dir gets its own explicit Modify ACE instead).
	if acl := argAfterGrant(args, vmGroupSID); acl != "(RX)" {
		t.Errorf("grant for VM group = %q, want (RX) traverse-only", acl)
	}
}

// TestModifyGrantArgs pins that per-job dirs grant the VM group inheritable
// Modify and nothing broader (no Everyone).
func TestModifyGrantArgs(t *testing.T) {
	args := modifyGrantArgs(`C:\ProgramData\ephemerd\runners\job-abc`)
	if acl := argAfterGrant(args, vmGroupSID); acl != "(OI)(CI)M" {
		t.Errorf("per-job grant for VM group = %q, want (OI)(CI)M", acl)
	}
	if acl := argAfterGrant(args, "S-1-1-0"); acl != "" {
		t.Errorf("per-job dir must not grant Everyone (S-1-1-0), got %q", acl)
	}
}
