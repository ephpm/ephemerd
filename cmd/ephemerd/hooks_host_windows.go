//go:build windows

package main

import (
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// buildHostSamplerHooks returns the OnTaskStarted / OnTaskDestroy callbacks
// for ephemerd running on a Windows host. Opens the HCS compute system by
// the containerd container ID and registers an hcsshim-backed sampler with
// the host-local SamplerRegistry under the windows-hyperv label.
//
// cpuLimit / memLimitBytes are the configured caps from
// [runner.windows]; passed through verbatim so the limit gauges read what
// the operator actually configured (HCS doesn't echo the cap back).
func buildHostSamplerHooks(reg *metrics.SamplerRegistry, log *slog.Logger, cpuLimit, memLimitBytes uint64) (onStarted, onDestroy func(*runtime.RunnerEnv)) {
	if reg == nil {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	// closers tracks the per-container HCS handle returned alongside the
	// sampler so we can Close it in onDestroy. Map is per-process; access
	// is serialized through the SamplerRegistry's internal lock.
	closers := newHCSCloserSet()

	onStarted = func(env *runtime.RunnerEnv) {
		sampler, closer, err := metrics.NewWindowsSamplerByID(env.ID, cpuLimit, memLimitBytes)
		if err != nil {
			log.Warn("HCS sampler open failed; container will have no resource metrics", "id", env.ID, "repo", env.Repo, "error", err)
			return
		}
		closers.add(env.ID, closer)
		reg.Register(env.ID, env.Repo, metrics.RuntimeWindowsHyperV, sampler)
		log.Info("HCS sampler registered", "id", env.ID, "repo", env.Repo)
	}
	onDestroy = func(env *runtime.RunnerEnv) {
		reg.Unregister(env.ID, env.Repo, metrics.RuntimeWindowsHyperV)
		closers.closeAndRemove(env.ID)
	}
	return onStarted, onDestroy
}
