//go:build windows

package runnerversion

import (
	"errors"
	"io/fs"
)

func ownerOf(fs.FileInfo) (uid, gid int) { return -1, -1 }

// chownTree is a Linux concern: the override overlay over the image's
// /home/runner is Linux-only.
func chownTree(string, int, int) error {
	return errors.New("chown is not supported on Windows")
}
