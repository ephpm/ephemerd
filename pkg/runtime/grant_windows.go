//go:build windows

package runtime

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Well-known SIDs used for the per-job runner-directory DACLs.
//
//   - vmGroupSID (S-1-5-83-0) = "NT VIRTUAL MACHINE\Virtual Machines". Hyper-V
//     utility VMs (used for the isolated Windows containers we create via
//     containerd's runhcs shim) open host paths — the VSMB-shared per-job
//     runner directory — through a per-VM virtual account whose token is a
//     member of this group. This is the exact principal hcsshim itself grants
//     when it prepares a host path for VSMB: see
//     github.com/Microsoft/hcsshim/internal/security.GrantVmGroupAccess, which
//     sets a DACL entry for sidVMGroup = "S-1-5-83-0" *directly* on the target
//     path (it never relies on inheritance from the VM Machines group).
//   - systemSID (S-1-5-18)  = "NT AUTHORITY\SYSTEM" — the account the ephemerd
//     daemon runs as; it must retain access to manage these dirs.
//   - adminsSID (S-1-5-32-544) = "BUILTIN\Administrators".
//
// We previously granted "Everyone" (S-1-1-0) here on the theory that the VM
// group ACE "does not reliably inherit" on Windows 11 client SKUs. That
// conflated two things: the VM group ACE does not *inherit* from a parent, but
// hcsshim's approach never relies on inheritance — it grants the VM group SID
// directly on the specific path the utility VM opens. Granting Everyone was
// scoped by *path*, not *principal*, so any local user or sibling job that
// could reach the filesystem read every job's checkout, artifacts, and
// (transiently) its JIT runner config. Granting S-1-5-83-0 directly, per path,
// gives the utility VM exactly the access it needs while keeping other
// principals out.
const (
	vmGroupSID = "S-1-5-83-0"
	systemSID  = "S-1-5-18"
	adminsSID  = "S-1-5-32-544"
)

// grantHyperVTraverse locks down the runners parent directory: it breaks
// inheritance from the world-traversable C:\ProgramData ACL, re-grants only
// SYSTEM + Administrators (so the daemon can still manage the tree), and grants
// the Hyper-V VM group traverse (read+execute) so utility VMs can step into a
// per-job subdirectory whose DACL explicitly grants them Modify. None of these
// ACEs are inheritable, so they do not propagate into per-job subdirectories.
func grantHyperVTraverse(path string) error {
	// /inheritance:r removes inherited ACEs (so ProgramData's world-readable
	// ACL no longer applies to this tree). We then explicitly re-grant the
	// principals that must retain access. RX = read+execute (traverse); no
	// (OI)(CI) flags, so the ACEs apply only to this directory itself.
	cmd := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant", "*"+systemSID+":(RX)",
		"/grant", "*"+adminsSID+":(RX)",
		"/grant", "*"+vmGroupSID+":(RX)",
		"/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls lock-down + grant VM-group RX on %s: %w: %s", path, err, string(out))
	}
	return nil
}

// grantHyperVModify grants the Hyper-V VM group Modify on a per-job directory,
// with Object Inherit + Container Inherit so files the runner writes during the
// job also get the ACE. Modify covers read+write+execute+delete but NOT
// changing the DACL itself. Because the ACE is scoped to the VM group SID
// (S-1-5-83-0) rather than Everyone, other local users and other jobs' non-VM
// processes are excluded. The residual is that a *concurrent* job's utility VM
// (a distinct virtual account, but still a VM-group member) could in principle
// open this directory — the same trust boundary hcsshim itself uses for VSMB,
// and a strict improvement over Everyone, which also admitted every
// interactive/local/authenticated principal on the host.
func grantHyperVModify(path string) error {
	cmd := exec.Command("icacls", path, "/grant", "*"+vmGroupSID+":(OI)(CI)M", "/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls grant VM-group M on %s: %w: %s", path, err, string(out))
	}
	return nil
}
