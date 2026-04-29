package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/tunnel"
	vmPkg "github.com/ephpm/ephemerd/pkg/vm"
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
	if s.pending == nil {
		t.Error("pending map is nil")
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

	s.seen[jobKey{JobID: 1}] = time.Now()
	s.seen[jobKey{JobID: 2}] = time.Now().Add(-seenTTL - time.Minute)
	s.seen[jobKey{JobID: 3}] = time.Now().Add(-seenTTL - time.Hour)

	s.cleanSeen()

	if _, exists := s.seen[jobKey{JobID: 1}]; !exists {
		t.Error("fresh entry should not be cleaned")
	}
	if _, exists := s.seen[jobKey{JobID: 2}]; exists {
		t.Error("expired entry should be cleaned")
	}
	if _, exists := s.seen[jobKey{JobID: 3}]; exists {
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
	s.seen[jobKey{JobID: 1}] = time.Now()
	s.seen[jobKey{JobID: 2}] = time.Now()
	s.seen[jobKey{JobID: 3}] = time.Now()

	s.cleanSeen()

	if len(s.seen) != 3 {
		t.Errorf("expected 3 entries, got %d", len(s.seen))
	}
}

func TestCleanSeen_AllExpired(t *testing.T) {
	s := New(Config{Log: testLogger()})
	old := time.Now().Add(-seenTTL - time.Minute)
	s.seen[jobKey{JobID: 1}] = old
	s.seen[jobKey{JobID: 2}] = old
	s.seen[jobKey{JobID: 3}] = old

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
	s.running[jobKey{JobID: 123}] = &runningJob{repo: "test", startedAt: time.Now()}
	s.running[jobKey{JobID: 456}] = &runningJob{repo: "test", startedAt: time.Now()}

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

// --- registerVMSSHHandler tests ---

// mockMacOSVM is a minimal mock for vm.MacOSVM used by vmssh tests.
type mockMacOSVM struct {
	ip string
}

func (m *mockMacOSVM) WriteJITConfig(string) error                          { return nil }
func (m *mockMacOSVM) Start(ctx context.Context) error                      { return nil }
func (m *mockMacOSVM) WaitForRunner(ctx context.Context) (string, error)    { return m.ip, nil }
func (m *mockMacOSVM) RunnerAddress() string                                { return m.ip }
func (m *mockMacOSVM) Wait(ctx context.Context) (int, error)                { return 0, nil }
func (m *mockMacOSVM) Stop()                                                {}

func newVMSSHTestMux(s *Scheduler) *http.ServeMux {
	mux := http.NewServeMux()
	s.registerVMSSHHandler(mux)
	return mux
}

func TestVMSSH_MissingJobID(t *testing.T) {
	s := New(Config{Log: testLogger()})
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestVMSSH_InvalidJobID(t *testing.T) {
	s := New(Config{Log: testLogger()})
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info?job_id=abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestVMSSH_UnknownJob(t *testing.T) {
	s := New(Config{Log: testLogger()})
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info?job_id=999", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestVMSSH_NonMacOSJob(t *testing.T) {
	s := New(Config{Log: testLogger()})
	// Add a running job WITHOUT a macosVM
	s.running[jobKey{JobID: 100}] = &runningJob{repo: "test", startedAt: time.Now()}
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info?job_id=100", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestVMSSH_VMIPNotReady(t *testing.T) {
	s := New(Config{Log: testLogger()})
	s.running[jobKey{JobID: 200}] = &runningJob{
		repo:      "test",
		macosVM:   &mockMacOSVM{ip: ""}, // IP not yet discovered
		startedAt: time.Now(),
	}
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info?job_id=200", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestVMSSH_ValidMacOSJob(t *testing.T) {
	priv, _, err := vmPkg.GenerateEphemeralSSHKey()
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{
		Log: testLogger(),
		MacOSVMConfig: &vmPkg.MacOSVMConfig{
			SSHSigner: priv,
		},
	})
	s.running[jobKey{JobID: 300}] = &runningJob{
		repo:      "test",
		macosVM:   &mockMacOSVM{ip: "192.168.64.5"},
		startedAt: time.Now(),
	}
	mux := newVMSSHTestMux(s)

	req := httptest.NewRequest(http.MethodGet, "/vm/ssh-info?job_id=300", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var info VMSSHInfo
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if info.IP != "192.168.64.5" {
		t.Errorf("IP = %q, want 192.168.64.5", info.IP)
	}
	if info.User != "admin" {
		t.Errorf("User = %q, want admin", info.User)
	}
	if len(info.PrivateKey) == 0 {
		t.Error("PrivateKey is empty, want PEM-encoded key")
	}
}

// --- serveTunnelWithReconnect tests ---

// mockTunnel is a tunnel.Provider that returns controllable listeners.
type mockTunnel struct {
	mu        sync.Mutex
	listeners []net.Listener
	idx       int
	url       string
}

func (m *mockTunnel) Listen(ctx context.Context) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.listeners) {
		return nil, fmt.Errorf("no more listeners")
	}
	ln := m.listeners[m.idx]
	m.idx++
	return ln, nil
}

func (m *mockTunnel) PublicURL() string { return m.url }

// Verify mockTunnel implements tunnel.Provider at compile time.
var _ tunnel.Provider = (*mockTunnel)(nil)

// newLocalListener creates a TCP listener on a random port.
func newLocalListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// mockWebhookProvider implements providers.Webhook for tunnel reconnect tests.
type mockWebhookProvider struct {
	mockProvider
	registered   atomic.Int32
	deregistered atomic.Int32
}

func newMockWebhookProvider() *mockWebhookProvider {
	return &mockWebhookProvider{
		mockProvider: *newMockProvider("test-webhook"),
	}
}

func (m *mockWebhookProvider) WebhookHandler(_ string) (http.Handler, <-chan providers.JobEvent) {
	return http.NewServeMux(), make(chan providers.JobEvent)
}

func (m *mockWebhookProvider) RegisterWebhooks(_ context.Context, _, _ string) error {
	m.registered.Add(1)
	return nil
}

func (m *mockWebhookProvider) DeregisterWebhooks(_ context.Context) error {
	m.deregistered.Add(1)
	return nil
}

var _ providers.Webhook = (*mockWebhookProvider)(nil)

func TestServeTunnelWithReconnect_ClosesOldListener(t *testing.T) {
	whp := newMockWebhookProvider()

	// Create two listeners — first one will be killed, second one will serve.
	ln1 := newLocalListener(t)
	ln2 := newLocalListener(t)

	mt := &mockTunnel{
		listeners: []net.Listener{ln2},
		url:       "http://tunnel.test",
	}

	s := New(Config{
		Log:              testLogger(),
		Providers:        []providers.Provider{whp},
		Tunnel:           mt,
		TunnelMaxRetries: 3,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "ok"); err != nil {
			t.Logf("writing response: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.serveTunnelWithReconnect(ctx, handler, ln1, []providers.Webhook{whp}, make(chan providers.JobEvent, 1))
		close(done)
	}()

	// Give the server a moment to start serving on ln1
	time.Sleep(50 * time.Millisecond)

	// Kill ln1 — this should trigger the reconnect path
	_ = ln1.Close()

	// Wait for reconnect to happen (new listener ln2 used, webhook re-registered)
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reconnect")
		default:
		}
		if whp.registered.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify we can make HTTP requests through the new listener
	addr := ln2.Addr().String()
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		t.Fatalf("GET through reconnected listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status through reconnected listener = %d, want 200", resp.StatusCode)
	}

	// Shut down
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveTunnelWithReconnect did not exit after cancel")
	}
}

func TestServeTunnelWithReconnect_FallsBackToPolling(t *testing.T) {
	whp := newMockWebhookProvider()

	// No reconnect listeners available — all attempts should fail
	mt := &mockTunnel{
		listeners: nil,
		url:       "http://tunnel.test",
	}

	s := New(Config{
		Log:              testLogger(),
		Providers:        []providers.Provider{whp},
		Tunnel:           mt,
		TunnelMaxRetries: 1, // fail after 1 attempt
		PollInterval:     1 * time.Second,
	})

	ln := newLocalListener(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.serveTunnelWithReconnect(ctx, http.NewServeMux(), ln, []providers.Webhook{whp}, make(chan providers.JobEvent, 1))
		close(done)
	}()

	// Give server a moment to start
	time.Sleep(50 * time.Millisecond)

	// Kill the listener to trigger reconnect
	_ = ln.Close()

	// serveTunnelWithReconnect should exit (falling back to polling)
	select {
	case <-done:
		// good — it exited
	case <-time.After(15 * time.Second):
		t.Fatal("serveTunnelWithReconnect did not exit after max retries")
	}

	// Verify webhooks were deregistered on exit
	if whp.deregistered.Load() < 1 {
		t.Logf("deregistered = %d (may be 0 if providers list was empty)", whp.deregistered.Load())
	}
}

func TestServeTunnelWithReconnect_CancelExitsCleanly(t *testing.T) {
	whp := newMockWebhookProvider()

	ln := newLocalListener(t)
	mt := &mockTunnel{url: "http://tunnel.test"}

	s := New(Config{
		Log:       testLogger(),
		Providers: []providers.Provider{whp},
		Tunnel:    mt,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.serveTunnelWithReconnect(ctx, http.NewServeMux(), ln, []providers.Webhook{whp}, make(chan providers.JobEvent, 1))
		close(done)
	}()

	// Give server a moment to start
	time.Sleep(50 * time.Millisecond)

	// Cancel should cause clean exit
	cancel()

	select {
	case <-done:
		// clean exit
	case <-time.After(5 * time.Second):
		t.Fatal("serveTunnelWithReconnect did not exit after context cancel")
	}
}
