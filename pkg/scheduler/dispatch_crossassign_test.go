package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	ghclient "github.com/ephpm/ephemerd/pkg/github"
	gh "github.com/google/go-github/v72/github"
)

// stubDispatcher records Create/Destroy calls so tests can assert which
// dispatched runner the scheduler targets.
type stubDispatcher struct {
	mu             sync.Mutex
	creates        []string
	destroys       []string
	waitBlock      chan struct{} // if non-nil, Wait blocks on this until closed
}

func (s *stubDispatcher) Create(ctx context.Context, id, image, jitConfig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, id)
	return nil
}

func (s *stubDispatcher) Wait(ctx context.Context, id string) (uint32, error) {
	if s.waitBlock != nil {
		select {
		case <-s.waitBlock:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, nil
}

func (s *stubDispatcher) Destroy(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroys = append(s.destroys, id)
	return nil
}

func (s *stubDispatcher) destroyList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.destroys))
	copy(out, s.destroys)
	return out
}

// TestHandleCompleted_DoesNotEagerDestroyCrossAssignedRunner reproduces the
// scheduler bug where a job-completed webhook destroys the runner we MAPPED
// to that job_id — even though GitHub may have given that runner a different
// job. Repro steps from the production incident (see ephemerd.log 10:36:35
// and /c/ProgramData/ephemerd/vm/linux/console.log for the matching 17:36):
//
//  1. Two Linux jobs J1 and J2 queue at the same moment with identical labels.
//  2. ephemerd dispatches two JIT runners R1 and R2, mapping R1→J1 and R2→J2.
//  3. GitHub assigns jobs by label-match, NOT by our mapping, so R1 may end
//     up running J2 and R2 may end up running J1.
//  4. J2 (assigned to R1 in reality) fails fast — webhook fires "completed"
//     for J2. The scheduler looks up s.running[J2] = {dispatched: R2} and
//     calls LinuxDispatcher.Destroy(R2) — killing R2 while R2 is happily
//     running J1 in the background.
//
// Fix direction: do NOT eagerly destroy on webhook completion for dispatched
// runners. JIT actions-runner is single-shot — it exits cleanly after running
// whatever job GitHub handed it, and the Wait() goroutine destroys the
// container naturally. Webhook completed events are advisory (metrics only).
func TestHandleCompleted_DoesNotEagerDestroyCrossAssignedRunner(t *testing.T) {
	stub := &stubDispatcher{}

	s := &Scheduler{
		cfg: Config{
			LinuxDispatcher: stub,
			Log:             testLogger(),
		},
		running: make(map[int64]*runningJob),
		sem:     make(chan struct{}, 4),
		seen:    make(map[int64]time.Time),
	}

	// Simulate state after two jobs were dispatched: our internal mapping.
	// In production this was set by handleLinuxJob right before the go-func
	// that calls LinuxDispatcher.Wait.
	_, cancel1 := context.WithCancel(context.Background())
	_, cancel2 := context.WithCancel(context.Background())
	s.running[100] = &runningJob{
		repo:       "owner/repo",
		dispatched: "runner-A",
		cancel:     cancel1,
		startedAt:  time.Now(),
	}
	s.running[200] = &runningJob{
		repo:       "owner/repo",
		dispatched: "runner-B",
		cancel:     cancel2,
		startedAt:  time.Now(),
	}

	// Fire "completed" for job 100. Current buggy code: destroys runner-A
	// because s.running[100].dispatched == "runner-A". But GitHub might have
	// given runner-A a different job (200), and that job is still going.
	event := ghclient.JobEvent{
		Repo: "owner/repo",
		Job: &gh.WorkflowJob{
			ID:         gh.Ptr(int64(100)),
			Conclusion: gh.Ptr("failure"),
		},
	}
	s.handleCompleted(context.Background(), event)

	// CORRECT behavior: no eager Destroy call. The dispatched runner exits
	// naturally via JIT one-shot semantics, and Wait() cleans up. Destroying
	// eagerly based on our (potentially stale) mapping is unsafe.
	destroys := stub.destroyList()
	if len(destroys) != 0 {
		t.Errorf("handleCompleted made %d eager Destroy calls %v; want 0 — "+
			"destroying the mapped runner is unsafe because GitHub may have "+
			"assigned it a different job", len(destroys), destroys)
	}

	// Job must still be removed from running map so metrics/drain work.
	s.mu.Lock()
	_, stillThere := s.running[100]
	s.mu.Unlock()
	if stillThere {
		t.Errorf("s.running[100] still present after handleCompleted; want removed")
	}
}

// TestHandleCompleted_RemapsByRunnerName verifies that when the webhook
// includes runner_name, the scheduler rebinds s.running so metrics and
// subsequent completion events attribute to the correct runner. This is the
// "fix (b)" behavior — accurate runner↔job binding once GitHub tells us
// which runner it actually picked.
func TestHandleCompleted_RemapsByRunnerName(t *testing.T) {
	stub := &stubDispatcher{}

	s := &Scheduler{
		cfg: Config{
			LinuxDispatcher: stub,
			Log:             testLogger(),
		},
		running: make(map[int64]*runningJob),
		sem:     make(chan struct{}, 4),
		seen:    make(map[int64]time.Time),
	}

	// Our mapping at dispatch time: J1→R1, J2→R2.
	_, cancel1 := context.WithCancel(context.Background())
	_, cancel2 := context.WithCancel(context.Background())
	s.running[100] = &runningJob{
		repo:       "owner/repo",
		dispatched: "runner-A",
		cancel:     cancel1,
		startedAt:  time.Now(),
	}
	s.running[200] = &runningJob{
		repo:       "owner/repo",
		dispatched: "runner-B",
		cancel:     cancel2,
		startedAt:  time.Now(),
	}

	// GitHub actually assigned runner-A to J2 (not J1 as we mapped). The
	// completed webhook for J2 arrives WITH runner_name="runner-A" so we
	// know the real binding.
	event := ghclient.JobEvent{
		Repo: "owner/repo",
		Job: &gh.WorkflowJob{
			ID:         gh.Ptr(int64(200)),
			RunnerName: gh.Ptr("runner-A"),
			Conclusion: gh.Ptr("success"),
		},
	}
	s.handleCompleted(context.Background(), event)

	// The scheduler should now have rebound: J2 completed on runner-A. We
	// remove J2 from tracking. Whatever job runner-B is actually running
	// (logically J1 now since GitHub cross-assigned) stays tracked.
	s.mu.Lock()
	_, stillJ2 := s.running[200]
	_, stillJ1 := s.running[100]
	s.mu.Unlock()

	if stillJ2 {
		t.Errorf("s.running[200] present after completed webhook; want removed")
	}
	if !stillJ1 {
		t.Errorf("s.running[100] removed unexpectedly; only the completed job should be removed")
	}

	// And still no eager Destroy of either runner — the mapping rebind is
	// for bookkeeping, not for container destruction.
	if destroys := stub.destroyList(); len(destroys) != 0 {
		t.Errorf("unexpected Destroy calls %v", destroys)
	}
}
