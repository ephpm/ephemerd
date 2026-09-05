package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		{"busy node, short wait", time.Minute, 1, 1, slog.LevelWarn, false},
		{"busy node, long wait", 2 * time.Hour, 1, 1, slog.LevelWarn, false},
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopVMBounded(macVM, 20*time.Millisecond, quietLogger())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopVMBounded blocked on a Stop() that never returns")
	}
	if macVM.stopCalls.Load() != 1 {
		t.Errorf("Stop called %d times, want 1", macVM.stopCalls.Load())
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
