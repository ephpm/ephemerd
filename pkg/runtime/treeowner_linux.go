//go:build linux

package runtime

import (
	"fmt"
	"os"
	"syscall"
)

// matchTreeOwner gives the per-job overlay upperdir the same owner as the
// lowerdir's root. Overlayfs takes the MERGED root's attributes from the
// upper root when it exists (it always does here: we create it), not from
// the lower. So a lowerdir prepared for the official image's unprivileged
// `runner` user (uid/gid 1001, see runnerversion.PrepareOverrideTree) would
// still mount as a root-owned /home/runner, and run.sh's very first write
// (run-helper.sh, then .runner and _work/) would fail with EACCES. For the
// custom-image path the lowerdir is root-owned and this is a no-op.
func matchTreeOwner(upper, lower string) error {
	st, err := os.Stat(lower)
	if err != nil {
		return fmt.Errorf("stat runner tree %s: %w", lower, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if sys.Uid == 0 && sys.Gid == 0 {
		return nil
	}
	if err := os.Chown(upper, int(sys.Uid), int(sys.Gid)); err != nil {
		return fmt.Errorf("chowning job upperdir %s to %d:%d: %w", upper, sys.Uid, sys.Gid, err)
	}
	return nil
}
