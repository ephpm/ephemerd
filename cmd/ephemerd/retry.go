package main

import (
	"context"
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
func retryInit[T any](ctx context.Context, attempts int, delay time.Duration, log *slog.Logger, what string, fn func() (T, error)) (T, error) {
	var lastErr error
	for i := 1; i <= attempts; i++ {
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
				var zero T
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	var zero T
	return zero, lastErr
}
