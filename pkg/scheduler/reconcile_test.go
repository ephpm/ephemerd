package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// catcherProvider is a mockProvider that also implements CatchUpPoll and counts calls.
type catcherProvider struct {
	*mockProvider
	calls atomic.Int32
}

func (c *catcherProvider) CatchUpPoll(_ context.Context) error {
	c.calls.Add(1)
	return nil
}

// TestRunReconcileLoop_PeriodicallyCatchesUp verifies the webhook-mode safety
// net actually fires CatchUpPoll on its interval and stops on ctx cancel.
func TestRunReconcileLoop_PeriodicallyCatchesUp(t *testing.T) {
	cp := &catcherProvider{mockProvider: newMockProvider("github")}
	s := New(Config{
		Providers:         []providers.Provider{cp},
		Log:               testLogger(),
		ReconcileInterval: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runReconcileLoop(ctx); close(done) }()
	time.Sleep(130 * time.Millisecond) // ~6 ticks
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runReconcileLoop did not return after ctx cancel")
	}
	if n := cp.calls.Load(); n < 2 {
		t.Errorf("CatchUpPoll called %d times, want >= 2 (periodic sweep)", n)
	}
}

// TestRunReconcileLoop_NoCatcher_Returns: a provider without CatchUpPoll makes
// the loop a no-op that returns immediately (no ticker started).
func TestRunReconcileLoop_NoCatcher_Returns(t *testing.T) {
	s := New(Config{
		Providers:         []providers.Provider{newMockProvider("github")},
		Log:               testLogger(),
		ReconcileInterval: 10 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() { s.runReconcileLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runReconcileLoop should return immediately with no CatchUpPoll-capable provider")
	}
}
