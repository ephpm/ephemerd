package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// freePort finds an available TCP port by binding to :0 and releasing it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}
	return port
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestServe_ReturnsMetrics(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// Give the server a moment to start listening
	var resp *http.Response
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, lastErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET /metrics failed after retries: %v", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/openmetrics") {
		t.Errorf("Content-Type = %q, want prometheus text format", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if len(body) == 0 {
		t.Error("response body is empty, expected prometheus metrics")
	}
}

func TestServe_CleanupShutsDownServer(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/metrics",
		Log:  testLogger(),
	})

	// Wait for server to start
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Logf("closing response body: %v", closeErr)
			}
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("server did not start: %v", lastErr)
	}

	// Call cleanup to shut down
	cleanup()

	// Give shutdown a moment to complete
	time.Sleep(100 * time.Millisecond)

	// Server should no longer accept connections
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err == nil {
		_ = resp.Body.Close()
		t.Error("expected connection error after cleanup, but request succeeded")
	}
}

func TestServe_CustomPath(t *testing.T) {
	port := freePort(t)

	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/custom-metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// Wait for server to start
	var resp *http.Response
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, lastErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/custom-metrics", port))
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET /custom-metrics failed: %v", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// The default /metrics path should 404
	resp404, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer func() {
		if err := resp404.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("/metrics status = %d, want %d (should not be registered)", resp404.StatusCode, http.StatusNotFound)
	}
}

// --- Label cardinality tests ---
//
// These tests guard against the classic Prometheus footgun: high-cardinality
// labels like per-run IDs or per-job UUIDs cause unbounded series growth.
// The metrics defined in this package must use only bounded labels (provider,
// repo, status, endpoint, status_code, event_type, reason).

// metricLabels asserts the exact set of label keys for each *Vec metric in
// metrics.go. If any field is renamed or a new label is added (especially
// something unbounded like job_id, run_id, or sha), this test will fail.
//
// Per-run IDs (run_id, job_id, sha) MUST NOT appear in any label set —
// they would cause Prometheus to retain a series per job indefinitely.
var bannedLabels = map[string]struct{}{
	"job_id":   {},
	"run_id":   {},
	"sha":      {},
	"runner":   {}, // runner names are per-job (JIT)
	"jit_id":   {},
	"event_id": {},
}

func TestMetrics_LabelCardinality_JobsTotal(t *testing.T) {
	want := []string{"provider", "repo", "status"}
	assertLabels(t, "ephemerd_jobs_total", JobsTotal.WithLabelValues("github", "owner/r", "success"), want)
}

func TestMetrics_LabelCardinality_JobDuration(t *testing.T) {
	// Histograms expose the same Desc() interface; reuse assertLabels by
	// observing a value first to register the series.
	JobDuration.WithLabelValues("github", "owner/r").Observe(1.0)
	want := []string{"provider", "repo"}
	assertLabels(t, "ephemerd_job_duration_seconds",
		JobDuration.WithLabelValues("github", "owner/r").(interface{ Desc() *prometheus.Desc }),
		want,
	)
}

func TestMetrics_LabelCardinality_JobStartup(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	JobStartup.WithLabelValues("owner/r")
}

func TestMetrics_LabelCardinality_GitHubAPIRequests(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	GitHubAPIRequests.WithLabelValues("/repos", "200")
}

func TestMetrics_LabelCardinality_GitHubWebhookEvents(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	GitHubWebhookEventsTotal.WithLabelValues("workflow_job")
}

func TestMetrics_LabelCardinality_JITRegistrationErrors(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	JITRegistrationErrors.WithLabelValues("owner/r", "rate_limit")
}

func TestMetrics_NoBannedLabelsExposed(t *testing.T) {
	// Trigger a few metrics so they appear in the registry, then scrape /metrics
	// and assert no banned label name appears anywhere in the output.
	JobsTotal.WithLabelValues("github", "owner/r", "success").Inc()
	JobDuration.WithLabelValues("github", "owner/r").Observe(1.0)
	JobStartup.WithLabelValues("owner/r").Observe(1.0)
	GitHubAPIRequests.WithLabelValues("/repos", "200").Inc()
	GitHubWebhookEventsTotal.WithLabelValues("workflow_job").Inc()
	JITRegistrationErrors.WithLabelValues("owner/r", "rate_limit").Inc()

	port := freePort(t)
	cleanup := Serve(context.Background(), ServerConfig{
		Port: port,
		Path: "/metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	var resp *http.Response
	var lastErr error
	for i := 0; i < 20; i++ {
		resp, lastErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("GET /metrics failed: %v", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("closing response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	bodyStr := string(body)
	for banned := range bannedLabels {
		// Match label key form `<banned>=` (Prometheus exposition format).
		needle := banned + "=\""
		if strings.Contains(bodyStr, needle) {
			t.Errorf("banned high-cardinality label %q found in metrics output", banned)
		}
	}
}

// assertLabels confirms a CounterVec/GaugeVec series has exactly the expected
// label keys by inspecting its descriptor. The descriptor format is:
//   Desc{... variableLabels: {a,b,c}}
// so we look for each key as a comma- or brace-bounded substring.
func assertLabels(t *testing.T, name string, m interface{ Desc() *prometheus.Desc }, want []string) {
	t.Helper()
	desc := m.Desc().String()
	// Extract the variableLabels section.
	start := strings.Index(desc, "variableLabels: {")
	if start < 0 {
		t.Fatalf("metric %s descriptor missing variableLabels section: %s", name, desc)
	}
	end := strings.Index(desc[start:], "}")
	if end < 0 {
		t.Fatalf("metric %s descriptor missing closing brace: %s", name, desc)
	}
	labelSection := desc[start+len("variableLabels: {") : start+end]
	got := map[string]struct{}{}
	for _, k := range strings.Split(labelSection, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			got[k] = struct{}{}
		}
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("metric %s missing expected label %q (got %v)", name, k, got)
		}
	}
	for banned := range bannedLabels {
		if _, ok := got[banned]; ok {
			t.Errorf("metric %s exposes banned label %q", name, banned)
		}
	}
	if len(got) != len(want) {
		t.Errorf("metric %s label count = %d, want %d (got %v)", name, len(got), len(want), got)
	}
}

func TestServe_Port0_PicksRandomPort(t *testing.T) {
	// Port 0 should cause the OS to pick a random port.
	// Since Serve() doesn't return the actual port, we can only verify
	// it doesn't panic or error — the server starts in a goroutine.
	cleanup := Serve(context.Background(), ServerConfig{
		Port: 0,
		Path: "/metrics",
		Log:  testLogger(),
	})
	defer cleanup()

	// If we got here without panic, the server accepted port 0.
	// We can't easily connect since we don't know the actual port,
	// but we verify cleanup works without error.
	cleanup()
}
