//go:build darwin

package main

import (
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// buildHostSamplerHooks is the macOS-host stub. The Darwin daemon only
// runs containers inside the Vz Linux VM; native macOS host has no
// native container runtime to sample. The in-VM stats path is wired
// separately via the dispatch consumer.
func buildHostSamplerHooks(_ *metrics.SamplerRegistry, _ *slog.Logger, _, _ uint64) (onStarted, onDestroy func(*runtime.RunnerEnv)) {
	return nil, nil
}
