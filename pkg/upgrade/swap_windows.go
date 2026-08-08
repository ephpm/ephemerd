//go:build windows

package upgrade

import (
	"fmt"
	"os"
)

// swapBinary replaces the binary at installPath with the staged binary on
// Windows, keeping the previous one as installPath+".old" for rollback.
//
// Windows will NOT let you overwrite or delete a running .exe — the image
// file is locked for the life of the process. It DOES, however, permit
// RENAMING a running .exe within the same volume (the process keeps its
// handle to the now-renamed file). We exploit that: rename the live
// ephemerd.exe to ephemerd.exe.old, then move the staged binary into the
// canonical path. The Windows service's binPath still points at the
// canonical path, so no `sc config` / registry ImagePath edit is needed —
// the SCM launches the new bytes on the next start. This is why the
// rename-in-place strategy is preferred over reconfiguring binPath to a
// versioned path (which would drift the install layout every upgrade) or
// shipping a separate updater helper binary.
//
// os.Rename maps to MoveFileEx with MOVEFILE_REPLACE_EXISTING, so a stale
// .old from a prior upgrade is overwritten rather than blocking the swap.
func swapBinary(installPath, stagedPath string) (backup string, err error) {
	backup = installPath + ".old"
	_ = os.Remove(backup) // best-effort; Rename below replaces it if present
	if err := os.Rename(installPath, backup); err != nil {
		return "", fmt.Errorf("renaming running binary aside: %w", err)
	}
	if err := os.Rename(stagedPath, installPath); err != nil {
		if rbErr := os.Rename(backup, installPath); rbErr != nil {
			return "", fmt.Errorf("installing new binary: %w (and rollback failed: %v — binary is at %s)", err, rbErr, backup)
		}
		return "", fmt.Errorf("installing new binary: %w (rolled back)", err)
	}
	return backup, nil
}
