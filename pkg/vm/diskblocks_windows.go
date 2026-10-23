package vm

import "os"

// diskBlocks falls back to the apparent size. macOS clones never live on a
// Windows host; this only keeps the sweep portable for tests.
func diskBlocks(fi os.FileInfo) int64 { return fi.Size() }
