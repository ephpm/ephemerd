//go:build windows

package runtime

import (
	"fmt"
	"os/exec"
	"syscall"
)

// everyoneSID is the well-known SID for "Everyone" (S-1-1-0). We grant to
// Everyone because Hyper-V utility VMs (used for the isolated Windows
// containers we create via containerd's runhcs shim) open host paths through
// a virtual account that does NOT inherit membership in
// "NT VIRTUAL MACHINE\Virtual Machines" (S-1-5-83-0) on Windows 11 client
// SKUs — we verified this empirically by observing VSMB Modify failures with
// ACCESS_DENIED even after the VM group had full inherited rights. The
// grants are narrowed by path: the parent runners directory only needs
// traverse (RX, no inheritance to children), while each per-job directory
// gets Modify with inheritance. That preserves isolation between concurrent
// jobs — the utility VM for job A can traverse `runners` but cannot touch
// job B's subdirectory because only job A's dir has an ACE for it.
const everyoneSID = "S-1-1-0"

// grantHyperVTraverse grants Everyone read+execute (traverse) on a directory
// without inheritance. Use this on the runners parent directory so Hyper-V
// utility VMs can step into any per-job subdirectory whose DACL explicitly
// grants them Modify access.
func grantHyperVTraverse(path string) error {
	// RX = GENERIC_READ + GENERIC_EXECUTE (traverse). No (OI)(CI) flags, so
	// the ACE applies only to the directory itself and does not leak into
	// per-job subdirectories.
	cmd := exec.Command("icacls", path, "/grant", "*"+everyoneSID+":(RX)", "/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls grant Everyone RX on %s: %w: %s", path, err, string(out))
	}
	return nil
}

// grantHyperVModify grants Everyone Modify on a per-job directory, with
// Object Inherit + Container Inherit so files the runner writes during the
// job also get the ACE. Modify covers read+write+execute+delete but NOT
// changing the DACL itself.
func grantHyperVModify(path string) error {
	cmd := exec.Command("icacls", path, "/grant", "*"+everyoneSID+":(OI)(CI)M", "/C", "/Q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls grant Everyone M on %s: %w: %s", path, err, string(out))
	}
	return nil
}
