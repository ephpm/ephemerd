package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/vm"
)

// macCloneSweepInterval is how often the stale macOS clone sweep runs after
// its startup pass. A leaked clone costs what its job wrote — tens of GB for
// a heavy build — so a day of leaks must never pile up, but the pass is a
// single directory read when there is nothing to do.
const macCloneSweepInterval = 30 * time.Minute

// macCloneSweeper owns the periodic sweep of <data>/vm/macos/jobs (see
// pkg/vm/macos_clones.go for why clones leaked and the layers that now stop
// it) and its running totals for status.
type macCloneSweeper struct {
	once sync.Once
	kick chan struct{}

	mu         sync.Mutex
	removed    int
	freedBytes int64

	// sweep overrides vm.SweepStaleMacOSClones; only tests set it.
	sweep func(dataDir string, live map[string]bool, grace time.Duration, now time.Time) vm.MacOSCloneSweep
}

func (m *macCloneSweeper) ch() chan struct{} {
	m.once.Do(func() { m.kick = make(chan struct{}, 1) })
	return m.kick
}

// kickNow requests a pass without waiting for the next tick. Non-blocking: a
// pass already pending covers this request.
func (m *macCloneSweeper) kickNow() {
	select {
	case m.ch() <- struct{}{}:
	default:
	}
}

// totals reports how many clones the sweep has removed since start, and the
// disk that gave back.
func (m *macCloneSweeper) totals() (int, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removed, m.freedBytes
}

// runMacCloneSweeper sweeps on every kick (SetMacOSVMConfig sends the first
// one, which is the startup pass) and every macCloneSweepInterval. A tick
// before macOS support is configured does nothing.
func (s *Scheduler) runMacCloneSweeper(ctx context.Context) {
	ticker := time.NewTicker(macCloneSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.macClones.ch():
		}
		s.sweepMacClones(time.Now())
	}
}

// sweepMacClones runs one pass. Protected from it: every VM this process
// still owns files for (vm.LiveMacOSVMIDs — provisioning, running, or in a
// wedged stop, which s.running does not cover) and anything written within
// vm.MacOSCloneGrace.
func (s *Scheduler) sweepMacClones(now time.Time) {
	s.mu.Lock()
	cfg := s.cfg.MacOSVMConfig
	s.mu.Unlock()
	if cfg == nil || cfg.DataDir == "" {
		return
	}
	sweep := s.macClones.sweep
	if sweep == nil {
		log := s.cfg.Log.With("component", "macos-clone-sweeper")
		sweep = func(dataDir string, live map[string]bool, grace time.Duration, now time.Time) vm.MacOSCloneSweep {
			return vm.SweepStaleMacOSClones(dataDir, live, grace, now, log)
		}
	}
	res := sweep(cfg.DataDir, vm.LiveMacOSVMIDs(), vm.MacOSCloneGrace, now)
	if res.Removed == 0 {
		return
	}
	s.macClones.mu.Lock()
	s.macClones.removed += res.Removed
	s.macClones.freedBytes += res.FreedBytes
	s.macClones.mu.Unlock()
	s.cfg.Log.Info("swept stale macOS VM clones", "component", "macos-clone-sweeper",
		"removed", res.Removed, "freed_bytes", res.FreedBytes, "failed", len(res.Failed))
}
