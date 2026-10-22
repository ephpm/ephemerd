package networking

import (
	"context"
	"strings"
	"time"
)

// setupAttempts and setupRetryDelay bound how hard a CNI ADD is retried
// after a transient failure. Vars, not consts, so tests can shrink the delay.
var (
	setupAttempts   = 3
	setupRetryDelay = 200 * time.Millisecond
)

// isTransientSetupError reports whether a CNI ADD failed for a reason that a
// second attempt can clear.
//
// The one seen on the fleet is EINTR from a plugin's netlink calls: the
// plugin runs as a separate process and reports the errno as text, so the
// error reaches us as "plugin type="loopback" failed (add): interrupted
// system call" rather than as a wrapped syscall.Errno. It shows up when
// other containers on the host are being wired up at the same moment
// (ducks, 2026-10-22: a postgres sibling started with no IP and no port
// forward because of it).
func isTransientSetupError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "interrupted system call")
}

// retrySetup runs attempt until it succeeds, fails with a non-transient
// error, or setupAttempts is used up. Between attempts it runs undo so a
// half-finished CNI ADD (an allocated IP, a created veth) is released
// before the next one starts.
func retrySetup[T any](ctx context.Context, attempt func() (T, error), undo func()) (T, error) {
	var (
		res T
		err error
	)
	for i := 0; i < setupAttempts; i++ {
		res, err = attempt()
		if err == nil || !isTransientSetupError(err) || i == setupAttempts-1 {
			return res, err
		}
		undo()
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(setupRetryDelay):
		}
	}
	return res, err
}
