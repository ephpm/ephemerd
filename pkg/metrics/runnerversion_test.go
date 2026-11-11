package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetRunnerVersionKeepsOneSeries(t *testing.T) {
	SetRunnerVersion("2.337.0", RunnerSourceEmbedded)
	SetRunnerVersion("2.338.2", RunnerSourceVerifiedCache)
	if n := testutil.CollectAndCount(RunnerVersionInfo); n != 1 {
		t.Fatalf("series count %d, want 1", n)
	}
	if v := testutil.ToFloat64(RunnerVersionInfo.WithLabelValues("2.338.2", RunnerSourceVerifiedCache)); v != 1 {
		t.Fatalf("active series = %v", v)
	}
}

func TestSetRunnerPinPublishedAt(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	SetRunnerPinPublishedAt(now.Add(-48*time.Hour), now)
	if v := testutil.ToFloat64(RunnerPinAgeSeconds); v != 48*3600 {
		t.Fatalf("age %v", v)
	}
	// Future or zero timestamps never go negative / never overwrite.
	SetRunnerPinPublishedAt(now.Add(time.Hour), now)
	if v := testutil.ToFloat64(RunnerPinAgeSeconds); v != 0 {
		t.Fatalf("future age %v", v)
	}
	SetRunnerPinPublishedAt(time.Time{}, now)
	if v := testutil.ToFloat64(RunnerPinAgeSeconds); v != 0 {
		t.Fatalf("zero time changed age to %v", v)
	}
}
