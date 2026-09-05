package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// wedgedMacVM is the macOS VM from issue #196 and the reason
// blockingMacVM (macos_provision_test.go) was not enough: its WaitForRunner
// never returns AT ALL, not even when the VM is stopped, and its Stop() never
// returns either.
//
// That is what the fleet's mac actually did. The daemon logged
//
//	ERROR macOS VM stuck in provisioning past deadline; force-stopping VM to reclaim the slot
//	WARN  macOS VM did not stop gracefully, forcing
//	INFO  macOS VM destroyed
//
// and then nothing for 28 hours: the guest wait never unwound, so the
// watchdog's `<-resCh` never returned, so the "reclaim the slot" it had just
// announced never happened. Every assertion below is about the dispatch path
// surviving a VM it cannot kill.
type wedgedMacVM struct {
	stopCalls atomic.Int32
	// stopReturns, when closed, lets Stop() return. Left open to model a
	// teardown wedged inside Virtualization.framework.
	stopReturns chan struct{}
}

// newWedgedMacVM registers cleanup that lets the wedged Stop() goroutines
// finish, so the test binary does not accumulate them across subtests.
func newWedgedMacVM(t *testing.T) *wedgedMacVM {
	t.Helper()
	m := &wedgedMacVM{stopReturns: make(chan struct{})}
	t.Cleanup(func() { close(m.stopReturns) })
	return m
}

func (m *wedgedMacVM) WriteJITConfig(string) error   { return nil }
func (m *wedgedMacVM) Start(_ context.Context) error { return nil }
func (m *wedgedMacVM) RunnerAddress() string         { return "" }
func (m *wedgedMacVM) Wait(context.Context) (int, error) {
	select {} //nolint:staticcheck // models a VM whose wait never returns
}

// WaitForRunner never returns — it ignores ctx exactly as a blocked
// golang.org/x/crypto/ssh session call does.
func (m *wedgedMacVM) WaitForRunner(context.Context) (string, error) {
	select {} //nolint:staticcheck // models the wedged guest wait
}

func (m *wedgedMacVM) Stop() {
	m.stopCalls.Add(1)
	<-m.stopReturns
}

// newSlotTestScheduler builds a scheduler with a single macOS slot and
// millisecond teardown bounds, so the 30-second production graces do not turn
// every assertion into a 30-second test.
func newSlotTestScheduler(t *testing.T, prov providers.Provider, mac vm.MacOSVM) *Scheduler {
	t.Helper()
	s := New(Config{
		Providers:             []providers.Provider{prov},
		MacOSVMConfig:         &vm.MacOSVMConfig{},
		MaxMacOSVMs:           1, // the incident's max_concurrent = 1
		MacOSProvisionTimeout: 50 * time.Millisecond,
		Log:                   quietLogger(),
	})
	s.macUnwindGrace = 50 * time.Millisecond
	s.macStopGrace = 50 * time.Millisecond
	s.newMacOSVM = func(vm.MacOSVMConfig, string) (vm.MacOSVM, error) { return mac, nil }
	return s
}

func macEvent(prov providers.Provider, jobID int64) providers.JobEvent {
	return providers.JobEvent{
		Provider: prov,
		Action:   "queued",
		Repo:     "repo",
		JobID:    jobID,
		Labels:   []string{"self-hosted", "macos"},
	}
}

// runMacDispatch runs handleMacOSJob and fails (rather than hanging) if it has
// not returned by deadline. A regression here must be a FAILURE, not a test
// that sits there until the package timeout.
func runMacDispatch(t *testing.T, s *Scheduler, event providers.JobEvent, deadline time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleMacOSJob(context.Background(), event)
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("handleMacOSJob did not return within %s: the dispatch path is blocked on VM teardown while holding a slot", deadline)
	}
}

// assertSlotFree asserts the pool has a slot available right now.
func assertSlotFree(t *testing.T, sem chan struct{}, what string) {
	t.Helper()
	select {
	case sem <- struct{}{}:
		<-sem
	default:
		t.Fatalf("%s: no free slot (held=%d capacity=%d)", what, len(sem), cap(sem))
	}
}

// TestHandleMacOSJob_UnkillableVMStillFreesSlot is the issue #196 regression
// test. The VM cannot be killed and its reachability wait never unwinds; the
// dispatch must still return and give the macOS slot back, because otherwise
// the node silently serves zero macOS jobs forever while reporting healthy.
//
// Without the fix this test FAILS on the deadline in runMacDispatch:
// waitForMacRunnerBounded parks on `<-resCh` after force-stopping the VM and
// handleMacOSJob never reaches its release.
func TestHandleMacOSJob_UnkillableVMStillFreesSlot(t *testing.T) {
	prov := newMockProvider("mac-wedged")
	defer func() { _ = prov.Stop(context.Background()) }()

	macVM := newWedgedMacVM(t)
	s := newSlotTestScheduler(t, prov, macVM)

	runMacDispatch(t, s, macEvent(prov, 100817345293), 5*time.Second)

	if macVM.stopCalls.Load() == 0 {
		t.Error("expected the wedged macOS VM to be force-stopped on the provisioning deadline (#178 behaviour must not regress)")
	}
	assertSlotFree(t, s.macSem, "after a provisioning deadline on an unkillable VM")
	if held := len(s.macSem); held != 0 {
		t.Errorf("macSem holds %d slots after the dispatch returned, want 0", held)
	}

	// The next macOS job must be able to run. This is the assertion the
	// production node failed for 28 hours.
	second := newWedgedMacVM(t)
	s.newMacOSVM = func(vm.MacOSVMConfig, string) (vm.MacOSVM, error) { return second, nil }
	runMacDispatch(t, s, macEvent(prov, 100817345294), 5*time.Second)
	if second.stopCalls.Load() == 0 {
		t.Error("the job queued behind the wedged one never reached provisioning: the slot was never actually reclaimed")
	}

	// Nothing may be left tracked, and the ghost JIT runners must be gone.
	if n := s.ActiveJobs(); n != 0 {
		t.Errorf("ActiveJobs() = %d after two abandoned provisions, want 0", n)
	}
	prov.mu.Lock()
	releases := len(prov.releases)
	prov.mu.Unlock()
	if releases != 2 {
		t.Errorf("ReleaseJob called %d times, want 2 (one ghost runner per abandoned provision)", releases)
	}
}

// TestHandleMacOSJob_ReleasesExactlyOncePerOutcome walks the distinct exits
// from the macOS dispatch and asserts each one leaves the pool exactly as it
// found it. "Exactly once" matters in both directions: a missed release
// starves the node, a double release hands out a slot somebody still holds.
func TestHandleMacOSJob_ReleasesExactlyOncePerOutcome(t *testing.T) {
	prov := newMockProvider("mac-outcomes")
	defer func() { _ = prov.Stop(context.Background()) }()

	t.Run("provisioning deadline", func(t *testing.T) {
		s := newSlotTestScheduler(t, prov, newWedgedMacVM(t))
		runMacDispatch(t, s, macEvent(prov, 1), 5*time.Second)
		if held := len(s.macSem); held != 0 {
			t.Fatalf("held=%d after the deadline path, want 0", held)
		}
		// Capacity must be intact, i.e. the slot came back once and not twice.
		assertCapacityIntact(t, s.macSem)
	})

	t.Run("vm create failure", func(t *testing.T) {
		s := newSlotTestScheduler(t, prov, nil)
		s.newMacOSVM = func(vm.MacOSVMConfig, string) (vm.MacOSVM, error) {
			return nil, errFakeCreate
		}
		runMacDispatch(t, s, macEvent(prov, 2), 5*time.Second)
		if held := len(s.macSem); held != 0 {
			t.Fatalf("held=%d after a VM create failure, want 0", held)
		}
		assertCapacityIntact(t, s.macSem)
	})

	t.Run("context cancelled before a slot is free", func(t *testing.T) {
		s := newSlotTestScheduler(t, prov, newWedgedMacVM(t))
		// Fill the sole slot so the dispatch has to wait for it.
		s.macSem <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.handleMacOSJob(ctx, macEvent(prov, 3))
		}()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handleMacOSJob did not return after its context was cancelled")
		}
		// It must not have taken a second slot, and must not have released
		// the one it never acquired.
		if held := len(s.macSem); held != 1 {
			t.Fatalf("held=%d after a cancelled dispatch, want 1 (the pre-existing holder)", held)
		}
		<-s.macSem
		assertCapacityIntact(t, s.macSem)
	})

	t.Run("successful job", func(t *testing.T) {
		var stops atomic.Int32
		s := newSlotTestScheduler(t, prov, &fastMacVM{ip: "192.168.64.5", stops: &stops})
		runMacDispatch(t, s, macEvent(prov, 4), 5*time.Second)
		// The wait-goroutine owns the slot now; fastMacVM.Wait returns
		// immediately, so it comes back promptly.
		waitForSlotFree(t, s.macSem, 5*time.Second)
		assertCapacityIntact(t, s.macSem)
		if n := s.ActiveJobs(); n != 0 {
			t.Errorf("ActiveJobs() = %d after the job finished, want 0", n)
		}
	})
}

var errFakeCreate = errors.New("vz: no macOS image installed")

// assertCapacityIntact proves the pool can still be filled to capacity and no
// further — i.e. releases and acquires balanced exactly.
func assertCapacityIntact(t *testing.T, sem chan struct{}) {
	t.Helper()
	n := 0
	for i := 0; i < cap(sem); i++ {
		select {
		case sem <- struct{}{}:
			n++
		default:
		}
	}
	if n != cap(sem) {
		t.Errorf("pool accepted %d of %d slots; a release was missed", n, cap(sem))
	}
	select {
	case sem <- struct{}{}:
		t.Error("pool accepted more than its capacity; a slot was released twice")
	default:
	}
	for i := 0; i < n; i++ {
		<-sem
	}
}

func waitForSlotFree(t *testing.T, sem chan struct{}, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(sem) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("slot was not released within %s (held=%d)", within, len(sem))
}

// TestSlotTokenReleaseIsIdempotent covers the property the whole fix leans on:
// the explicit release that lets the next job in, and the deferred backstop
// that catches a forgotten path, are the same release.
func TestSlotTokenReleaseIsIdempotent(t *testing.T) {
	sem := make(chan struct{}, 2)
	sem <- struct{}{}
	tok := &slotToken{sem: sem}

	tok.release()
	if len(sem) != 0 {
		t.Fatalf("held=%d after release, want 0", len(sem))
	}

	// A second holder takes the freed slot; further releases of the spent
	// token must not steal it.
	sem <- struct{}{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok.release()
		}()
	}
	wg.Wait()
	if len(sem) != 1 {
		t.Errorf("held=%d after repeated releases of a spent token, want 1", len(sem))
	}

	// A nil token is a no-op, so a path that never acquired can still defer.
	var nilTok *slotToken
	nilTok.release()
}

// TestAcquireSlotBlocksAndLogs checks the observability gap that let #196 hide:
// a dispatch parked on a full pool used to log nothing whatsoever.
func TestAcquireSlotBlocksAndLogs(t *testing.T) {
	after, every, leak := slotWaitLogAfter, slotWaitLogEvery, slotLeakSuspectAfter
	defer func() { slotWaitLogAfter, slotWaitLogEvery, slotLeakSuspectAfter = after, every, leak }()
	slotWaitLogAfter = 10 * time.Millisecond
	slotWaitLogEvery = 10 * time.Millisecond
	slotLeakSuspectAfter = time.Hour // stay at Warn for this test

	s := New(Config{MaxConcurrent: 1, MaxMacOSVMs: 1, Log: quietLogger()})
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s.macSem <- struct{}{} // pool full

	got := make(chan *slotToken, 1)
	go func() { got <- s.acquireSlot(context.Background(), s.macSem, "macos", log) }()

	// Give the waiter time to block and log at least once, then free the slot.
	time.Sleep(60 * time.Millisecond)
	<-s.macSem

	select {
	case tok := <-got:
		if tok == nil {
			t.Fatal("acquireSlot returned nil despite a slot becoming free")
		}
		tok.release()
	case <-time.After(5 * time.Second):
		t.Fatal("acquireSlot never returned after the slot was freed")
	}

	out := buf.String()
	if !strings.Contains(out, "waiting for a free concurrency slot") {
		t.Errorf("blocked acquire logged nothing about waiting; got:\n%s", out)
	}
	if !strings.Contains(out, "pool=macos") {
		t.Errorf("wait log did not name the pool; got:\n%s", out)
	}
	if !strings.Contains(out, "capacity=1") {
		t.Errorf("wait log did not report the pool capacity; got:\n%s", out)
	}
}

// TestAcquireSlotFastPathIsSilent: the common case must not narrate itself.
func TestAcquireSlotFastPathIsSilent(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxMacOSVMs: 1, Log: quietLogger()})
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tok := s.acquireSlot(context.Background(), s.macSem, "macos", log)
	if tok == nil {
		t.Fatal("acquireSlot returned nil on an empty pool")
	}
	tok.release()
	if buf.Len() != 0 {
		t.Errorf("uncontended acquire logged %q, want silence", buf.String())
	}
}

// TestAcquireSlotHonoursContext: a cancelled dispatch takes no slot.
func TestAcquireSlotHonoursContext(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxMacOSVMs: 1, Log: quietLogger()})
	s.macSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if tok := s.acquireSlot(ctx, s.macSem, "macos", quietLogger()); tok != nil {
		t.Error("acquireSlot handed out a slot under a cancelled context")
	}
	if len(s.macSem) != 1 {
		t.Errorf("held=%d, want 1 (the cancelled waiter must not have taken one)", len(s.macSem))
	}
}

// TestSlotWaitSeverity pins the leak watchdog's escalation rule: held slots
// with nothing tracked, waited on long enough, is an Error — that combination
// is exactly what `ephemerd status` showed on the wedged mac.
func TestSlotWaitSeverity(t *testing.T) {
	tests := []struct {
		name        string
		waited      time.Duration
		held        int
		tracked     int
		wantLevel   slog.Level
		wantSuspect bool
	}{
		{"this pool has a tracked job, short wait", time.Minute, 1, 1, slog.LevelWarn, false},
		// The third column is jobs tracked IN THIS POOL, not on the node.
		// It used to be s.ActiveJobs() (every pool), which meant a Linux job
		// on the mac explained away a leaked macOS slot; see
		// TestSlotLeakEscalationIsScopedToThePool.
		{"this pool has a tracked job, long wait", 2 * time.Hour, 1, 1, slog.LevelWarn, false},
		{"untracked holder, short wait", time.Minute, 1, 0, slog.LevelWarn, false},
		{"untracked holder, long wait", slotLeakSuspectAfter, 1, 0, slog.LevelError, true},
		{"nothing held, long wait", 2 * time.Hour, 0, 0, slog.LevelWarn, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level, msg := slotWaitSeverity(tc.waited, tc.held, tc.tracked)
			if level != tc.wantLevel {
				t.Errorf("level = %v, want %v", level, tc.wantLevel)
			}
			if suspect := strings.Contains(msg, "suspected slot leak"); suspect != tc.wantSuspect {
				t.Errorf("message %q: leak-suspected = %v, want %v", msg, suspect, tc.wantSuspect)
			}
		})
	}
}

// TestAwaitUnwind covers the bound that replaced the unbounded `<-resCh`.
func TestAwaitUnwind(t *testing.T) {
	closed := func() chan struct{} {
		c := make(chan struct{})
		close(c)
		return c
	}

	t.Run("both already finished", func(t *testing.T) {
		if !awaitUnwind(closed(), closed(), time.Millisecond) {
			t.Error("awaitUnwind reported a timeout when both signals were already done")
		}
	})

	t.Run("both finish in time", func(t *testing.T) {
		stopped, unwound := make(chan struct{}), make(chan struct{})
		go func() { time.Sleep(5 * time.Millisecond); close(stopped) }()
		go func() { time.Sleep(10 * time.Millisecond); close(unwound) }()
		if !awaitUnwind(stopped, unwound, 5*time.Second) {
			t.Error("awaitUnwind reported a timeout for a teardown that completed")
		}
	})

	t.Run("wait never unwinds", func(t *testing.T) {
		start := time.Now()
		if awaitUnwind(closed(), make(chan struct{}), 20*time.Millisecond) {
			t.Error("awaitUnwind claimed a clean unwind for a wait that never returned")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("awaitUnwind took %s to give up; it must be bounded by grace", elapsed)
		}
	})

	t.Run("stop never returns", func(t *testing.T) {
		if awaitUnwind(make(chan struct{}), closed(), 20*time.Millisecond) {
			t.Error("awaitUnwind claimed a clean unwind while Stop was still wedged")
		}
	})
}

// TestStopVMBoundedDoesNotHangOnAWedgedStop: teardown is best-effort,
// capacity is not.
func TestStopVMBoundedDoesNotHangOnAWedgedStop(t *testing.T) {
	macVM := newWedgedMacVM(t)
	s := New(Config{MaxMacOSVMs: 1, Log: quietLogger()})
	s.macStopGrace = 20 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.stopVMBounded(macVM, macVMRef{JobID: 1, VMID: "1-wedged"}, quietLogger())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopVMBounded blocked on a Stop() that never returns")
	}
	if macVM.stopCalls.Load() != 1 {
		t.Errorf("Stop called %d times, want 1", macVM.stopCalls.Load())
	}
	// The VM it walked away from must be charged, or nothing bounds how many
	// of them a node can strand.
	if n := s.AbandonedMacOSVMs(); n != 1 {
		t.Errorf("AbandonedMacOSVMs() = %d after walking away from a wedged stop, want 1", n)
	}
}

// TestSlotUsageReportsHeldSlots is the reporting gap from the issue: a node
// with every slot held must not look idle.
func TestSlotUsageReportsHeldSlots(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxMacOSVMs: 1, Log: quietLogger()})

	if got := s.HeldSlots(); got != 0 {
		t.Errorf("HeldSlots() = %d on a fresh scheduler, want 0", got)
	}
	if got := s.SlotCapacity(); got != 5 { // local 2 + linux 2 + macos 1
		t.Errorf("SlotCapacity() = %d, want 5", got)
	}

	// Hold the macOS slot without tracking a job — the #196 state exactly.
	s.macSem <- struct{}{}
	defer func() { <-s.macSem }()

	if got := s.ActiveJobs(); got != 0 {
		t.Fatalf("ActiveJobs() = %d, want 0 (an untracked holder is the whole point)", got)
	}
	if got := s.HeldSlots(); got != 1 {
		t.Errorf("HeldSlots() = %d while the macOS slot is held, want 1", got)
	}

	byPool := map[string]SlotStats{}
	for _, p := range s.SlotUsage() {
		byPool[p.Pool] = p
	}
	if mac := byPool["macos"]; mac.Held != 1 || mac.Capacity != 1 {
		t.Errorf("macos pool = %+v, want held 1 capacity 1", mac)
	}
	if local := byPool["local"]; local.Held != 0 || local.Capacity != 2 {
		t.Errorf("local pool = %+v, want held 0 capacity 2", local)
	}
}

// TestStatusRPCExposesHeldSlots is the same check for the control socket,
// which is what `ephemerd status` and mayfly actually read.
func TestStatusRPCExposesHeldSlots(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxMacOSVMs: 1, Log: quietLogger()})
	cs := &controlServer{sched: s, log: quietLogger()}

	s.macSem <- struct{}{}
	defer func() { <-s.macSem }()

	resp, err := cs.Status(context.Background(), &apiv1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	// The pre-existing contract is unchanged.
	if resp.Status != "ok" || resp.ActiveJobs != 0 || resp.MaxConcurrent != 2 {
		t.Errorf("existing Status fields changed: %+v", resp)
	}
	if resp.HeldSlots != 1 {
		t.Errorf("HeldSlots = %d, want 1 (the untracked macOS holder)", resp.HeldSlots)
	}
	if resp.SlotCapacity != 5 {
		t.Errorf("SlotCapacity = %d, want 5", resp.SlotCapacity)
	}
	var sawMac bool
	for _, p := range resp.SlotPools {
		if p.Pool == "macos" {
			sawMac = true
			if p.Held != 1 || p.Capacity != 1 {
				t.Errorf("macos pool = %+v, want held 1 capacity 1", p)
			}
		}
	}
	if !sawMac {
		t.Error("Status did not break the macos pool out in SlotPools")
	}
}

// TestHealthzExposesHeldSlots checks the endpoint an operator (and the fleet
// monitor) actually reads. active_jobs keeps its old meaning; held_slots is
// what would have shown the wedged mac.
func TestHealthzExposesHeldSlots(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxMacOSVMs: 1, Log: quietLogger()})
	s.macSem <- struct{}{}
	defer func() { <-s.macSem }()

	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}

	var body struct {
		Status        string      `json:"status"`
		ActiveJobs    int         `json:"active_jobs"`
		MaxConcurrent int         `json:"max_concurrent"`
		HeldSlots     int         `json:"held_slots"`
		SlotCapacity  int         `json:"slot_capacity"`
		Slots         []SlotStats `json:"slots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding healthz body %q: %v", rec.Body.String(), err)
	}
	// Backwards compatibility: the pre-existing fields must be untouched.
	if body.Status != "ok" || body.ActiveJobs != 0 || body.MaxConcurrent != 2 {
		t.Errorf("existing healthz fields changed: %+v", body)
	}
	if body.HeldSlots != 1 {
		t.Errorf("held_slots = %d, want 1", body.HeldSlots)
	}
	if body.SlotCapacity != 5 {
		t.Errorf("slot_capacity = %d, want 5", body.SlotCapacity)
	}
	var sawMac bool
	for _, p := range body.Slots {
		if p.Pool == "macos" {
			sawMac = true
			if p.Held != 1 || p.Capacity != 1 {
				t.Errorf("macos pool in healthz = %+v, want held 1 capacity 1", p)
			}
		}
	}
	if !sawMac {
		t.Error("healthz slots did not break out the macos pool")
	}
}

// ---------------------------------------------------------------------------
// MAJOR 1: no macOS Stop() may block its caller — least of all the event loop.
// ---------------------------------------------------------------------------

// seedMacOSRunner files a tracked macOS job plus its runner-ledger entry, the
// state the orphan sweep, handleCompleted and destroyAll all operate on.
func seedMacOSRunner(s *Scheduler, prov providers.Provider, jobID int64, name string, dispatchedAt time.Time, macVM vm.MacOSVM) jobKey {
	key := jobKey{Provider: prov.Name(), JobID: jobID}
	_, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.running[key] = &runningJob{
		provider:  prov,
		claim:     &providers.Claim{RunnerID: jobID * 10, RunnerName: name, Repo: "myrepo"},
		repo:      "myrepo",
		cancel:    cancel,
		macosVM:   macVM,
		macVMID:   fmt.Sprintf("%d-seeded", jobID),
		startedAt: dispatchedAt,
	}
	s.runners[name] = &runnerBinding{
		intentKey:    key,
		dispatchedAt: dispatchedAt,
		observable:   true,
		labelSet:     labelSetKey([]string{"self-hosted", "macos"}),
	}
	s.mu.Unlock()
	return key
}

// newTeardownTestScheduler builds a scheduler whose macOS teardown bounds are
// milliseconds, for the paths that tear a TRACKED macOS job down.
func newTeardownTestScheduler(t *testing.T, prov providers.Provider, sweep bool) *Scheduler {
	t.Helper()
	s := New(Config{
		Providers:       []providers.Provider{prov},
		MacOSVMConfig:   &vm.MacOSVMConfig{},
		MaxMacOSVMs:     1,
		ShutdownTimeout: 50 * time.Millisecond,
		OrphanSweep:     OrphanSweepConfig{Enabled: sweep, Grace: 10 * time.Minute},
		Log:             quietLogger(),
	})
	s.webhookMode = true
	s.busyProbe = idleProbe
	s.macStopGrace = 50 * time.Millisecond
	s.macUnwindGrace = 50 * time.Millisecond
	return s
}

// mustReturnWithin fails (rather than hangs) if fn has not returned by d.
func mustReturnWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: a macOS VM Stop() is blocking its caller", what, d)
	}
}

// TestSweepOrphanRunners_WedgedMacOSVMCannotBlockTheEventLoop is the worst of
// the three unbounded Stop() sites the #196 fix left behind.
//
// sweepOrphanRunners is called SYNCHRONOUSLY from the cleanupTicker arm of
// Run's select — the scheduler's one and only event loop. A tracked macOS job
// nominated as an orphan whose Vz stop wedges (exactly the failure that
// produced the 28-hour outage) parked that loop forever: no queued,
// in_progress or completed handling for any platform, and no drain on SIGTERM.
// That is strictly worse than the leaked slot the rest of the PR fixes.
//
// Without the fix this test FAILS on mustReturnWithin: the sweep sits inside
// wedgedMacVM.Stop() until the test binary's own timeout.
func TestSweepOrphanRunners_WedgedMacOSVMCannotBlockTheEventLoop(t *testing.T) {
	prov := &reportingProvider{claimCountingProvider: newClaimCountingProvider("mac-sweep")}
	s := newTeardownTestScheduler(t, prov, true)

	macVM := newWedgedMacVM(t)
	key := seedMacOSRunner(s, prov, 4242, "mac-r-1", time.Now().Add(-30*time.Minute), macVM)

	mustReturnWithin(t, 5*time.Second, "sweepOrphanRunners", s.sweepOrphanRunners)

	if macVM.stopCalls.Load() == 0 {
		t.Error("the orphaned macOS VM was never stopped; the sweep must still tear it down")
	}
	s.mu.Lock()
	_, stillRunning := s.running[key]
	s.mu.Unlock()
	if stillRunning {
		t.Error("orphaned macOS job is still tracked after the sweep")
	}
	// A stop it walked away from has to be counted, or nothing bounds them.
	if n := s.AbandonedMacOSVMs(); n != 1 {
		t.Errorf("AbandonedMacOSVMs() = %d after the sweep abandoned a wedged stop, want 1", n)
	}
}

// TestHandleCompleted_WedgedMacOSVMCannotBlockTheHandler: the completed-event
// handler also ran a bare Stop(). A wedge there parks the handler forever, and
// with it the deferred sweepOrphanRunners that reconciles the ledger after
// every completion.
//
// Without the fix this FAILS on mustReturnWithin.
func TestHandleCompleted_WedgedMacOSVMCannotBlockTheHandler(t *testing.T) {
	prov := &reportingProvider{claimCountingProvider: newClaimCountingProvider("mac-completed")}
	s := newTeardownTestScheduler(t, prov, false)

	macVM := newWedgedMacVM(t)
	key := seedMacOSRunner(s, prov, 5150, "mac-r-2", time.Now(), macVM)

	event := providers.JobEvent{
		Provider:   prov,
		Action:     "completed",
		Repo:       "myrepo",
		JobID:      5150,
		RunnerName: "mac-r-2",
		Conclusion: "success",
	}
	mustReturnWithin(t, 5*time.Second, "handleCompleted", func() {
		s.handleCompleted(context.Background(), event)
	})

	if macVM.stopCalls.Load() == 0 {
		t.Error("the completed job's macOS VM was never stopped")
	}
	s.mu.Lock()
	_, stillRunning := s.running[key]
	s.mu.Unlock()
	if stillRunning {
		t.Error("completed macOS job is still tracked")
	}
}

// TestDrain_WedgedMacOSVMCannotBlockShutdown: destroyAll is the force-kill arm
// of drain(), reached only after ShutdownTimeout has already expired. A bare
// Stop() there meant a wedged guest kept the process alive indefinitely —
// drain never returns, Run never returns, and SIGTERM has to be escalated to
// SIGKILL by hand. The shutdown timeout exists to bound shutdown; a teardown
// that ignores it defeats the entire mechanism.
//
// Without the fix this FAILS on mustReturnWithin.
func TestDrain_WedgedMacOSVMCannotBlockShutdown(t *testing.T) {
	prov := &reportingProvider{claimCountingProvider: newClaimCountingProvider("mac-drain")}
	s := newTeardownTestScheduler(t, prov, false)

	macVM := newWedgedMacVM(t)
	seedMacOSRunner(s, prov, 6006, "mac-r-3", time.Now(), macVM)

	mustReturnWithin(t, 10*time.Second, "drain", s.drain)

	if macVM.stopCalls.Load() == 0 {
		t.Error("the running macOS VM was never stopped on shutdown")
	}
	if n := s.ActiveJobs(); n != 0 {
		t.Errorf("ActiveJobs() = %d after drain force-killed everything, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// MAJOR 3: the abandoned-VM trade is bounded and legible.
// ---------------------------------------------------------------------------

// TestMacOSPoolCordonsPastTheAbandonedVMCap.
//
// The semaphore used to be the accidental limiter on stranded guests: leak the
// only slot and the node provisions zero more, so at most one guest could ever
// be abandoned. Releasing the slot on the failure path is exactly what removed
// that, and an abandoned VM is not tracked and no longer charged against
// MaxMacOSVMs — so without an explicit cap a repeatably-wedging guest lets the
// node keep provisioning until Virtualization.framework refuses (about two
// concurrent macOS guests on Apple Silicon) or the host runs out of RAM.
//
// Without the cap this test FAILS: the second dispatch constructs and boots
// another VM instead of being refused.
func TestMacOSPoolCordonsPastTheAbandonedVMCap(t *testing.T) {
	prov := newMockProvider("mac-cap")
	defer func() { _ = prov.Stop(context.Background()) }()

	// Constructed by hand rather than via newWedgedMacVM: this test needs to
	// release the wedged Stop() mid-test to prove the cordon lifts.
	first := &wedgedMacVM{stopReturns: make(chan struct{})}
	s := newSlotTestScheduler(t, prov, first)
	s.macAbandonedCap = 1

	runMacDispatch(t, s, macEvent(prov, 1), 5*time.Second)
	if n := s.AbandonedMacOSVMs(); n != 1 {
		t.Fatalf("AbandonedMacOSVMs() = %d after one unkillable VM, want 1", n)
	}

	// Second job: must be refused before anything is constructed or booted.
	var created atomic.Int32
	s.newMacOSVM = func(vm.MacOSVMConfig, string) (vm.MacOSVM, error) {
		created.Add(1)
		return &wedgedMacVM{stopReturns: make(chan struct{})}, nil
	}
	runMacDispatch(t, s, macEvent(prov, 2), 5*time.Second)
	if got := created.Load(); got != 0 {
		t.Errorf("provisioned %d macOS VMs past the abandoned cap; the point of the cap is that it provisions none", got)
	}
	assertCapacityIntact(t, s.macSem)
	if n := s.ActiveJobs(); n != 0 {
		t.Errorf("ActiveJobs() = %d, want 0", n)
	}

	// The count is reportable, not just internal.
	if n := s.AbandonedMacOSVMs(); n != 1 {
		t.Errorf("AbandonedMacOSVMs() = %d, want 1", n)
	}
	if d := s.AbandonedMacOSVMDetails(); len(d) != 1 || d[0].JobID != 1 {
		t.Errorf("AbandonedMacOSVMDetails() = %+v, want one entry for job 1", d)
	}

	// The cordon is not permanent: a merely-slow Vz stop that eventually
	// returns discharges its entry and the pool takes work again. A node that
	// recovers on its own must not need a restart.
	close(first.stopReturns)
	deadline := time.Now().Add(5 * time.Second)
	for s.AbandonedMacOSVMs() != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := s.AbandonedMacOSVMs(); n != 0 {
		t.Fatalf("AbandonedMacOSVMs() = %d after the abandoned stop finally returned, want 0", n)
	}

	runMacDispatch(t, s, macEvent(prov, 3), 5*time.Second)
	if created.Load() == 0 {
		t.Error("the macOS pool stayed cordoned after its abandoned VMs were confirmed dead")
	}
}

// TestStatusAndHealthzReportAbandonedMacOSVMs: held_slots showed the outage;
// abandoned_macos_vms shows what the fix for it costs. An operator has to be
// able to see stranded guests without reading the log.
func TestStatusAndHealthzReportAbandonedMacOSVMs(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxMacOSVMs: 1, Log: quietLogger()})
	s.macAbandoned.charge(macVMRef{JobID: 77, VMID: "77-deadbeef"}, "test")

	cs := &controlServer{sched: s, log: quietLogger()}
	resp, err := cs.Status(context.Background(), &apiv1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if resp.AbandonedMacosVms != 1 {
		t.Errorf("Status.AbandonedMacosVms = %d, want 1", resp.AbandonedMacosVms)
	}
	if resp.AbandonedMacosVmCap != defaultMacAbandonedVMCap {
		t.Errorf("Status.AbandonedMacosVmCap = %d, want %d", resp.AbandonedMacosVmCap, defaultMacAbandonedVMCap)
	}
	// The pre-existing contract is untouched.
	if resp.Status != "ok" || resp.ActiveJobs != 0 || resp.MaxConcurrent != 2 {
		t.Errorf("existing Status fields changed: %+v", resp)
	}

	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body struct {
		ActiveJobs        int              `json:"active_jobs"`
		HeldSlots         int              `json:"held_slots"`
		AbandonedMacOSVMs int              `json:"abandoned_macos_vms"`
		AbandonedCap      int              `json:"abandoned_macos_vm_cap"`
		Details           []AbandonedMacVM `json:"abandoned_macos_vm_details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding healthz body %q: %v", rec.Body.String(), err)
	}
	if body.AbandonedMacOSVMs != 1 {
		t.Errorf("healthz abandoned_macos_vms = %d, want 1", body.AbandonedMacOSVMs)
	}
	if body.AbandonedCap != defaultMacAbandonedVMCap {
		t.Errorf("healthz abandoned_macos_vm_cap = %d, want %d", body.AbandonedCap, defaultMacAbandonedVMCap)
	}
	if len(body.Details) != 1 || body.Details[0].VMID != "77-deadbeef" {
		t.Errorf("healthz abandoned_macos_vm_details = %+v, want the stranded VM named", body.Details)
	}
}

// ---------------------------------------------------------------------------
// MINOR 5: the deadline path already stopped the VM; do not stop it again.
// ---------------------------------------------------------------------------

// TestHandleMacOSJob_DeadlinePathStopsTheVMExactlyOnce.
//
// waitForMacRunnerBounded force-stops the VM when the deadline fires (#178,
// unchanged). The caller's error path then called stopVMBounded on the very
// same VM. Against darwinMacOSVM's stopOnce that is never a cheap no-op: the
// second caller BLOCKS until the first invocation returns, so on the wedged
// guest this whole PR is about it burned a full teardown grace and parked a
// third goroutine inside the Once — while a ghost JIT runner still needed
// deregistering.
//
// Without the fix this test FAILS: Stop is called twice and the second
// abandonment is charged against the cap as well, so a single wedged VM
// consumes the node's entire abandoned-VM budget.
func TestHandleMacOSJob_DeadlinePathStopsTheVMExactlyOnce(t *testing.T) {
	prov := newMockProvider("mac-double-stop")
	defer func() { _ = prov.Stop(context.Background()) }()

	macVM := newWedgedMacVM(t)
	s := newSlotTestScheduler(t, prov, macVM)
	s.macAbandonedCap = 99 // not the subject here

	runMacDispatch(t, s, macEvent(prov, 999), 5*time.Second)

	if got := macVM.stopCalls.Load(); got != 1 {
		t.Errorf("Stop called %d times on the deadline path, want 1 — the second call re-enters a sync.Once that is still wedged", got)
	}
	if n := s.AbandonedMacOSVMs(); n != 1 {
		t.Errorf("AbandonedMacOSVMs() = %d for ONE wedged VM, want 1 — a redundant stop must not double-charge the cap", n)
	}
}

// ---------------------------------------------------------------------------
// MINOR 4: the leak escalation is per-pool, not per-node.
// ---------------------------------------------------------------------------

// TestTrackedInPool: jobs are counted against the pool whose slot they hold.
func TestTrackedInPool(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxMacOSVMs: 2, Log: quietLogger()})
	s.mu.Lock()
	s.running[jobKey{Provider: "p", JobID: 1}] = &runningJob{}                  // local
	s.running[jobKey{Provider: "p", JobID: 2}] = &runningJob{dispatched: "r-2"} // linux
	s.running[jobKey{Provider: "p", JobID: 3}] = &runningJob{
		macosVM: &fastMacVM{stops: new(atomic.Int32)},
	} // macos
	s.mu.Unlock()

	for pool, want := range map[string]int{"local": 1, "linux": 1, "macos": 1} {
		if got := s.trackedInPool(pool); got != want {
			t.Errorf("trackedInPool(%q) = %d, want %d", pool, got, want)
		}
	}
	if got := s.ActiveJobs(); got != 3 {
		t.Errorf("ActiveJobs() = %d, want 3 (the global count keeps its meaning)", got)
	}
}

// TestSlotLeakEscalationIsScopedToThePool is the reason trackedInPool exists.
//
// The escalation compared a PER-POOL held count against s.ActiveJobs(), which
// is len(s.running) across every pool. The #196 node is a mac: it serves Linux
// jobs from its embedded VM continuously, so there was almost always a tracked
// job somewhere — and one was enough to make a leaked macOS slot look
// accounted for and hold the report at Warn forever. The one signal that would
// have named the outage was suppressed by unrelated work.
//
// Without the fix this test FAILS: the log says "waiting for a free
// concurrency slot" at Warn instead of naming the suspected leak.
func TestSlotLeakEscalationIsScopedToThePool(t *testing.T) {
	after, every, leak := slotWaitLogAfter, slotWaitLogEvery, slotLeakSuspectAfter
	defer func() { slotWaitLogAfter, slotWaitLogEvery, slotLeakSuspectAfter = after, every, leak }()
	slotWaitLogAfter = 10 * time.Millisecond
	slotWaitLogEvery = 10 * time.Millisecond
	slotLeakSuspectAfter = 10 * time.Millisecond

	s := New(Config{MaxConcurrent: 1, MaxMacOSVMs: 1, Log: quietLogger()})

	// A perfectly healthy LOCAL job, tracked, doing real work. It has nothing
	// to do with the macOS pool.
	s.mu.Lock()
	s.running[jobKey{Provider: "github", JobID: 1}] = &runningJob{repo: "myrepo"}
	s.mu.Unlock()

	// The macOS slot is held by nothing tracked: the #196 state exactly.
	s.macSem <- struct{}{}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := make(chan *slotToken, 1)
	go func() { got <- s.acquireSlot(context.Background(), s.macSem, "macos", log) }()

	time.Sleep(60 * time.Millisecond)
	<-s.macSem // let the waiter through so the goroutine (and the log) settle
	select {
	case tok := <-got:
		tok.release()
	case <-time.After(5 * time.Second):
		t.Fatal("acquireSlot never returned after the slot was freed")
	}

	out := buf.String()
	if !strings.Contains(out, "suspected slot leak") {
		t.Errorf("a leaked macOS slot was not escalated because an unrelated LOCAL job was tracked; got:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("leak report was not logged at ERROR; got:\n%s", out)
	}
	if !strings.Contains(out, "pool_tracked_jobs=0") {
		t.Errorf("wait log did not report the POOL's tracked-job count; got:\n%s", out)
	}
}

