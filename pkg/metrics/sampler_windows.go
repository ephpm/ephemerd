//go:build windows

package metrics

import (
	"context"
	"errors"
	"fmt"

	"github.com/Microsoft/hcsshim"
)

// hcsStatsReader is the minimal subset of hcsshim.Container we need. Lets
// tests inject a fake without spinning up a real Hyper-V container.
type hcsStatsReader interface {
	Statistics() (hcsshim.Statistics, error)
}

// WindowsSampler reads HCS statistics for a running Hyper-V isolated
// Windows container. The configured CPU and memory limits are constants
// for the container's lifetime and are baked in at construction.
type WindowsSampler struct {
	reader   hcsStatsReader
	cpuLimit uint64
	memLimit uint64
}

// NewWindowsSampler builds a Sampler over an hcsshim.Container. cpuLimit
// is in cores (0 = unlimited); memLimitBytes is in bytes (0 = unlimited).
func NewWindowsSampler(reader hcsStatsReader, cpuLimit, memLimitBytes uint64) *WindowsSampler {
	return &WindowsSampler{reader: reader, cpuLimit: cpuLimit, memLimit: memLimitBytes}
}

// NewWindowsSamplerByID opens the compute system for the given containerd
// container ID and returns a sampler over it. The container handle is
// retained for the sampler's lifetime; close it via the returned closer
// when the container is destroyed.
func NewWindowsSamplerByID(id string, cpuLimit, memLimitBytes uint64) (*WindowsSampler, func() error, error) {
	c, err := hcsshim.OpenContainer(id)
	if err != nil {
		return nil, nil, fmt.Errorf("opening compute system %q: %w", id, err)
	}
	return NewWindowsSampler(c, cpuLimit, memLimitBytes), c.Close, nil
}

// Sample reads the latest HCS statistics. HCS doesn't distinguish anon
// from file-backed memory the way cgroupv2 does, so MemoryAnonBytes is
// always reported as 0 on Windows. Network bytes are summed across all
// HCS endpoints attached to the compute system.
func (s *WindowsSampler) Sample(_ context.Context) (ContainerStats, error) {
	if s.reader == nil {
		return ContainerStats{}, errors.New("nil HCS reader")
	}
	stats, err := s.reader.Statistics()
	if err != nil {
		return ContainerStats{}, fmt.Errorf("reading HCS statistics: %w", err)
	}
	var rx, tx uint64
	for _, n := range stats.Network {
		rx += n.BytesReceived
		tx += n.BytesSent
	}
	// HCS reports CPU time in 100-nanosecond units (Windows tick).
	return ContainerStats{
		CPUUsageNanos:    stats.Processor.TotalRuntime100ns * 100,
		MemoryBytes:      stats.Memory.UsageCommitBytes,
		MemoryAnonBytes:  0,
		CPULimit:         s.cpuLimit,
		MemoryLimitBytes: s.memLimit,
		NetworkRxBytes:   rx,
		NetworkTxBytes:   tx,
	}, nil
}
