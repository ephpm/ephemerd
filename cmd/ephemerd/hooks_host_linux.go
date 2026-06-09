//go:build linux

package main

import (
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runtime"
)

// buildHostSamplerHooks returns the OnTaskStarted / OnTaskDestroy callbacks
// for ephemerd running on a Linux *host* (not the in-VM worker). Builds a
// cgroupv2 sampler over the container's containerd task and registers it
// with the host-local SamplerRegistry under the linux-native label.
//
// cpuLimit / memLimitBytes are 0 today because Linux host containers don't
// have configured resource caps in ephemerd; the sampler still records
// what the kernel reports.
func buildHostSamplerHooks(reg *metrics.SamplerRegistry, log *slog.Logger, _, _ uint64) (onStarted, onDestroy func(*runtime.RunnerEnv)) {
	if reg == nil {
		return nil, nil
	}
	onStarted = func(env *runtime.RunnerEnv) {
		sampler := metrics.NewLinuxSampler(env.Task, 0, 0).WithLogger(log)
		if env.Netns != "" {
			sampler = sampler.WithNetwork(env.Netns)
		}
		reg.Register(env.ID, env.Repo, metrics.RuntimeLinuxNative, sampler)
	}
	onDestroy = func(env *runtime.RunnerEnv) {
		reg.Unregister(env.ID, env.Repo, metrics.RuntimeLinuxNative)
	}
	return onStarted, onDestroy
}
