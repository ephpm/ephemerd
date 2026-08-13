//go:build !darwin

package runnerbusy

import "context"

// ProcessGroupBusy has no implementation off macOS: native (un-contained)
// runners only exist on macOS hosts. Returns Unknown + ErrUnsupported so
// the caller falls through rather than mistaking "no probe" for "not
// busy".
func ProcessGroupBusy(_ context.Context, _ int) (State, error) {
	return Unknown, ErrUnsupported
}
