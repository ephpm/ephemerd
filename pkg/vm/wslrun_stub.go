//go:build !windows

package vm

import (
	"context"
	"fmt"
)

// NewRunDistro is only available on Windows.
func NewRunDistro(_ context.Context, _ RunDistroConfig) (*RunDistro, error) {
	return nil, fmt.Errorf("WSL delegation is only available on Windows")
}

// Run is only available on Windows.
func (d *RunDistro) Run(_ context.Context, _ RunInWSLConfig) (int, error) {
	return 1, fmt.Errorf("WSL delegation is only available on Windows")
}

// Destroy is only available on Windows.
func (d *RunDistro) Destroy() {}
