package scheduler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- New() tests ---

func TestNew_Defaults(t *testing.T) {
	s := New(Config{Log: testLogger()})

	if s.cfg.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", s.cfg.MaxConcurrent)
	}
	if s.cfg.WebhookPort != 8080 {
		t.Errorf("WebhookPort = %d, want 8080", s.cfg.WebhookPort)
	}
	if cap(s.sem) != 4 {
		t.Errorf("sem capacity = %d, want 4", cap(s.sem))
	}
	if s.running == nil {
		t.Error("running map is nil")
	}
	if s.seen == nil {
		t.Error("seen map is nil")
	}
}

func TestNew_CustomValues(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 8,
		WebhookPort:   9090,
		Log:           testLogger(),
	})

	if s.cfg.MaxConcurrent != 8 {
		t.Errorf("MaxConcurrent = %d, want 8", s.cfg.MaxConcurrent)
	}
	if s.cfg.WebhookPort != 9090 {
		t.Errorf("WebhookPort = %d, want 9090", s.cfg.WebhookPort)
	}
	if cap(s.sem) != 8 {
		t.Errorf("sem capacity = %d, want 8", cap(s.sem))
	}
}

func TestNew_NegativeMaxConcurrent(t *testing.T) {
	s := New(Config{MaxConcurrent: -1, Log: testLogger()})
	if s.cfg.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want default 4", s.cfg.MaxConcurrent)
	}
}

func TestNew_ZeroWebhookPort(t *testing.T) {
	s := New(Config{WebhookPort: 0, Log: testLogger()})
	if s.cfg.WebhookPort != 8080 {
		t.Errorf("WebhookPort = %d, want default 8080", s.cfg.WebhookPort)
	}
}

// --- isMacOSJob tests ---

func TestIsMacOSJob(t *testing.T) {
	tests := []struct {
		labels []string
		want   bool
	}{
		{[]string{"self-hosted", "macos", "arm64"}, true},
		{[]string{"self-hosted", "macosx"}, true},
		{[]string{"macos-14"}, true},
		{[]string{"macos-latest"}, true},
		{[]string{"self-hosted", "MACOS", "ARM64"}, true},
		{[]string{"self-hosted", "linux", "x64"}, false},
		{[]string{"ubuntu-latest"}, false},
		{[]string{"windows-latest"}, false},
		{[]string{"self-hosted"}, false},
		{nil, false},
		{[]string{}, false},
	}

	for _, tt := range tests {
		got := isMacOSJob(tt.labels)
		if got != tt.want {
			t.Errorf("isMacOSJob(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

// --- buildLabelsForOS tests ---

func expectedArchLabel() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

func TestBuildLabelsForOS(t *testing.T) {
	tests := []struct {
		name      string
		targetOS  string
		extra     []string
		wantFirst string
		wantOS    string
	}{
		{"linux", "linux", nil, "self-hosted", "linux"},
		{"windows", "windows", nil, "self-hosted", "windows"},
		{"darwin", "darwin", nil, "self-hosted", "macos"},
		{"with extra labels", "linux", []string{"gpu", "fast"}, "self-hosted", "linux"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := buildLabelsForOS(tt.targetOS, tt.extra)
			if len(labels) < 3 {
				t.Fatalf("expected at least 3 labels, got %d: %v", len(labels), labels)
			}
			if labels[0] != tt.wantFirst {
				t.Errorf("labels[0] = %q, want %q", labels[0], tt.wantFirst)
			}
			if labels[1] != tt.wantOS {
				t.Errorf("labels[1] = %q, want %q", labels[1], tt.wantOS)
			}
			if labels[2] != "x64" && labels[2] != "arm64" {
				t.Errorf("labels[2] = %q, want x64 or arm64", labels[2])
			}
			for i, extra := range tt.extra {
				if labels[3+i] != extra {
					t.Errorf("labels[%d] = %q, want %q", 3+i, labels[3+i], extra)
				}
			}
		})
	}
}

func TestBuildLabelsForOS_Darwin(t *testing.T) {
	labels := buildLabelsForOS("darwin", []string{"gpu"})

	if labels[0] != "self-hosted" {
		t.Errorf("labels[0] = %q, want self-hosted", labels[0])
	}
	if labels[1] != "macos" {
		t.Errorf("labels[1] = %q, want macos", labels[1])
	}
	if labels[2] != expectedArchLabel() {
		t.Errorf("labels[2] = %q, want %q", labels[2], expectedArchLabel())
	}
	if labels[3] != "gpu" {
		t.Errorf("labels[3] = %q, want gpu", labels[3])
	}
}

func TestBuildLabelsForOS_NoExtraLabels(t *testing.T) {
	labels := buildLabelsForOS("linux", nil)
	if len(labels) != 3 {
		t.Errorf("expected 3 labels, got %d: %v", len(labels), labels)
	}
}

func TestBuildLabelsForOS_EmptyExtraLabels(t *testing.T) {
	labels := buildLabelsForOS("linux", []string{})
	if len(labels) != 3 {
		t.Errorf("expected 3 labels, got %d: %v", len(labels), labels)
	}
}

// --- isLinuxJob tests ---

func TestIsLinuxJob(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"with linux label", []string{"self-hosted", "linux", "x64"}, true},
		{"linux only", []string{"linux"}, true},
		{"case insensitive", []string{"Linux"}, true},
		{"mixed case", []string{"LINUX"}, true},
		{"no linux", []string{"self-hosted", "windows", "x64"}, false},
		{"empty labels", nil, false},
		{"macos label", []string{"self-hosted", "macos"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLinuxJob(tt.labels); got != tt.want {
				t.Errorf("isLinuxJob(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// --- canHandleJob tests ---

func TestCanHandleJob(t *testing.T) {
	tests := []struct {
		name       string
		labels     []string
		dispatcher bool
		want       bool
	}{
		{"no OS label accepts", []string{"self-hosted", "x64"}, false, true},
		{"empty labels accepts", nil, false, true},
		{"linux with dispatcher", []string{"self-hosted", "linux"}, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{Log: testLogger()})
			if tt.dispatcher {
				s.cfg.LinuxDispatcher = &DispatchClient{}
			}
			if got := s.canHandleJob(tt.labels); got != tt.want {
				t.Errorf("canHandleJob(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestCanHandleJob_PlatformSpecific(t *testing.T) {
	s := New(Config{Log: testLogger()})

	winResult := s.canHandleJob([]string{"self-hosted", "windows"})
	macResult := s.canHandleJob([]string{"self-hosted", "macos"})
	macxResult := s.canHandleJob([]string{"self-hosted", "macosx"})

	if macResult != macxResult {
		t.Errorf("macos (%v) and macosx (%v) should produce the same result", macResult, macxResult)
	}

	trueCount := 0
	if winResult {
		trueCount++
	}
	if macResult {
		trueCount++
	}
	if trueCount > 1 {
		t.Error("windows and macos labels should not both be accepted on the same platform")
	}
}

// --- isConflict tests ---

func TestIsConflict_GitHubErrorResponse(t *testing.T) {
	ghErr := &gh.ErrorResponse{
		Response: &http.Response{
			StatusCode: http.StatusConflict,
		},
	}
	if !isConflict(ghErr) {
		t.Error("expected GitHub 409 ErrorResponse to be detected as conflict")
	}
}

func TestIsConflict_WrappedGitHubError(t *testing.T) {
	ghErr := &gh.ErrorResponse{
		Response: &http.Response{
			StatusCode: http.StatusConflict,
		},
	}
	wrapped := errors.Join(errors.New("context"), ghErr)
	if !isConflict(wrapped) {
		t.Error("expected wrapped GitHub 409 to be detected as conflict")
	}
}

func TestIsConflict_Non409(t *testing.T) {
	ghErr := &gh.ErrorResponse{
		Response: &http.Response{
			StatusCode: http.StatusNotFound,
		},
	}
	if isConflict(ghErr) {
		t.Error("expected 404 not to be detected as conflict")
	}
}

func TestIsConflict_StringFallback(t *testing.T) {
	err := errors.New("POST https://api.github.com/...: 409 Conflict")
	if !isConflict(err) {
		t.Error("expected error containing '409' to be detected as conflict")
	}
}

func TestIsConflict_NoConflict(t *testing.T) {
	err := errors.New("connection refused")
	if isConflict(err) {
		t.Error("expected generic error not to be detected as conflict")
	}
}

// --- cleanSeen tests ---

func TestCleanSeen_RemovesExpired(t *testing.T) {
	s := New(Config{Log: testLogger()})

	s.seen[1] = time.Now()
	s.seen[2] = time.Now().Add(-seenTTL - time.Minute)
	s.seen[3] = time.Now().Add(-seenTTL - time.Hour)

	s.cleanSeen()

	if _, exists := s.seen[1]; !exists {
		t.Error("fresh entry should not be cleaned")
	}
	if _, exists := s.seen[2]; exists {
		t.Error("expired entry should be cleaned")
	}
	if _, exists := s.seen[3]; exists {
		t.Error("old entry should be cleaned")
	}
}

func TestCleanSeen_EmptyMap(t *testing.T) {
	s := New(Config{Log: testLogger()})
	s.cleanSeen()
	if len(s.seen) != 0 {
		t.Errorf("seen map should be empty, got %d entries", len(s.seen))
	}
}

func TestCleanSeen_AllFresh(t *testing.T) {
	s := New(Config{Log: testLogger()})
	s.seen[1] = time.Now()
	s.seen[2] = time.Now()
	s.seen[3] = time.Now()

	s.cleanSeen()

	if len(s.seen) != 3 {
		t.Errorf("expected 3 entries, got %d", len(s.seen))
	}
}

func TestCleanSeen_AllExpired(t *testing.T) {
	s := New(Config{Log: testLogger()})
	old := time.Now().Add(-seenTTL - time.Minute)
	s.seen[1] = old
	s.seen[2] = old
	s.seen[3] = old

	s.cleanSeen()

	if len(s.seen) != 0 {
		t.Errorf("expected 0 entries after cleanup, got %d", len(s.seen))
	}
}

// --- handleHealthz tests ---

func TestHandleHealthz(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 4,
		Log:           testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("status = %v, want %q", body["status"], "ok")
	}
	if body["max_concurrent"] != float64(4) {
		t.Errorf("max_concurrent = %v, want 4", body["max_concurrent"])
	}
	if body["draining"] != false {
		t.Errorf("draining = %v, want false", body["draining"])
	}
	if body["active_jobs"] != float64(0) {
		t.Errorf("active_jobs = %v, want 0", body["active_jobs"])
	}
	if _, ok := body["uptime"]; !ok {
		t.Error("expected uptime field in response")
	}
}

func TestHandleHealthz_WithRunningJobs(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, Log: testLogger()})
	s.running[123] = &runningJob{repo: "test", startedAt: time.Now()}
	s.running[456] = &runningJob{repo: "test", startedAt: time.Now()}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.handleHealthz(w, req)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if body["active_jobs"] != float64(2) {
		t.Errorf("active_jobs = %v, want 2", body["active_jobs"])
	}
}

func TestHandleHealthz_Draining(t *testing.T) {
	s := New(Config{Log: testLogger()})
	s.draining = true

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.handleHealthz(w, req)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if body["draining"] != true {
		t.Errorf("draining = %v, want true", body["draining"])
	}
}

// --- SocketPath tests ---

func TestSocketPath(t *testing.T) {
	path := SocketPath("/var/lib/ephemerd")
	if path == "" {
		t.Fatal("SocketPath returned empty string")
	}
	if path != "/var/lib/ephemerd/ephemerd.sock" {
		t.Logf("SocketPath = %q (OS-specific separator)", path)
	}
}

// --- seenTTL constant test ---

func TestSeenTTL(t *testing.T) {
	if seenTTL != 10*time.Minute {
		t.Errorf("seenTTL = %v, want 10m", seenTTL)
	}
}

// --- backoffDuration tests ---

func TestBackoffDuration_ExponentialSequence(t *testing.T) {
	repo := "test-backoff-sequence"
	// Reset any prior state
	resetBackoff(repo)

	// Each call increments the failure count: 2^1, 2^2, 2^3, ...
	expected := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second, // capped at 60s
		60 * time.Second, // stays capped
	}

	for i, want := range expected {
		got := backoffDuration(repo)
		if got != want {
			t.Errorf("call %d: backoffDuration(%q) = %v, want %v", i+1, repo, got, want)
		}
	}

	// Clean up
	resetBackoff(repo)
}

func TestBackoffDuration_IndependentRepos(t *testing.T) {
	resetBackoff("repo-a")
	resetBackoff("repo-b")

	// Advance repo-a 3 times
	backoffDuration("repo-a")
	backoffDuration("repo-a")
	d3 := backoffDuration("repo-a") // 3rd call → 2^3 = 8s

	// repo-b should start fresh
	d1 := backoffDuration("repo-b") // 1st call → 2^1 = 2s

	if d3 != 8*time.Second {
		t.Errorf("repo-a call 3: got %v, want 8s", d3)
	}
	if d1 != 2*time.Second {
		t.Errorf("repo-b call 1: got %v, want 2s", d1)
	}

	resetBackoff("repo-a")
	resetBackoff("repo-b")
}

func TestResetBackoff(t *testing.T) {
	repo := "test-reset"
	resetBackoff(repo)

	// Build up some backoff
	backoffDuration(repo) // 2s
	backoffDuration(repo) // 4s
	backoffDuration(repo) // 8s

	// Reset
	resetBackoff(repo)

	// Should restart from 2s
	got := backoffDuration(repo)
	if got != 2*time.Second {
		t.Errorf("after reset: backoffDuration = %v, want 2s", got)
	}

	resetBackoff(repo)
}

func TestResetBackoff_NonexistentRepo(t *testing.T) {
	// Should not panic
	resetBackoff("never-seen-before")
}
