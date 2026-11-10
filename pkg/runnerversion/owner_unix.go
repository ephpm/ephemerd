//go:build !windows

package runnerversion

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func ownerOf(st fs.FileInfo) (uid, gid int) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid), int(s.Gid)
	}
	return -1, -1
}

// chownTree sets uid:gid on every entry under root (symlinks included,
// not followed).
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
