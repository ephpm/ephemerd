package scheduler

// Tests for dropping STALE dispatch decisions.
//
// On 2026-10-05 the Windows node spent ~34 of its 48 slot-hours a day booting
// runners for jobs GitHub had cancelled up to three days earlier. Three things
// combined:
//
//   - admitDispatch's satisfied check reads the in-memory started map, which
//     forgets a finished job after ~JobTimeout, while a slot wait had no bound
//     at all (dispatches were 24h and 28h into their wait);
//   - reprovisionIfStranded re-queued any job not in started, so each idle
//     runner's exit put the dead job straight back in line;
//   - cleanSeen reset the zombie counter of jobs still pending or running, so
//     maxProvisionAttempts never fired.
//
// The platform is now asked before a stale decision becomes a runner, waits
// are bounded, and the counter survives while the node still holds the job.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// jobStateProvider answers JobAwaitingRunner from a test-controlled value.
type jobStateProvider struct {
	*reportingProvider
	awaiting atomic.Bool
	err      error
	calls    atomic.Int32
}

func (p *jobStateProvider) JobAwaitingRunner(context.Context, *providers.JobEvent) (bool, error) {
	p.calls.Add(1)
	if p.err != nil {
		return false, p.err
	}
	return p.awaiting.Load(), nil
}

var _ providers.JobStateReporter = (*jobStateProvider)(nil)

func newJobStateProvider(awaiting bool, err error) (*jobStateProvider, *claimCountingProvider) {
	base := newClaimCountingProvider("github")
	p := &jobStateProvider{reportingProvider: &reportingProvider{claimCountingProvider: base}, err: err}
	p.awaiting.Store(awaiting)
	return p, base
}

// waitForStarted blocks until started contains key (or the deadline elapses).
func waitForStarted(t *testing.T, s *Scheduler, key jobKey) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, ok := s.started[key]
		s.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAdmitDispatch_AsksPlatformAfterWaiting is the production shape: a job
// is blocked behind a full pool, finishes at GitHub while it waits, and this
// node holds no record of that (the completed webhook was missed, or started
// already forgot it). When the slot frees, the dispatch must ask and drop it.
func TestAdmitDispatch_AsksPlatformAfterWaiting(t *testing.T) {
	tests := []struct {
		name        string
		webhookMode bool
		awaiting    bool
		apiErr      error
		wantClaims  int32
		wantRunning bool
		wantStarted bool
	}{
		{
			name:        "job finished at the platform while we waited",
			webhookMode: true, awaiting: false,
			wantClaims: 0, wantRunning: false, wantStarted: true,
		},
		{
			name:        "job still queued when the slot frees",
			webhookMode: true, awaiting: true,
			wantClaims: 1, wantRunning: true,
		},
		{
			// Fail open: an API outage must never strand a live job.
			name:        "platform cannot answer",
			webhookMode: true, apiErr: errors.New("api down"),
			wantClaims: 1, wantRunning: true,
		},
		{
			// Unlike the started map, the platform's answer is valid in poll
			// mode too.
			name:        "poll mode also drops a finished job",
			webhookMode: false, awaiting: false,
			wantClaims: 0, wantRunning: false, wantStarted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
			defer close(fake.waitBlock)
			_, dc, stopDispatch := startFakeDispatchServer(t, fake)
			defer stopDispatch()

			prov, base := newJobStateProvider(tt.awaiting, tt.apiErr)
			s := New(Config{
				Providers:       []providers.Provider{prov},
				LinuxDispatcher: dc,
				MaxConcurrent:   1,
				JobTimeout:      30 * time.Second,
				Log:             quietLogger(),
			})
			s.webhookMode = tt.webhookMode

			const jobID int64 = 910
			key := jobKey{Provider: prov.Name(), JobID: jobID}

			s.linuxSem <- struct{}{} // the pool is full
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobID))
			}()
			if !waitForPending(t, s, key) {
				t.Fatal("handler never reached the semaphore wait")
			}
			// pending is set just BEFORE acquireSlot. Freeing the slot that
			// instant can let the handler take the fast path, which correctly
			// skips the platform check — so give it time to actually block.
			time.Sleep(50 * time.Millisecond)
			<-s.linuxSem
			<-done

			if got := prov.calls.Load(); got != 1 {
				t.Errorf("platform consulted %d time(s), want 1", got)
			}
			if got := base.claims.Load(); got != tt.wantClaims {
				t.Errorf("claims = %d, want %d", got, tt.wantClaims)
			}
			s.mu.Lock()
			_, running := s.running[key]
			_, pending := s.pending[key]
			_, started := s.started[key]
			s.mu.Unlock()
			if running != tt.wantRunning {
				t.Errorf("running = %v, want %v", running, tt.wantRunning)
			}
			if pending {
				t.Error("pending was not cleared")
			}
			if started != tt.wantStarted {
				t.Errorf("started = %v, want %v (a gone verdict is recorded so later gates need not ask again)", started, tt.wantStarted)
			}
			if !tt.wantRunning && len(s.linuxSem) != 0 {
				t.Errorf("abandoned dispatch leaked its slot: linuxSem holds %d", len(s.linuxSem))
			}
		})
	}
}

// A dispatch that found a free slot at once is acting on a webhook seconds
// old. It must not spend an API call per job asking about it.
func TestAdmitDispatch_FastPathDoesNotAsk(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	defer close(fake.waitBlock)
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov, base := newJobStateProvider(false, nil) // would say "gone" if asked
	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   1,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	s.handleQueued(context.Background(), linuxQueuedEvent(prov, 920))

	if got := prov.calls.Load(); got != 0 {
		t.Errorf("platform consulted %d time(s) on the fast path, want 0", got)
	}
	if got := base.claims.Load(); got != 1 {
		t.Errorf("claims = %d, want 1", got)
	}
}

// TestReprovisionIfStranded_AsksPlatformFirst: a runner exits unassigned and
// the node never saw its job run. Re-queue only if the platform agrees the job
// still needs a runner.
func TestReprovisionIfStranded_AsksPlatformFirst(t *testing.T) {
	tests := []struct {
		name       string
		awaiting   bool
		apiErr     error
		wantClaims int32
	}{
		{name: "job is gone", awaiting: false, wantClaims: 1},
		{name: "job still queued", awaiting: true, wantClaims: 2},
		{name: "platform cannot answer", apiErr: errors.New("api down"), wantClaims: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
			defer close(fake.waitBlock)
			_, dc, stopDispatch := startFakeDispatchServer(t, fake)
			defer stopDispatch()

			prov, base := newJobStateProvider(true, nil)
			s := New(Config{
				Providers:       []providers.Provider{prov},
				LinuxDispatcher: dc,
				MaxConcurrent:   8,
				JobTimeout:      30 * time.Second,
				Log:             quietLogger(),
			})
			s.webhookMode = true

			const jobID int64 = 930
			key := jobKey{Provider: prov.Name(), JobID: jobID}
			event := linuxQueuedEvent(prov, jobID)

			s.handleQueued(context.Background(), event)
			if got := base.claims.Load(); got != 1 {
				t.Fatalf("first dispatch: claims = %d, want 1", got)
			}

			// The runner exits without the job ever being observed; by now the
			// platform has its own view of the job.
			s.mu.Lock()
			s.untrackRunningLocked(key, s.running[key])
			s.mu.Unlock()
			prov.awaiting.Store(tt.awaiting)
			prov.err = tt.apiErr

			s.reprovisionIfStranded(context.Background(), event)

			if tt.wantClaims == 1 {
				if !waitForStarted(t, s, key) {
					t.Fatal("gone verdict was never recorded")
				}
				time.Sleep(100 * time.Millisecond) // let any (bug) re-dispatch land
			} else {
				waitForClaims(t, base, tt.wantClaims)
			}
			if got := base.claims.Load(); got != tt.wantClaims {
				t.Errorf("claims = %d, want %d", got, tt.wantClaims)
			}
		})
	}
}

// TestStrandLoop_StopsWhenPlatformSaysGone drives the real wait-goroutine:
// every runner exits at once, unassigned, exactly like the idle runners on the
// Windows node. Before the fix this looped to the zombie cap (and in
// production past it, see cleanSeen). With the platform reporting the job
// finished, the first exit must end it.
func TestStrandLoop_StopsWhenPlatformSaysGone(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	close(fake.waitBlock) // Wait returns immediately: the runner exits unassigned
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov, base := newJobStateProvider(false, nil)
	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   8,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobID int64 = 940
	key := jobKey{Provider: prov.Name(), JobID: jobID}
	s.handleQueued(context.Background(), linuxQueuedEvent(prov, jobID))

	if !waitForStarted(t, s, key) {
		t.Fatal("the strand loop never consulted the platform")
	}
	time.Sleep(150 * time.Millisecond)
	if got := base.claims.Load(); got != 1 {
		t.Errorf("claims = %d, want 1: a job the platform reports finished was re-provisioned", got)
	}
}

// TestAcquireSlot_GivesUpAfterMaxWait: a wait has an end. The dispatch is
// dropped cleanly — no claim, no leaked slot, and its dedup cleared so the job
// is dispatched afresh if it turns out to be still queued.
func TestAcquireSlot_GivesUpAfterMaxWait(t *testing.T) {
	old := maxSlotWait
	maxSlotWait = 100 * time.Millisecond
	t.Cleanup(func() { maxSlotWait = old })

	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	defer close(fake.waitBlock)
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov, base := newJobStateProvider(true, nil)
	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   1,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})
	s.webhookMode = true

	const jobID int64 = 950
	key := jobKey{Provider: prov.Name(), JobID: jobID}
	event := linuxQueuedEvent(prov, jobID)

	s.linuxSem <- struct{}{} // full, and nothing will free it
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleQueued(context.Background(), event)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch was still blocked long after maxSlotWait")
	}

	if got := base.claims.Load(); got != 0 {
		t.Errorf("claims = %d, want 0", got)
	}
	if len(s.linuxSem) != 1 {
		t.Errorf("linuxSem holds %d, want 1 (only the occupier): the abandoned wait took or freed a slot", len(s.linuxSem))
	}
	s.mu.Lock()
	_, pending := s.pending[key]
	_, seen := s.seen[key]
	s.mu.Unlock()
	if pending || seen {
		t.Errorf("pending=%v seen=%v after give-up, want both cleared so the job can be re-dispatched", pending, seen)
	}

	// Still queued and now there is room: the next event dispatches it.
	<-s.linuxSem
	s.handleQueued(context.Background(), event)
	if got := base.claims.Load(); got != 1 {
		t.Errorf("re-dispatch after give-up: claims = %d, want 1", got)
	}
}

// TestCleanSeen_KeepsAttemptsForHeldJobs: the zombie counter survives a seen
// expiry while the node still holds the job. Resetting it there is what let
// strand cycles (each longer than seenTTL) re-provision forever.
func TestCleanSeen_KeepsAttemptsForHeldJobs(t *testing.T) {
	s := New(Config{Log: quietLogger()})
	pendingKey := jobKey{Provider: "github", JobID: 1}
	runningKey := jobKey{Provider: "github", JobID: 2}
	goneKey := jobKey{Provider: "github", JobID: 3}
	expired := time.Now().Add(-2 * seenTTL)

	s.mu.Lock()
	for _, k := range []jobKey{pendingKey, runningKey, goneKey} {
		s.seen[k] = expired
		s.attempts[k] = 3
		s.jobLabels[k] = "linux,self-hosted"
	}
	s.pending[pendingKey] = struct{}{}
	s.running[runningKey] = &runningJob{}
	s.mu.Unlock()

	s.cleanSeen()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range []jobKey{pendingKey, runningKey} {
		if s.attempts[k] != 3 {
			t.Errorf("job %d: attempts = %d, want 3 — reset while the node still holds the job", k.JobID, s.attempts[k])
		}
		if _, ok := s.jobLabels[k]; !ok {
			t.Errorf("job %d: labels dropped while the job is still demand", k.JobID)
		}
	}
	if _, ok := s.attempts[goneKey]; ok {
		t.Error("attempts kept for a job that left the queue; a later rerun would start over the cap")
	}
	if _, ok := s.seen[pendingKey]; ok {
		t.Error("expired seen stamp was not pruned")
	}
}
