package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
)

// mockProvider is a fake providers.Poll implementation for testing
// multi-provider fan-in, composite keys, and per-provider routing.
type mockProvider struct {
	name         string
	defaultImage string
	events       chan providers.JobEvent

	mu       sync.Mutex
	claims   []*providers.Claim  // jobs claimed via ClaimJob
	releases []*providers.Claim  // jobs released via ReleaseJob
	images   map[int64]string    // jobID → image for FetchJobImage
}

var _ providers.Poll = (*mockProvider)(nil)

func newMockProvider(name string) *mockProvider {
	return &mockProvider{
		name:         name,
		defaultImage: name + "-runner:latest",
		events:       make(chan providers.JobEvent, 64),
		images:       make(map[int64]string),
	}
}

func (m *mockProvider) Name() string         { return m.name }
func (m *mockProvider) DefaultImage() string { return m.defaultImage }
func (m *mockProvider) DefaultJobImage() string { return "" }

func (m *mockProvider) Start(_ context.Context, _ providers.PollConfig) (<-chan providers.JobEvent, error) {
	return m.events, nil
}

func (m *mockProvider) ClaimJob(_ context.Context, event *providers.JobEvent, runnerName string, _ []string) (*providers.Claim, error) {
	claim := &providers.Claim{
		RunnerID:   event.JobID * 10, // deterministic ID for assertions
		RunnerName: runnerName,
		Repo:       event.Repo,
	}
	m.mu.Lock()
	m.claims = append(m.claims, claim)
	m.mu.Unlock()
	return claim, nil
}

func (m *mockProvider) ReleaseJob(_ context.Context, claim *providers.Claim) error {
	m.mu.Lock()
	m.releases = append(m.releases, claim)
	m.mu.Unlock()
	return nil
}

func (m *mockProvider) FetchJobImage(_ context.Context, event *providers.JobEvent) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.images[event.JobID]
}

func (m *mockProvider) Stop(_ context.Context) error {
	close(m.events)
	return nil
}

// emit sends a job event on the provider's channel with Provider set to itself.
func (m *mockProvider) emit(action string, jobID int64, repo string) {
	m.events <- providers.JobEvent{
		Provider: m,
		Action:   action,
		Repo:     repo,
		JobID:    jobID,
		Labels:   []string{"self-hosted"},
	}
}

func mpLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- Tests ---

func TestMultiProvider_CompositeKeyDedup(t *testing.T) {
	// Two providers emit events with the SAME job ID.
	// They must be tracked independently (not deduplicated).
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	// Same job ID from different providers → different keys
	keyGH := keyFor(providers.JobEvent{Provider: gh, JobID: 42})
	keyFG := keyFor(providers.JobEvent{Provider: fg, JobID: 42})

	if keyGH == keyFG {
		t.Fatal("composite keys should differ for same job ID from different providers")
	}

	// Mark both as seen
	s.seen[keyGH] = time.Now()
	s.seen[keyFG] = time.Now()

	if len(s.seen) != 2 {
		t.Errorf("seen map should have 2 entries, got %d", len(s.seen))
	}

	// Track both as running
	s.running[keyGH] = &runningJob{repo: "repo", startedAt: time.Now()}
	s.running[keyFG] = &runningJob{repo: "repo", startedAt: time.Now()}

	if len(s.running) != 2 {
		t.Errorf("running map should have 2 entries, got %d", len(s.running))
	}
}

func TestMultiProvider_HandleQueuedRoutesToCorrectProvider(t *testing.T) {
	// When a queued event arrives from provider A, handleQueued should
	// record it under provider A's composite key.
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	event := providers.JobEvent{
		Provider: fg,
		Action:   "queued",
		Repo:     "myrepo",
		JobID:    100,
		Labels:   []string{"self-hosted"},
	}

	// Set draining so handleQueued records the event in seen then
	// exits early without needing a Runtime.
	s.draining = true
	s.handleQueued(context.Background(), event)

	key := keyFor(event)
	s.mu.Lock()
	_, seen := s.seen[key]
	s.mu.Unlock()

	if !seen {
		t.Error("event should be recorded in seen map with forgejo composite key")
	}

	if key.Provider != "forgejo" {
		t.Errorf("key.Provider = %q, want forgejo", key.Provider)
	}
}

func TestMultiProvider_HandleCompletedRoutesToCorrectProvider(t *testing.T) {
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	// Simulate running jobs from both providers
	ghKey := jobKey{Provider: "github", JobID: 1}
	fgKey := jobKey{Provider: "forgejo", JobID: 1}

	_, ghCancel := context.WithCancel(context.Background())
	_, fgCancel := context.WithCancel(context.Background())

	s.running[ghKey] = &runningJob{
		provider: gh,
		claim:    &providers.Claim{RunnerID: 10, Repo: "repo"},
		repo:     "repo",
		cancel:   ghCancel,
		startedAt: time.Now(),
	}
	s.running[fgKey] = &runningJob{
		provider: fg,
		claim:    &providers.Claim{RunnerID: 20, Repo: "repo"},
		repo:     "repo",
		cancel:   fgCancel,
		startedAt: time.Now(),
	}

	// Complete the forgejo job — should only remove the forgejo entry
	s.handleCompleted(context.Background(), providers.JobEvent{
		Provider:   fg,
		Action:     "completed",
		Repo:       "repo",
		JobID:      1,
		Conclusion: "success",
	})

	s.mu.Lock()
	_, ghStillRunning := s.running[ghKey]
	_, fgStillRunning := s.running[fgKey]
	s.mu.Unlock()

	if !ghStillRunning {
		t.Error("github job should still be running after forgejo job completed")
	}
	if fgStillRunning {
		t.Error("forgejo job should be removed after completion")
	}
}

func TestMultiProvider_DestroyAllReleasesEachProvider(t *testing.T) {
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	_, ghCancel := context.WithCancel(context.Background())
	_, fgCancel := context.WithCancel(context.Background())

	ghClaim := &providers.Claim{RunnerID: 10, Repo: "repo-a"}
	fgClaim := &providers.Claim{RunnerID: 20, Repo: "repo-b"}

	s.running[jobKey{Provider: "github", JobID: 1}] = &runningJob{
		provider:  gh,
		claim:     ghClaim,
		repo:      "repo-a",
		cancel:    ghCancel,
		startedAt: time.Now(),
	}
	s.running[jobKey{Provider: "forgejo", JobID: 2}] = &runningJob{
		provider:  fg,
		claim:     fgClaim,
		repo:      "repo-b",
		cancel:    fgCancel,
		startedAt: time.Now(),
	}

	s.destroyAll()

	// Both providers should have received ReleaseJob calls
	gh.mu.Lock()
	ghReleases := len(gh.releases)
	gh.mu.Unlock()

	fg.mu.Lock()
	fgReleases := len(fg.releases)
	fg.mu.Unlock()

	if ghReleases != 1 {
		t.Errorf("github provider got %d ReleaseJob calls, want 1", ghReleases)
	}
	if fgReleases != 1 {
		t.Errorf("forgejo provider got %d ReleaseJob calls, want 1", fgReleases)
	}

	// Verify the right claims were released
	gh.mu.Lock()
	if gh.releases[0].RunnerID != 10 {
		t.Errorf("github released runner %d, want 10", gh.releases[0].RunnerID)
	}
	gh.mu.Unlock()

	fg.mu.Lock()
	if fg.releases[0].RunnerID != 20 {
		t.Errorf("forgejo released runner %d, want 20", fg.releases[0].RunnerID)
	}
	fg.mu.Unlock()

	// Running map should be empty
	s.mu.Lock()
	if len(s.running) != 0 {
		t.Errorf("running map should be empty after destroyAll, got %d", len(s.running))
	}
	s.mu.Unlock()
}

func TestMultiProvider_DefaultImagePerProvider(t *testing.T) {
	gh := newMockProvider("github")
	gh.defaultImage = "ghcr.io/actions/actions-runner:latest"
	fg := newMockProvider("forgejo")
	fg.defaultImage = "data.forgejo.org/forgejo/runner:12"

	if gh.DefaultImage() == fg.DefaultImage() {
		t.Fatal("providers should have different default images")
	}
	if gh.DefaultImage() != "ghcr.io/actions/actions-runner:latest" {
		t.Errorf("github DefaultImage = %q", gh.DefaultImage())
	}
	if fg.DefaultImage() != "data.forgejo.org/forgejo/runner:12" {
		t.Errorf("forgejo DefaultImage = %q", fg.DefaultImage())
	}
}

func TestMultiProvider_RunnerNameIncludesProvider(t *testing.T) {
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	// claimJob generates names like "ephemerd-<provider>-<repo>-<random>"
	ghEvent := &providers.JobEvent{Provider: gh, Repo: "myrepo", JobID: 1}
	fgEvent := &providers.JobEvent{Provider: fg, Repo: "myrepo", JobID: 1}

	ghClaim, err := s.claimJob(context.Background(), ghEvent, nil, mpLogger(), 1)
	if err != nil {
		t.Fatalf("claimJob(github): %v", err)
	}
	fgClaim, err := s.claimJob(context.Background(), fgEvent, nil, mpLogger(), 1)
	if err != nil {
		t.Fatalf("claimJob(forgejo): %v", err)
	}

	// Runner names should contain the provider name
	if ghClaim.RunnerName == "" || fgClaim.RunnerName == "" {
		t.Fatal("runner names should not be empty")
	}

	// The names should start with ephemerd-<provider>-
	ghPrefix := "ephemerd-github-myrepo-"
	fgPrefix := "ephemerd-forgejo-myrepo-"
	if len(ghClaim.RunnerName) < len(ghPrefix) || ghClaim.RunnerName[:len(ghPrefix)] != ghPrefix {
		t.Errorf("github runner name %q should start with %q", ghClaim.RunnerName, ghPrefix)
	}
	if len(fgClaim.RunnerName) < len(fgPrefix) || fgClaim.RunnerName[:len(fgPrefix)] != fgPrefix {
		t.Errorf("forgejo runner name %q should start with %q", fgClaim.RunnerName, fgPrefix)
	}
}

func TestMultiProvider_KeyForNilProvider(t *testing.T) {
	// Events with nil Provider (e.g., from tests) should still produce a valid key.
	event := providers.JobEvent{JobID: 42}
	key := keyFor(event)
	if key.Provider != "" {
		t.Errorf("key.Provider = %q, want empty for nil provider", key.Provider)
	}
	if key.JobID != 42 {
		t.Errorf("key.JobID = %d, want 42", key.JobID)
	}
}

func TestMultiProvider_SameJobIDDifferentProviders(t *testing.T) {
	// Regression: ensure job ID 42 from github and job ID 42 from forgejo
	// don't collide in the seen or running maps.
	gh := newMockProvider("github")
	fg := newMockProvider("forgejo")

	s := New(Config{
		Providers: []providers.Provider{gh, fg},
		Log:       mpLogger(),
	})

	// Both providers see job 42
	s.seen[jobKey{Provider: "github", JobID: 42}] = time.Now()
	s.seen[jobKey{Provider: "forgejo", JobID: 42}] = time.Now()

	// cleanSeen should keep both if fresh
	s.cleanSeen()
	if len(s.seen) != 2 {
		t.Errorf("cleanSeen removed fresh entries: got %d, want 2", len(s.seen))
	}

	// Expire only github's entry
	s.seen[jobKey{Provider: "github", JobID: 42}] = time.Now().Add(-seenTTL - time.Minute)
	s.cleanSeen()

	if len(s.seen) != 1 {
		t.Errorf("cleanSeen should leave 1 entry, got %d", len(s.seen))
	}
	if _, ok := s.seen[jobKey{Provider: "forgejo", JobID: 42}]; !ok {
		t.Error("forgejo entry should survive cleanup")
	}
}
