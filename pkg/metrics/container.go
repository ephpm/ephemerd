package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Runtime labels for per-container metrics. The label distinguishes which
// runtime instance produced the sample — see docs/arch/container-metrics.md.
const (
	RuntimeWindowsHyperV = "windows-hyperv"
	RuntimeLinuxVM       = "linux-vm"
	RuntimeLinuxNative   = "linux-native"
)

var containerLabels = []string{"id", "repo", "runtime"}

var (
	// ContainerCPUSeconds is the cumulative CPU time consumed by a container.
	// Use rate() in promQL to derive utilization.
	ContainerCPUSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_container_cpu_usage_seconds_total",
		Help: "Cumulative CPU time consumed by the container, in seconds.",
	}, containerLabels)

	// ContainerMemoryBytes is the current memory in use by a container.
	ContainerMemoryBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_container_memory_bytes",
		Help: "Current memory usage of the container, in bytes.",
	}, containerLabels)

	// ContainerMemoryAnonBytes is the current anonymous memory in use. On
	// Windows this is always 0 — HCS doesn't split anon from file-backed.
	ContainerMemoryAnonBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_container_memory_anon_bytes",
		Help: "Current anonymous (non-file-backed) memory usage of the container, in bytes. 0 on Windows.",
	}, containerLabels)

	// ContainerMemoryLimitBytes is the configured memory cap. 0 = unlimited.
	ContainerMemoryLimitBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_container_memory_limit_bytes",
		Help: "Configured memory limit for the container, in bytes. 0 means unlimited.",
	}, containerLabels)

	// ContainerCPULimit is the configured vCPU count. 0 = unlimited.
	ContainerCPULimit = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ephemerd_container_cpu_limit",
		Help: "Configured vCPU count for the container. 0 means unlimited.",
	}, containerLabels)

	// ContainerNetworkRxBytes is the cumulative network bytes received by
	// the container's network namespace. Counts the runner container only
	// — sibling containers spawned via dind get their own netns and are
	// not included. See docs/arch/container-metrics.md for the rationale.
	ContainerNetworkRxBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_container_network_rx_bytes_total",
		Help: "Cumulative bytes received by the container's network namespace (loopback excluded).",
	}, containerLabels)

	// ContainerNetworkTxBytes is the cumulative network bytes transmitted.
	ContainerNetworkTxBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ephemerd_container_network_tx_bytes_total",
		Help: "Cumulative bytes transmitted by the container's network namespace (loopback excluded).",
	}, containerLabels)
)

// ContainerStats is the platform-neutral sample shape produced by all
// samplers and consumed by RecordContainerStats.
type ContainerStats struct {
	CPUUsageNanos    uint64
	MemoryBytes      uint64
	MemoryAnonBytes  uint64
	CPULimit         uint64 // cores, 0 = unlimited
	MemoryLimitBytes uint64 // 0 = unlimited
	NetworkRxBytes   uint64 // cumulative bytes received (loopback excluded)
	NetworkTxBytes   uint64 // cumulative bytes transmitted (loopback excluded)
}

// Sampler is the per-container resource sampler interface. Implementations
// live in sampler_linux.go and sampler_windows.go (with a no-op stub in
// sampler_other.go for cross-compile).
type Sampler interface {
	Sample(ctx context.Context) (ContainerStats, error)
}

// RecordContainerStats applies a single sample to the metric series for
// (id, repo, runtime). Cumulative fields (CPU, network bytes) are
// translated into Prometheus counter Add() calls by diffing against the
// previous sample; gauges (memory, limits) are set directly.
func RecordContainerStats(id, repo, runtimeLabel string, s ContainerStats) {
	labels := prometheus.Labels{"id": id, "repo": repo, "runtime": runtimeLabel}
	key := containerKey{id, repo, runtimeLabel}

	counterMu.Lock()
	prev, ok := lastCounters[key]
	lastCounters[key] = cumulativeCounters{
		cpuNanos: s.CPUUsageNanos,
		rxBytes:  s.NetworkRxBytes,
		txBytes:  s.NetworkTxBytes,
	}
	counterMu.Unlock()

	if ok {
		if d := monotonicDelta(s.CPUUsageNanos, prev.cpuNanos); d > 0 {
			ContainerCPUSeconds.With(labels).Add(float64(d) / 1e9)
		}
		if d := monotonicDelta(s.NetworkRxBytes, prev.rxBytes); d > 0 {
			ContainerNetworkRxBytes.With(labels).Add(float64(d))
		}
		if d := monotonicDelta(s.NetworkTxBytes, prev.txBytes); d > 0 {
			ContainerNetworkTxBytes.With(labels).Add(float64(d))
		}
	}
	// First sample after Register (or counter reset — cur < prev — from an
	// in-VM restart) records no delta; Prometheus reads the next interval
	// as a fresh starting point.

	ContainerMemoryBytes.With(labels).Set(float64(s.MemoryBytes))
	ContainerMemoryAnonBytes.With(labels).Set(float64(s.MemoryAnonBytes))
	ContainerMemoryLimitBytes.With(labels).Set(float64(s.MemoryLimitBytes))
	ContainerCPULimit.With(labels).Set(float64(s.CPULimit))
}

// monotonicDelta returns cur-prev when cur >= prev, otherwise 0 — covers
// both the first-sample case and the counter-went-backwards case (process
// restart, sampler swap).
func monotonicDelta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// DeleteContainerSeries removes every metric series for (id, repo,
// runtime). Call when a container is destroyed to bound cardinality.
func DeleteContainerSeries(id, repo, runtimeLabel string) {
	labels := prometheus.Labels{"id": id, "repo": repo, "runtime": runtimeLabel}
	ContainerCPUSeconds.Delete(labels)
	ContainerMemoryBytes.Delete(labels)
	ContainerMemoryAnonBytes.Delete(labels)
	ContainerMemoryLimitBytes.Delete(labels)
	ContainerCPULimit.Delete(labels)
	ContainerNetworkRxBytes.Delete(labels)
	ContainerNetworkTxBytes.Delete(labels)

	counterMu.Lock()
	delete(lastCounters, containerKey{id, repo, runtimeLabel})
	counterMu.Unlock()
}

type containerKey struct{ id, repo, runtimeLabel string }

// cumulativeCounters caches the previous sample's monotonic counters so
// RecordContainerStats can derive Prometheus-counter Add() deltas.
type cumulativeCounters struct {
	cpuNanos uint64
	rxBytes  uint64
	txBytes  uint64
}

var (
	counterMu    sync.Mutex
	lastCounters = make(map[containerKey]cumulativeCounters)
)

// SamplerRegistry tracks active host-local Samplers and ticks them on a
// shared interval. Use this for native containers whose stats can be
// read directly in-process. The Linux-VM path bypasses this registry
// entirely — the in-VM ephemerd runs its own ticker and pushes batches
// to the host over the Dispatch stream, which calls RecordContainerStats
// directly.
type SamplerRegistry struct {
	mu       sync.Mutex
	samplers map[containerKey]registeredSampler
	interval time.Duration
	log      *slog.Logger
	cancel   context.CancelFunc
	done     chan struct{}
}

type registeredSampler struct {
	sampler Sampler
}

// NewSamplerRegistry creates a registry that ticks every interval. The
// returned registry must be Start()ed before samples flow, and Stop()ped
// at shutdown to drain the ticker goroutine.
func NewSamplerRegistry(interval time.Duration, log *slog.Logger) *SamplerRegistry {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &SamplerRegistry{
		samplers: make(map[containerKey]registeredSampler),
		interval: interval,
		log:      log,
	}
}

// Start launches the ticker goroutine. Safe to call once.
func (r *SamplerRegistry) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	go r.loop(ctx)
}

// Stop signals the ticker goroutine and waits for it to drain.
func (r *SamplerRegistry) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
}

// Register adds a sampler for (id, repo, runtime). Replaces any prior
// sampler at the same key.
func (r *SamplerRegistry) Register(id, repo, runtimeLabel string, s Sampler) {
	r.mu.Lock()
	r.samplers[containerKey{id, repo, runtimeLabel}] = registeredSampler{sampler: s}
	r.mu.Unlock()
}

// Unregister removes the sampler and deletes the associated metric series.
func (r *SamplerRegistry) Unregister(id, repo, runtimeLabel string) {
	r.mu.Lock()
	delete(r.samplers, containerKey{id, repo, runtimeLabel})
	r.mu.Unlock()
	DeleteContainerSeries(id, repo, runtimeLabel)
}

func (r *SamplerRegistry) loop(ctx context.Context) {
	defer close(r.done)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *SamplerRegistry) tick(ctx context.Context) {
	r.mu.Lock()
	// Snapshot so the sampler calls don't hold the lock — a slow HCS
	// query shouldn't block Register/Unregister.
	type snap struct {
		key     containerKey
		sampler Sampler
	}
	snaps := make([]snap, 0, len(r.samplers))
	for k, rs := range r.samplers {
		snaps = append(snaps, snap{k, rs.sampler})
	}
	r.mu.Unlock()

	for _, s := range snaps {
		stats, err := s.sampler.Sample(ctx)
		if err != nil {
			r.log.Debug("container stats sample failed", "id", s.key.id, "repo", s.key.repo, "runtime", s.key.runtimeLabel, "error", err)
			continue
		}
		RecordContainerStats(s.key.id, s.key.repo, s.key.runtimeLabel, stats)
	}
}
