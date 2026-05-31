//go:build linux

package dind

import (
	"fmt"
	"os"
	"syscall"
)

// chownNewDirsLikeAncestor copies the uid/gid of ancestor onto every dir
// in newDirs. Used by ensureBindSourceDir so an auto-created bind source
// inherits the runner user's ownership instead of being root-owned.
// Linux-only because Stat_t and the chown semantics differ on Windows
// (where ownership is ACL-based, not uid/gid).
func chownNewDirsLikeAncestor(newDirs []string, ancestor string) error {
	info, err := os.Stat(ancestor)
	if err != nil {
		return fmt.Errorf("re-stat ancestor %s: %w", ancestor, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Filesystem doesn't expose POSIX stat info — skip chown rather
		// than fail the whole bind (root ownership beats no mount).
		return nil
	}
	uid, gid := int(stat.Uid), int(stat.Gid)
	for _, d := range newDirs {
		if err := os.Chown(d, uid, gid); err != nil {
			return fmt.Errorf("chown %s to %d:%d: %w", d, uid, gid, err)
		}
	}
	return nil
}
