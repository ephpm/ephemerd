//go:build linux

package main

import (
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// buildDispatchSamplerHooks returns the OnTaskStarted / OnTaskDestroy
// callbacks the in-VM Linux runtime uses to plug per-container cgroup
// samplers into the dispatch server's StreamContainerStats surface.
func buildDispatchSamplerHooks(ds *scheduler.DispatchServer, log *slog.Logger) (onStarted, onDestroy func(*runtime.RunnerEnv)) {
	if ds == nil {
		return nil, nil
	}
	onStarted = func(env *runtime.RunnerEnv) {
		sampler := metrics.NewLinuxSampler(env.Task, 0, 0).WithLogger(log)
		if env.Netns != "" {
			sampler = sampler.WithNetwork(env.Netns)
		}
		ds.RegisterSampler(env.ID, env.Repo, sampler)
	}
	onDestroy = func(env *runtime.RunnerEnv) {
		ds.UnregisterSampler(env.ID)
	}
	return onStarted, onDestroy
}
