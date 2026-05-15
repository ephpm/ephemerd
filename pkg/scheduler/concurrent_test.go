package scheduler

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/providers"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeDispatchServer is a configurable in-process Dispatch gRPC server used
// across scheduler tests. Each RPC bumps an atomic counter (for concurrent
// tests) and records the request (for dispatch round-trip tests).
type fakeDispatchServer struct {
	apiv1.UnimplementedDispatchServer

	createCalls  atomic.Int32
	waitCalls    atomic.Int32
	destroyCalls atomic.Int32

	// Recorded requests, protected by mu. Used by tests that assert on
	// request fields rather than just call counts.
	mu              sync.Mutex
	createRequests  []*apiv1.CreateJobRequest
	waitRequests    []*apiv1.WaitJobRequest
	destroyRequests []*apiv1.DestroyJobRequest

	// createErr, waitErr, destroyErr cause the corresponding RPC to fail
	// when set.
	createErr  error
	waitErr    error
	destroyErr error

	// waitExitCode is the exit code returned by WaitJob on success.
	waitExitCode uint32

	// waitBlock, when non-nil, causes WaitJob to block until the channel
	// is signalled. Used to keep dispatched jobs alive while the test
	// asserts intermediate state.
	waitBlock chan struct{}

	// destroyed receives the id of every destroyed job, in order.
	destroyed chan string
}

func (f *fakeDispatchServer) CreateJob(ctx context.Context, req *apiv1.CreateJobRequest) (*apiv1.CreateJobResponse, error) {
	f.createCalls.Add(1)
	f.mu.Lock()
	f.createRequests = append(f.createRequests, req)
	f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &apiv1.CreateJobResponse{}, nil
}

func (f *fakeDispatchServer) WaitJob(ctx context.Context, req *apiv1.WaitJobRequest) (*apiv1.WaitJobResponse, error) {
	f.waitCalls.Add(1)
	f.mu.Lock()
	f.waitRequests = append(f.waitRequests, req)
	f.mu.Unlock()
	if f.waitErr != nil {
		return &apiv1.WaitJobResponse{ExitCode: 1}, f.waitErr
	}
	if f.waitBlock != nil {
		select {
		case <-f.waitBlock:
		case <-ctx.Done():
			return &apiv1.WaitJobResponse{ExitCode: 137}, ctx.Err()
		}
	}
	return &apiv1.WaitJobResponse{ExitCode: f.waitExitCode}, nil
}

func (f *fakeDispatchServer) DestroyJob(ctx context.Context, req *apiv1.DestroyJobRequest) (*apiv1.DestroyJobResponse, error) {
	f.destroyCalls.Add(1)
	f.mu.Lock()
	f.destroyRequests = append(f.destroyRequests, req)
	f.mu.Unlock()
	if f.destroyed != nil {
		// Best-effort signal; never block the RPC if the test isn't reading.
		select {
		case f.destroyed <- req.Id:
		default:
		}
	}
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	return &apiv1.DestroyJobResponse{}, nil
}

// startFakeDispatchServer starts a gRPC server on a random port and returns
// the bound address, a connected DispatchClient, and a cleanup function.
// Cleanup is safe to call multiple times.
func startFakeDispatchServer(t *testing.T, fake *fakeDispatchServer) (string, *DispatchClient, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	apiv1.RegisterDispatchServer(srv, fake)
	go func() {
		if err := srv.Serve(lis); err != nil {
			// Serve returns nil on graceful stop, an error otherwise; both
			// are fine when the test is shutting down.
			t.Logf("fake dispatch serve: %v", err)
		}
	}()

	addr := lis.Addr().String()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		srv.GracefulStop()
		t.Fatalf("dial: %v", err)
	}

	dc := &DispatchClient{conn: conn, client: apiv1.NewDispatchClient(conn)}

	var stopOnce sync.Once
	cleanup := func() {
		stopOnce.Do(func() {
			if err := conn.Close(); err != nil {
				t.Logf("dispatch conn close: %v", err)
			}
			srv.GracefulStop()
		})
	}
	return addr, dc, cleanup
}

// claimCountingProvider wraps mockProvider with atomic counters for ClaimJob
// and ReleaseJob so tests can assert exactly-once behaviour under concurrency.
type claimCountingProvider struct {
	*mockProvider
	claims   atomic.Int32
	releases atomic.Int32

	// claimErr, when set, causes ClaimJob to fail without registering a claim.
	claimErr error
}

func newClaimCountingProvider(name string) *claimCountingProvider {
	return &claimCountingProvider{mockProvider: newMockProvider(name)}
}

func (p *claimCountingProvider) ClaimJob(ctx context.Context, event *providers.JobEvent, runnerName string, labels []string) (*providers.Claim, error) {
	p.claims.Add(1)
	if p.claimErr != nil {
		return nil, p.claimErr
	}
	return &providers.Claim{
		RunnerID:   event.JobID * 10,
		RunnerName: runnerName,
		Repo:       event.Repo,
	}, nil
}

func (p *claimCountingProvider) ReleaseJob(ctx context.Context, claim *providers.Claim) error {
	p.releases.Add(1)
	return nil
}

// --- Item #13: concurrent claim + dedup ---

// TestConcurrentHandleQueued_DedupsToSingleClaim simulates a webhook delivery
// arriving at the same time as a poll discovery for the same job. Many
// goroutines call handleQueued with the same job id; the dedup logic must
// guarantee that exactly ONE of them proceeds far enough to call ClaimJob.
//
// Without proper locking, two providers (or webhook + poll) could both
// register a runner with GitHub for the same job, leaving a ghost runner
// behind on whichever lost the race.
func TestConcurrentHandleQueued_DedupsToSingleClaim(t *testing.T) {
	fake := &fakeDispatchServer{
		// Block Wait so the dispatched job stays alive — we don't want
		// the cleanup goroutine to delete the running entry mid-test.
		waitBlock: make(chan struct{}),
	}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov := newClaimCountingProvider("test-provider")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   16,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const goroutines = 32
	const jobID int64 = 7777

	event := providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "concurrent-repo",
		JobID:    jobID,
		Labels:   []string{"self-hosted", "linux"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			s.handleQueued(ctx, event)
		}()
	}
	close(start)
	wg.Wait()

	// Wait briefly for the (single) handleLinuxJob goroutine to make its
	// dispatch.Create call. handleLinuxJob's path is:
	//   acquire sem -> claim -> dispatcher.Create -> register running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, running := s.running[jobKey{Provider: prov.Name(), JobID: jobID}]
		s.mu.Unlock()
		if running || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Exactly one ClaimJob call across all goroutines.
	if got := prov.claims.Load(); got != 1 {
		t.Errorf("ClaimJob called %d times, want exactly 1", got)
	}
	// Exactly one CreateJob dispatch.
	if got := fake.createCalls.Load(); got != 1 {
		t.Errorf("dispatch CreateJob called %d times, want exactly 1", got)
	}

	// Exactly one running job entry, under the correct composite key.
	s.mu.Lock()
	if got := len(s.running); got != 1 {
		t.Errorf("running map has %d entries, want 1", got)
	}
	if _, ok := s.running[jobKey{Provider: prov.Name(), JobID: jobID}]; !ok {
		t.Errorf("expected running entry under provider key")
	}
	s.mu.Unlock()

	// Cleanup: unblock Wait so the goroutine can exit.
	close(fake.waitBlock)
	cancel()
	// Give the wait goroutine a moment to clean up.
	for i := 0; i < 50; i++ {
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConcurrentHandleQueued_DifferentProvidersBothRun verifies the dedup
// cohabits with multi-provider routing: two providers emitting the same
// numeric job id must each get their own claim.
func TestConcurrentHandleQueued_DifferentProvidersBothRun(t *testing.T) {
	fake := &fakeDispatchServer{waitBlock: make(chan struct{})}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	provA := newClaimCountingProvider("provider-a")
	provB := newClaimCountingProvider("provider-b")
	defer func() {
		if err := provA.Stop(context.Background()); err != nil {
			t.Logf("provider-a stop: %v", err)
		}
		if err := provB.Stop(context.Background()); err != nil {
			t.Logf("provider-b stop: %v", err)
		}
	}()

	s := New(Config{
		Providers:       []providers.Provider{provA, provB},
		LinuxDispatcher: dc,
		MaxConcurrent:   8,
		JobTimeout:      30 * time.Second,
		Log:             quietLogger(),
	})

	const jobID int64 = 5555
	eventA := providers.JobEvent{
		Provider: provA, Action: "queued", Repo: "repo", JobID: jobID,
		Labels: []string{"self-hosted", "linux"},
	}
	eventB := providers.JobEvent{
		Provider: provB, Action: "queued", Repo: "repo", JobID: jobID,
		Labels: []string{"self-hosted", "linux"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			s.handleQueued(ctx, eventA)
		}()
		go func() {
			defer wg.Done()
			<-start
			s.handleQueued(ctx, eventB)
		}()
	}
	close(start)
	wg.Wait()

	// Wait for both running entries to materialize.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.running)
		s.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Each provider should claim exactly once.
	if got := provA.claims.Load(); got != 1 {
		t.Errorf("provider-a ClaimJob called %d times, want 1", got)
	}
	if got := provB.claims.Load(); got != 1 {
		t.Errorf("provider-b ClaimJob called %d times, want 1", got)
	}

	// Two CreateJob dispatches (one per provider).
	if got := fake.createCalls.Load(); got != 2 {
		t.Errorf("dispatch CreateJob called %d times, want 2", got)
	}

	close(fake.waitBlock)
	cancel()
}

// --- Item #14: timeout / cancellation during cleanup ---

// TestHandleCompleted_DestroyRunsAfterCancel asserts that handleCompleted
// runs the dispatch Destroy RPC even when the input context is already
// cancelled. The cleanup must use a fresh context.Background(), otherwise a
// shutting-down scheduler would leak containers.
func TestHandleCompleted_DestroyRunsAfterCancel(t *testing.T) {
	fake := &fakeDispatchServer{destroyed: make(chan string, 4)}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov := newClaimCountingProvider("dispatch-prov")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		Log:             quietLogger(),
	})

	const jobID int64 = 9001
	jobCtx, jobCancel := context.WithCancel(context.Background())
	key := jobKey{Provider: prov.Name(), JobID: jobID}
	s.running[key] = &runningJob{
		provider:   prov,
		claim:      &providers.Claim{RunnerID: 42, RunnerName: "ephemerd-dispatch-9001"},
		repo:       "repo",
		image:      "test-image",
		cancel:     jobCancel,
		dispatched: "ephemerd-dispatch-9001",
		startedAt:  time.Now(),
	}

	// Cancel the parent context BEFORE handleCompleted runs. The cleanup
	// path must still call Destroy.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	completed := providers.JobEvent{
		Provider:   prov,
		Action:     "completed",
		Repo:       "repo",
		JobID:      jobID,
		Conclusion: "cancelled",
	}

	s.handleCompleted(parentCtx, completed)

	// Destroy should have been called even though parentCtx was cancelled.
	select {
	case id := <-fake.destroyed:
		if id != "ephemerd-dispatch-9001" {
			t.Errorf("destroyed id = %q, want ephemerd-dispatch-9001", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch.Destroy was not invoked after cancellation")
	}

	if got := fake.destroyCalls.Load(); got != 1 {
		t.Errorf("dispatch.Destroy called %d times, want 1", got)
	}

	// The job's per-job cancel must have fired (handleCompleted always cancels).
	select {
	case <-jobCtx.Done():
	default:
		t.Error("expected per-job context to be cancelled by handleCompleted")
	}

	// The running entry must be gone.
	s.mu.Lock()
	_, exists := s.running[key]
	s.mu.Unlock()
	if exists {
		t.Error("running entry should be removed by handleCompleted")
	}
}

// TestDestroyAll_DestroysEvenWhenContextCancelled asserts that destroyAll
// (called during shutdown) destroys every dispatched container even though
// the parent context is cancelled at this point in the lifecycle. The Linux
// dispatcher path uses context.Background() specifically to survive shutdown.
func TestDestroyAll_DestroysEvenWhenContextCancelled(t *testing.T) {
	fake := &fakeDispatchServer{destroyed: make(chan string, 8)}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov := newClaimCountingProvider("shutdown-prov")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		Log:             quietLogger(),
	})

	// Two dispatched jobs in flight when shutdown hits.
	for i, name := range []string{"dispatched-1", "dispatched-2"} {
		_, cancel := context.WithCancel(context.Background())
		s.running[jobKey{Provider: prov.Name(), JobID: int64(100 + i)}] = &runningJob{
			provider:   prov,
			claim:      &providers.Claim{RunnerID: int64(100 + i), RunnerName: name},
			repo:       "repo",
			cancel:     cancel,
			dispatched: name,
			startedAt:  time.Now(),
		}
	}

	s.destroyAll()

	// Both destroys must have happened.
	deadline := time.After(2 * time.Second)
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case id := <-fake.destroyed:
			got[id] = true
		case <-deadline:
			t.Fatalf("only saw %d destroys (got=%v), want 2", len(got), got)
		}
	}

	if !got["dispatched-1"] || !got["dispatched-2"] {
		t.Errorf("missing destroy calls: %v", got)
	}

	// Both providers must have been ReleaseJob'd.
	if rel := prov.releases.Load(); rel != 2 {
		t.Errorf("ReleaseJob called %d times, want 2", rel)
	}

	// running map should be empty.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) != 0 {
		t.Errorf("running map has %d entries after destroyAll, want 0", len(s.running))
	}
}

// TestHandleLinuxJob_TimeoutTriggersDispatchDestroy exercises the
// JobTimeout path of handleLinuxJob: a long-running dispatched job whose
// jobCtx fires before it finishes. Wait returns an error (timeout), and the
// cleanup goroutine still calls dispatch.Destroy.
func TestHandleLinuxJob_TimeoutTriggersDispatchDestroy(t *testing.T) {
	fake := &fakeDispatchServer{
		destroyed: make(chan string, 1),
		waitErr:   errors.New("simulated timeout"),
	}
	_, dc, stopDispatch := startFakeDispatchServer(t, fake)
	defer stopDispatch()

	prov := newClaimCountingProvider("timeout-prov")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	s := New(Config{
		Providers:       []providers.Provider{prov},
		LinuxDispatcher: dc,
		MaxConcurrent:   2,
		JobTimeout:      50 * time.Millisecond,
		Log:             quietLogger(),
	})

	event := providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "repo",
		JobID:    424242,
		Labels:   []string{"self-hosted", "linux"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.handleQueued(ctx, event)

	// Wait for Destroy to fire.
	select {
	case <-fake.destroyed:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch.Destroy was not called after job wait error / timeout")
	}

	if got := fake.destroyCalls.Load(); got < 1 {
		t.Errorf("destroy calls = %d, want >= 1", got)
	}
}

// TestDestroyAll_StopsMacOSVMOnCancelled is a focused unit test for the
// macOS branch of destroyAll: Stop() must run on every macosVM regardless
// of context state, and the running map must be cleared.
func TestDestroyAll_StopsMacOSVMOnCancelled(t *testing.T) {
	prov := newClaimCountingProvider("mac-prov")
	defer func() {
		if err := prov.Stop(context.Background()); err != nil {
			t.Logf("provider stop: %v", err)
		}
	}()

	s := New(Config{
		Providers: []providers.Provider{prov},
		Log:       quietLogger(),
	})

	// stops counts every macosVM.Stop() invocation across all jobs.
	var stops atomic.Int32
	for i := 0; i < 3; i++ {
		_, cancel := context.WithCancel(context.Background())
		s.running[jobKey{Provider: prov.Name(), JobID: int64(i)}] = &runningJob{
			provider:  prov,
			claim:     &providers.Claim{RunnerID: int64(i)},
			repo:      "repo",
			cancel:    cancel,
			macosVM:   &stopRecordingMacVM{stopCount: &stops},
			startedAt: time.Now(),
		}
	}

	s.destroyAll()

	if got := stops.Load(); got != 3 {
		t.Errorf("macosVM.Stop called %d times, want 3", got)
	}

	// Provider releases.
	if rel := prov.releases.Load(); rel != 3 {
		t.Errorf("ReleaseJob called %d times, want 3", rel)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) != 0 {
		t.Errorf("running map has %d entries, want 0", len(s.running))
	}
}

// stopRecordingMacVM is a vm.MacOSVM that increments a counter on every
// Stop() call. Used to observe destroyAll's per-job Stop invocations.
type stopRecordingMacVM struct {
	stopCount *atomic.Int32
}

func (m *stopRecordingMacVM) WriteJITConfig(string) error     { return nil }
func (m *stopRecordingMacVM) Start(ctx context.Context) error { return nil }
func (m *stopRecordingMacVM) WaitForRunner(ctx context.Context) (string, error) {
	return "10.0.0.1", nil
}
func (m *stopRecordingMacVM) RunnerAddress() string                 { return "10.0.0.1" }
func (m *stopRecordingMacVM) Wait(ctx context.Context) (int, error) { return 0, nil }
func (m *stopRecordingMacVM) Stop()                                 { m.stopCount.Add(1) }
