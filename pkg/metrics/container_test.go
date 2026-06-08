package metrics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeSampler struct {
	mu      sync.Mutex
	calls   atomic.Int64
	next    ContainerStats
	nextErr error
}

func (f *fakeSampler) Sample(_ context.Context) (ContainerStats, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next, f.nextErr
}

func (f *fakeSampler) setNext(s ContainerStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = s
}

func TestRecordContainerStats_GaugesAndCounter(t *testing.T) {
	id, repo, rt := "test-job-1", "owner/repo", RuntimeLinuxNative
	t.Cleanup(func() { DeleteContainerSeries(id, repo, rt) })

	RecordContainerStats(id, repo, rt, ContainerStats{
		CPUUsageNanos:    0,
		MemoryBytes:      1024,
		MemoryLimitBytes: 4096,
		CPULimit:         2,
	})
	// First sample establishes baseline; counter must be 0.
	if got := gaugeValue(t, ContainerMemoryBytes, id, repo, rt); got != 1024 {
		t.Errorf("memory_bytes = %v, want 1024", got)
	}
	if got := counterValue(t, ContainerCPUSeconds, id, repo, rt); got != 0 {
		t.Errorf("cpu_usage_seconds_total = %v, want 0 on first sample", got)
	}

	// Second sample: 2 seconds of CPU consumed => counter += 2.
	RecordContainerStats(id, repo, rt, ContainerStats{
		CPUUsageNanos:    2_000_000_000,
		MemoryBytes:      2048,
		MemoryLimitBytes: 4096,
		CPULimit:         2,
	})
	if got := counterValue(t, ContainerCPUSeconds, id, repo, rt); got != 2 {
		t.Errorf("cpu_usage_seconds_total after second sample = %v, want 2", got)
	}
	if got := gaugeValue(t, ContainerMemoryBytes, id, repo, rt); got != 2048 {
		t.Errorf("memory_bytes after second sample = %v, want 2048", got)
	}

	// Counter reset (e.g. in-VM restart) — record nothing, re-baseline.
	RecordContainerStats(id, repo, rt, ContainerStats{CPUUsageNanos: 500_000_000})
	if got := counterValue(t, ContainerCPUSeconds, id, repo, rt); got != 2 {
		t.Errorf("counter must be flat on reset, got %v want 2", got)
	}
	// Next sample after reset accumulates from the new baseline.
	RecordContainerStats(id, repo, rt, ContainerStats{CPUUsageNanos: 1_500_000_000})
	if got := counterValue(t, ContainerCPUSeconds, id, repo, rt); got != 3 {
		t.Errorf("counter after post-reset sample = %v, want 3", got)
	}
}

func TestDeleteContainerSeries_RemovesAllSeries(t *testing.T) {
	id, repo, rt := "test-job-delete", "owner/repo", RuntimeLinuxNative
	RecordContainerStats(id, repo, rt, ContainerStats{MemoryBytes: 1, CPUUsageNanos: 1})
	DeleteContainerSeries(id, repo, rt)

	if hasSeries(t, "ephemerd_container_memory_bytes", id, repo, rt) {
		t.Error("memory_bytes series still present after delete")
	}
	if hasSeries(t, "ephemerd_container_cpu_usage_seconds_total", id, repo, rt) {
		t.Error("cpu_usage_seconds_total series still present after delete")
	}
}

func TestSamplerRegistry_TicksAndDeletes(t *testing.T) {
	id, repo, rt := "test-reg-1", "owner/repo", RuntimeLinuxNative
	t.Cleanup(func() { DeleteContainerSeries(id, repo, rt) })

	fake := &fakeSampler{}
	fake.setNext(ContainerStats{
		CPUUsageNanos:    5_000_000_000,
		MemoryBytes:      8192,
		MemoryLimitBytes: 16384,
		CPULimit:         4,
	})

	reg := NewSamplerRegistry(20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	defer reg.Stop()

	reg.Register(id, repo, rt, fake)

	// Wait until at least two samples have run so the counter delta path
	// executes (first sample baselines, second produces a delta).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.calls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fake.calls.Load() < 2 {
		t.Fatalf("sampler called %d times, want >=2", fake.calls.Load())
	}
	if got := gaugeValue(t, ContainerMemoryBytes, id, repo, rt); got != 8192 {
		t.Errorf("memory_bytes = %v, want 8192", got)
	}

	reg.Unregister(id, repo, rt)
	if hasSeries(t, "ephemerd_container_memory_bytes", id, repo, rt) {
		t.Error("series still present after Unregister")
	}
}

func TestSamplerRegistry_ErrorsAreSkipped(t *testing.T) {
	id, repo, rt := "test-reg-err", "owner/repo", RuntimeLinuxNative
	t.Cleanup(func() { DeleteContainerSeries(id, repo, rt) })

	fake := &fakeSampler{nextErr: errors.New("boom")}

	reg := NewSamplerRegistry(20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.Start(ctx)
	defer reg.Stop()

	reg.Register(id, repo, rt, fake)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if fake.calls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fake.calls.Load() < 2 {
		t.Fatalf("sampler called %d times, want >=2", fake.calls.Load())
	}
	// Erroring sampler must not produce any series.
	if hasSeries(t, "ephemerd_container_memory_bytes", id, repo, rt) {
		t.Error("series present despite sampler errors")
	}
}

func gaugeValue(t *testing.T, gv *prometheus.GaugeVec, id, repo, rt string) float64 {
	t.Helper()
	g, err := gv.GetMetricWith(prometheus.Labels{"id": id, "repo": repo, "runtime": rt})
	if err != nil {
		t.Fatalf("GetMetricWith: %v", err)
	}
	return testutil.ToFloat64(g)
}

func counterValue(t *testing.T, cv *prometheus.CounterVec, id, repo, rt string) float64 {
	t.Helper()
	c, err := cv.GetMetricWith(prometheus.Labels{"id": id, "repo": repo, "runtime": rt})
	if err != nil {
		t.Fatalf("GetMetricWith: %v", err)
	}
	return testutil.ToFloat64(c)
}

// hasSeries walks the default Gatherer for the named MetricFamily and
// reports whether a series exists matching the (id, repo, runtime) labels.
func hasSeries(t *testing.T, metricName, id, repo, rt string) bool {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["id"] == id && labels["repo"] == repo && labels["runtime"] == rt {
				return true
			}
		}
	}
	return false
}
