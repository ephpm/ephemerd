//go:build !windows

package upgrade

import (
	"fmt"
	"os"
)

// swapBinary replaces the binary at installPath with the staged binary,
// keeping the previous one as installPath+".old" for rollback. Returns the
// backup path.
//
// On Linux/macOS a running executable's file can be replaced while the
// process runs: the running process keeps the old inode open, and the path
// is repointed atomically. We move the current binary aside (so a rollback
// is a single rename back) and rename the staged binary — on the same
// filesystem — into place. The service manager restart then execs the new
// inode.
func swapBinary(installPath, stagedPath string) (backup string, err error) {
	backup = installPath + ".old"
	// Best-effort remove of a stale backup from a previous upgrade; a
	// leftover .old must not block the rename.
	_ = os.Remove(backup)
	if err := os.Rename(installPath, backup); err != nil {
		return "", fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(stagedPath, installPath); err != nil {
		// Roll the backup back so we don't leave the node with no binary.
		if rbErr := os.Rename(backup, installPath); rbErr != nil {
			return "", fmt.Errorf("installing new binary: %w (and rollback failed: %w — binary is at %s)", err, rbErr, backup)
		}
		return "", fmt.Errorf("installing new binary: %w (rolled back)", err)
	}
	return backup, nil
}
