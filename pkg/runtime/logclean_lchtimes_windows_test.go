//go:build windows

package runtime

import (
	"errors"
	"time"
)

// lchtimes is a stub on Windows — the symlink test that calls it is skipped
// on this platform because creating symlinks needs admin/developer-mode
// privileges that CI runners typically lack.
func lchtimes(_ string, _ time.Time) error {
	return errors.New("lchtimes not implemented on windows")
}
