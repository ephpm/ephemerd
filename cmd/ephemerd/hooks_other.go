//go:build !linux

package main

import (
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// buildDispatchSamplerHooks is the cross-compile stub. Worker mode (and
// therefore the in-VM cgroup sampler wiring) only runs on Linux; this
// stub exists so the call site in main.go compiles for Windows + Darwin.
func buildDispatchSamplerHooks(_ *scheduler.DispatchServer, _ *slog.Logger) (onStarted, onDestroy func(*runtime.RunnerEnv)) {
	return nil, nil
}
