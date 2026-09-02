package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// retryInit calls fn until it succeeds, up to attempts tries with delay
// between them, honoring ctx cancellation while waiting. Each failure is
// logged at Warn with the attempt count; after the last attempt the last
// error is returned as-is, so a persistent misconfiguration still surfaces
// its real cause rather than a generic timeout.
//
// This exists for daemon-startup dependencies that are only transiently
// unavailable right after boot (see the networking.New call in serve), and
// mirrors the bounded connect loop pkg/containerd/server.go uses for its own
// client.
//
// ctx is checked BEFORE each attempt as well as between them. fn takes no
// ctx, and some fn's (vm.StartLinuxVM) block for MINUTES per attempt, so
// checking only between attempts meant a cancelled or deadline-expired ctx
// still bought one more full attempt — long enough for the Windows SCM to
// hard-kill the service mid-shutdown (30s limit), and long enough to blow
// straight through a wall-clock budget a caller set precisely to bound this.
//
// A ctx give-up still carries the last real error. "context deadline
// exceeded" on its own tells an operator nothing about WHY the dependency
// never came up.
func retryInit[T any](ctx context.Context, attempts int, delay time.Duration, log *slog.Logger, what string, fn func() (T, error)) (T, error) {
	var lastErr error
	giveUp := func(err error) (T, error) {
		var zero T
		if lastErr != nil {
			return zero, fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		return zero, err
	}

	for i := 1; i <= attempts; i++ {
		if err := ctx.Err(); err != nil {
			return giveUp(err)
		}
		v, err := fn()
		if err == nil {
			if i > 1 {
				log.Info(what+" ready", "attempts", i)
			}
			return v, nil
		}
		lastErr = err
		if i < attempts {
			log.Warn(what+" not ready, retrying", "attempt", i, "max_attempts", attempts, "retry_in", delay, "error", err)
			select {
			case <-ctx.Done():
				return giveUp(ctx.Err())
			case <-time.After(delay):
			}
		}
	}
	var zero T
	return zero, lastErr
}
