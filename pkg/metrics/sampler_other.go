//go:build !linux && !windows

package metrics

import (
	"context"
	"errors"
)

// noopSampler is the cross-compile stub returned on platforms without a
// native sampler (currently anything other than Linux and Windows). It
// always returns an error so the registry skips the sample without
// inventing a misleading zero metric.
type noopSampler struct{}

func (noopSampler) Sample(_ context.Context) (ContainerStats, error) {
	return ContainerStats{}, errors.New("per-container sampling not implemented on this platform")
}

// NewNoopSampler returns the cross-compile stub sampler.
func NewNoopSampler() Sampler { return noopSampler{} }
