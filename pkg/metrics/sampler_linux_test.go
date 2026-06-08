//go:build linux

package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/api/types"
	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/typeurl/v2"
	anypb "google.golang.org/protobuf/types/known/anypb"
)

type fakeTaskMetrics struct {
	stats *cgroupsv2.Metrics
	err   error
}

func (f *fakeTaskMetrics) Metrics(_ context.Context) (*types.Metric, error) {
	if f.err != nil {
		return nil, f.err
	}
	a, err := typeurl.MarshalAny(f.stats)
	if err != nil {
		return nil, err
	}
	return &types.Metric{Data: &anypb.Any{TypeUrl: a.GetTypeUrl(), Value: a.GetValue()}}, nil
}

func TestLinuxSampler_MapsFields(t *testing.T) {
	fake := &fakeTaskMetrics{stats: &cgroupsv2.Metrics{
		CPU:    &cgroupsv2.CPUStat{UsageUsec: 1_500_000},   // 1.5 s
		Memory: &cgroupsv2.MemoryStat{Usage: 2048, Anon: 1024, UsageLimit: 8192},
	}}
	s := NewLinuxSampler(fake, 4, 0)

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got, want := out.CPUUsageNanos, uint64(1_500_000_000); got != want {
		t.Errorf("CPUUsageNanos = %d, want %d", got, want)
	}
	if got, want := out.MemoryBytes, uint64(2048); got != want {
		t.Errorf("MemoryBytes = %d, want %d", got, want)
	}
	if got, want := out.MemoryAnonBytes, uint64(1024); got != want {
		t.Errorf("MemoryAnonBytes = %d, want %d", got, want)
	}
	if got, want := out.CPULimit, uint64(4); got != want {
		t.Errorf("CPULimit = %d, want %d", got, want)
	}
	// Caller passed 0; sampler should surface the kernel-reported cap.
	if got, want := out.MemoryLimitBytes, uint64(8192); got != want {
		t.Errorf("MemoryLimitBytes = %d, want %d", got, want)
	}
}

func TestLinuxSampler_ExplicitMemoryLimitOverridesKernelMax(t *testing.T) {
	fake := &fakeTaskMetrics{stats: &cgroupsv2.Metrics{
		Memory: &cgroupsv2.MemoryStat{Usage: 1024, UsageLimit: 4096},
	}}
	s := NewLinuxSampler(fake, 0, 2048) // caller-supplied wins

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got, want := out.MemoryLimitBytes, uint64(2048); got != want {
		t.Errorf("MemoryLimitBytes = %d, want %d (caller-supplied should win)", got, want)
	}
}

func TestLinuxSampler_ErrorPropagates(t *testing.T) {
	want := errors.New("metrics rpc failed")
	s := NewLinuxSampler(&fakeTaskMetrics{err: want}, 0, 0)
	if _, err := s.Sample(context.Background()); !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestLinuxSampler_NilReader(t *testing.T) {
	s := &LinuxSampler{}
	if _, err := s.Sample(context.Background()); err == nil {
		t.Fatal("expected error from nil reader")
	}
}

type fakeNetReader struct {
	rx, tx uint64
	err    error
	called int
}

func (f *fakeNetReader) ReadNetwork(_ string) (uint64, uint64, error) {
	f.called++
	if f.err != nil {
		return 0, 0, f.err
	}
	return f.rx, f.tx, nil
}

func TestLinuxSampler_NetworkMapsBytes(t *testing.T) {
	fakeT := &fakeTaskMetrics{stats: &cgroupsv2.Metrics{
		Memory: &cgroupsv2.MemoryStat{Usage: 1024},
	}}
	fakeN := &fakeNetReader{rx: 99_000, tx: 12_345}
	s := NewLinuxSampler(fakeT, 0, 0).WithNetwork("/var/run/netns/test-ns").withNetworkReader(fakeN)

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got, want := out.NetworkRxBytes, uint64(99_000); got != want {
		t.Errorf("NetworkRxBytes = %d, want %d", got, want)
	}
	if got, want := out.NetworkTxBytes, uint64(12_345); got != want {
		t.Errorf("NetworkTxBytes = %d, want %d", got, want)
	}
	if fakeN.called != 1 {
		t.Errorf("network reader called %d times, want 1", fakeN.called)
	}
}

func TestLinuxSampler_NetworkDisabledWhenNetnsEmpty(t *testing.T) {
	fakeT := &fakeTaskMetrics{stats: &cgroupsv2.Metrics{
		Memory: &cgroupsv2.MemoryStat{Usage: 1024},
	}}
	fakeN := &fakeNetReader{rx: 99, tx: 99}
	s := NewLinuxSampler(fakeT, 0, 0).withNetworkReader(fakeN) // no WithNetwork

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if out.NetworkRxBytes != 0 || out.NetworkTxBytes != 0 {
		t.Errorf("network bytes leaked through when netns path unset: rx=%d tx=%d", out.NetworkRxBytes, out.NetworkTxBytes)
	}
	if fakeN.called != 0 {
		t.Errorf("network reader called %d times despite empty netns path, want 0", fakeN.called)
	}
}

func TestLinuxSampler_NetworkErrorIsSwallowed(t *testing.T) {
	fakeT := &fakeTaskMetrics{stats: &cgroupsv2.Metrics{
		CPU: &cgroupsv2.CPUStat{UsageUsec: 500_000},
	}}
	fakeN := &fakeNetReader{err: errors.New("netns gone")}
	s := NewLinuxSampler(fakeT, 0, 0).WithNetwork("/proc/9999/ns/net").withNetworkReader(fakeN)

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample should not error when only the network read fails: %v", err)
	}
	if out.CPUUsageNanos != 500_000_000 {
		t.Errorf("CPU should still surface, got %d", out.CPUUsageNanos)
	}
	if out.NetworkRxBytes != 0 || out.NetworkTxBytes != 0 {
		t.Errorf("network bytes should be 0 on error, got rx=%d tx=%d", out.NetworkRxBytes, out.NetworkTxBytes)
	}
}
