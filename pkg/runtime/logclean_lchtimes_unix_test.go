//go:build unix

package runtime

import (
	"time"

	"golang.org/x/sys/unix"
)

// lchtimes sets the access+modification time of path without following
// symlinks. os.Chtimes follows symbolic links on Unix, so it can't be used
// to backdate a symlink itself for tests like
// TestCleanOldLogs_StaleSymlinkRemoved.
func lchtimes(path string, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Lutimes(path, []unix.Timeval{tv, tv})
}
