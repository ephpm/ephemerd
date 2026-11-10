//go:build !linux

package runtime

import (
	"context"
	"errors"

	"github.com/ephpm/ephemerd/pkg/runnerversion"
)

// ProbeImageRunner is Linux-only: the official-image runner path (and the
// overlay override over it) does not exist on Windows, and on macOS the
// Linux runtime lives inside the VM.
func (r *Runtime) ProbeImageRunner(context.Context, string) (runnerversion.TreeInfo, error) {
	return runnerversion.TreeInfo{UID: -1, GID: -1}, errors.New("image runner probe is Linux-only")
}
