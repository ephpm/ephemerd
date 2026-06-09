//go:build windows

package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/Microsoft/hcsshim"
)

type fakeHCSStats struct {
	stats hcsshim.Statistics
	err   error
}

func (f *fakeHCSStats) Statistics() (hcsshim.Statistics, error) {
	if f.err != nil {
		return hcsshim.Statistics{}, f.err
	}
	return f.stats, nil
}

func TestWindowsSampler_MapsFields(t *testing.T) {
	fake := &fakeHCSStats{stats: hcsshim.Statistics{}}
	// HCS reports CPU in 100-ns units. 25,000,000 ticks = 2.5 s.
	fake.stats.Processor.TotalRuntime100ns = 25_000_000
	fake.stats.Memory.UsageCommitBytes = 1024 * 1024 // 1 MiB
	fake.stats.Network = []hcsshim.NetworkStats{
		{BytesReceived: 1_000_000, BytesSent: 250_000},
		{BytesReceived: 500_000, BytesSent: 100_000},
	}
	s := NewWindowsSampler(fake, 2, 4*1024*1024*1024)

	out, err := s.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got, want := out.CPUUsageNanos, uint64(2_500_000_000); got != want {
		t.Errorf("CPUUsageNanos = %d, want %d", got, want)
	}
	if got, want := out.MemoryBytes, uint64(1024*1024); got != want {
		t.Errorf("MemoryBytes = %d, want %d", got, want)
	}
	if got, want := out.MemoryAnonBytes, uint64(0); got != want {
		t.Errorf("MemoryAnonBytes = %d, want 0 (HCS has no anon split)", got)
	}
	if got, want := out.CPULimit, uint64(2); got != want {
		t.Errorf("CPULimit = %d, want %d", got, want)
	}
	if got, want := out.MemoryLimitBytes, uint64(4*1024*1024*1024); got != want {
		t.Errorf("MemoryLimitBytes = %d, want %d", got, want)
	}
	if got, want := out.NetworkRxBytes, uint64(1_500_000); got != want {
		t.Errorf("NetworkRxBytes = %d, want %d (summed across endpoints)", got, want)
	}
	if got, want := out.NetworkTxBytes, uint64(350_000); got != want {
		t.Errorf("NetworkTxBytes = %d, want %d (summed across endpoints)", got, want)
	}
}


func TestWindowsSampler_ErrorPropagates(t *testing.T) {
	want := errors.New("hcs failed")
	s := NewWindowsSampler(&fakeHCSStats{err: want}, 0, 0)
	if _, err := s.Sample(context.Background()); !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestWindowsSampler_NilReader(t *testing.T) {
	s := &WindowsSampler{}
	if _, err := s.Sample(context.Background()); err == nil {
		t.Fatal("expected error from nil reader")
	}
}
