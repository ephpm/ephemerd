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
// inheritance from the world-traversable C:\ProgramData ACL, re-grants
// SYSTEM + Administrators FULL control (so the daemon can still create, clean,
// and manage per-job subdirectories), and grants the Hyper-V VM group
// traverse-only (read+execute) so utility VMs can step into a per-job
// subdirectory whose DACL explicitly grants them Modify.
//
// The principal grants differ deliberately:
//   - SYSTEM/Administrators get (OI)(CI)F: the daemon runs as SYSTEM and MUST
//     retain write on this directory — it creates each `job-<id>` dir here at
//     Create() time (copyDirForJob) and removes orphan `job-*` dirs here at
//     startup. Granting only (RX) would break inheritance's SYSTEM full-control
//     ACE and leave the daemon unable to add or delete subdirectories, failing
//     every Windows job that needs a runner mount. The inheritable flags also
//     ensure per-job dirs stay daemon-manageable (create/cleanup/RemoveAll).
//   - The VM group gets (RX) with NO inherit flags: utility VMs only need to
//     traverse the parent to reach their own job dir, and each job dir receives
//     its own explicit VM-group Modify ACE in grantHyperVModify — we do not want
//     a blanket inherited VM-group ACE on every child.
func grantHyperVTraverse(path string) error {
	cmd := exec.Command("icacls", traverseGrantArgs(path)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls lock-down + grant SYSTEM/Admins F, VM-group RX on %s: %w: %s", path, err, string(out))
	}
	return nil
}

// traverseGrantArgs builds the icacls arguments for the runners parent
// lock-down. Split out so the grant POLICY (who gets what) is unit-testable
// without a live Windows host or actually invoking icacls.
//
//   - /inheritance:r drops inherited ACEs (removing ProgramData's world-readable
//     grant) — but that also drops the inherited SYSTEM/Admins full-control ACE,
//     so we MUST re-grant them write or the daemon can no longer create/clean
//     per-job dirs here.
//   - SYSTEM + Administrators: (OI)(CI)F — full and inheritable.
//   - VM group: (RX) only, non-inheritable — traverse to reach a job dir; each
//     job dir gets its own explicit VM-group Modify ACE in grantHyperVModify.
func traverseGrantArgs(path string) []string {
	return []string{
		path,
		"/inheritance:r",
		"/grant", "*" + systemSID + ":(OI)(CI)F",
		"/grant", "*" + adminsSID + ":(OI)(CI)F",
		"/grant", "*" + vmGroupSID + ":(RX)",
		"/C", "/Q",
	}
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
	cmd := exec.Command("icacls", modifyGrantArgs(path)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls grant VM-group M on %s: %w: %s", path, err, string(out))
	}
	return nil
}

// modifyGrantArgs builds the icacls arguments granting the VM group Modify on a
// per-job directory, inheritable so files the runner writes also get the ACE.
// Split out for the same unit-testability reason as traverseGrantArgs.
func modifyGrantArgs(path string) []string {
	return []string{path, "/grant", "*" + vmGroupSID + ":(OI)(CI)M", "/C", "/Q"}
}
