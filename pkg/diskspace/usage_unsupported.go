//go:build !linux && !darwin && !windows

package diskspace

import (
	"errors"
	"fmt"
)

// ErrUnsupported is returned by Check on platforms with no capacity probe
// wired up. Callers treat it the same way they treat any Check failure:
// log once and skip garbage collection rather than guessing.
var ErrUnsupported = errors.New("diskspace: unsupported platform")

func check(path string) (Usage, error) {
	return Usage{Path: path}, fmt.Errorf("%w", ErrUnsupported)
}
