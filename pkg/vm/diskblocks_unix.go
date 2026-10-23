//go:build !windows

package vm

import (
	"os"
	"syscall"
)

// diskBlocks reports the bytes a file actually occupies. st_blocks is in
// 512-byte units on every unix ephemerd runs on.
func diskBlocks(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}
