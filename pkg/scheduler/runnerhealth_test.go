package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// fakeRunnerHealth records every detector call and answers Allow from a
// per-platform table.
type fakeRunnerHealth struct {
	mu         sync.Mutex
	registered map[string]string // runner -> platform
	started    []string
	exited     map[string]bool // runner -> killedByUs
	deny       map[string]bool // platform -> breaker open
	allowCalls []string
}

func newFakeRunnerHealth() *fakeRunnerHealth {
	return &fakeRunnerHealth{registered: map[string]string{}, exited: map[string]bool{}, deny: map[string]bool{}}
}

func (f *fakeRunnerHealth) Registered(name, platform string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered[name] = platform
}

func (f *fakeRunnerHealth) JobStarted(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
}

func (f *fakeRunnerHealth) Exited(name string, killedByUs bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exited[name] = killedByUs
}

func (f *fakeRunnerHealth) Allow(platform string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowCalls = append(f.allowCalls, platform)
	return !f.deny[platform]
}

// The breaker is a live gate at the point of claiming: an open breaker
// abandons the dispatch (and only that platform's), a closed one admits.
func TestAdmitDispatch_BreakerGate(t *testing.T) {
	prov := newClaimErrorProvider("github", nil)
	rh := newFakeRunnerHealth()
	s := New(Config{
		Providers:     []providers.Provider{prov},
		MaxConcurrent: 1,
		RunnerHealth:  rh,
		Log:           quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bindContexts(ctx)

	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 300}

	if got := s.admitDispatch(ctx, event, 0, "linux"); got != dispatchAdmit {
		t.Fatalf("closed breaker: got %v, want admit", got)
	}
	rh.deny["linux"] = true
	if got := s.admitDispatch(ctx, event, 0, "linux"); got != dispatchAbandonBreaker {
		t.Fatalf("open breaker: got %v, want abandon-breaker", got)
	}
	// Another platform's breaker is independent.
	if got := s.admitDispatch(ctx, event, 0, "windows"); got != dispatchAdmit {
		t.Fatalf("other platform: got %v, want admit", got)
	}
	// Cordon still wins over everything and does not consult the breaker.
	before := len(rh.allowCalls)
	s.Cordon()
	if got := s.admitDispatch(ctx, event, 0, "linux"); got != dispatchAbandonCordoned {
		t.Fatalf("cordoned: got %v", got)
	}
	if len(rh.allowCalls) != before {
		t.Fatal("cordoned dispatch must not consume a breaker Allow (a half-open probe slot)")
	}
}

// Nil RunnerHealth is the historical scheduler: admitDispatch never gates
// on a breaker and trackRunning/handleInProgress never call anything.
func TestRunnerHealth_NilIsNoop(t *testing.T) {
	prov := newClaimErrorProvider("github", nil)
	s := New(Config{Providers: []providers.Provider{prov}, MaxConcurrent: 1, Log: quietLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bindContexts(ctx)
	event := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 301}
	if got := s.admitDispatch(ctx, event, 0, "linux"); got != dispatchAdmit {
		t.Fatalf("got %v", got)
	}
	rj := &runningJob{claim: &providers.Claim{RunnerName: "r-301"}, provider: prov}
	s.trackRunning(keyFor(event), rj, prov, "")
	s.handleInProgress(providers.JobEvent{Provider: prov, Action: "in_progress", Repo: "r", JobID: 301, RunnerName: "r-301"})
	s.runnerExited("r-301", ctx)
}

// Lifecycle plumbing: Registered on track (only for providers that report
// runner names), JobStarted on the in_progress webhook that names the
// runner, Exited with killedByUs derived from the job context.
func TestRunnerHealth_LifecycleHooks(t *testing.T) {
	// A provider that reports runner names (like GitHub): its runners are
	// observable, so they are registered with the detector.
	prov := &reportingProvider{claimCountingProvider: newClaimCountingProvider("github")}
	rh := newFakeRunnerHealth()
	s := New(Config{Providers: []providers.Provider{prov}, MaxConcurrent: 2, RunnerHealth: rh, Log: quietLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bindContexts(ctx)

	ev := providers.JobEvent{Provider: prov, Action: "queued", Repo: "r", JobID: 302}
	key := keyFor(ev)

	// Local job on this host.
	local := &runningJob{claim: &providers.Claim{RunnerName: "local-302"}, provider: prov}
	s.trackRunning(key, local, prov, "")
	// Linux-VM dispatch and macOS are classed by their path, not the host.
	s.trackRunning(jobKey{Provider: "github", JobID: 303}, &runningJob{claim: &providers.Claim{RunnerName: "vm-303"}, dispatched: "vm-303", provider: prov}, prov, "")

	if rh.registered["local-302"] == "" || rh.registered["vm-303"] != "linux" {
		t.Fatalf("registered: %+v", rh.registered)
	}
	// A provider that does NOT report runner names never registers: with no
	// observable job-started signal every short job would look zero-work.
	blind := newClaimErrorProvider("gitea", nil)
	s.trackRunning(jobKey{Provider: "gitea", JobID: 304}, &runningJob{claim: &providers.Claim{RunnerName: "blind-304"}, provider: blind}, blind, "")
	if _, ok := rh.registered["blind-304"]; ok {
		t.Fatal("runner of a provider without RunnerNameReporter must not be registered")
	}

	// in_progress naming our runner -> JobStarted once (a second identical
	// report, as polling produces, does not repeat it).
	ip := providers.JobEvent{Provider: prov, Action: "in_progress", Repo: "r", JobID: 302, RunnerName: "local-302"}
	s.handleInProgress(ip)
	s.handleInProgress(ip)
	if len(rh.started) != 1 || rh.started[0] != "local-302" {
		t.Fatalf("started: %v", rh.started)
	}
	// A runner we do not own is ignored.
	s.handleInProgress(providers.JobEvent{Provider: prov, Action: "in_progress", Repo: "r", JobID: 999, RunnerName: "someone-elses"})
	if len(rh.started) != 1 {
		t.Fatalf("foreign runner must not count: %v", rh.started)
	}

	// Exit: live context = runner ended on its own; cancelled = we killed it.
	s.runnerExited("local-302", ctx)
	jobCtx, jobCancel := context.WithCancel(ctx)
	jobCancel()
	s.runnerExited("vm-303", jobCtx)
	if rh.exited["local-302"] || !rh.exited["vm-303"] {
		t.Fatalf("exited: %+v", rh.exited)
	}
}
