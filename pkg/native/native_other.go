//go:build !darwin

package native

import (
	"context"
	"fmt"
	"log/slog"
)

// Runner is a stub on non-darwin platforms.
type Runner struct{}

// New returns an error on non-darwin platforms.
func New(_, _, _, _ string, _ *slog.Logger) (*Runner, error) {
	return nil, fmt.Errorf("native macOS runner is only supported on darwin")
}

// SetRunAsUser is a stub on non-darwin platforms.
func (r *Runner) SetRunAsUser(_ string) {}

// Start is a stub on non-darwin platforms.
func (r *Runner) Start(_ context.Context) error {
	return fmt.Errorf("native macOS runner is only supported on darwin")
}

// Wait is a stub on non-darwin platforms.
func (r *Runner) Wait() (int, error) {
	return -1, fmt.Errorf("native macOS runner is only supported on darwin")
}

// Stop is a stub on non-darwin platforms.
func (r *Runner) Stop() {}
