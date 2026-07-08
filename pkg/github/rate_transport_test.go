package github

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestRateTrackingTransport_UpdatesFromHeaders verifies the transport
// parses X-RateLimit-* headers and updates the shared snapshot on every
// response — including error responses.
func TestRateTrackingTransport_UpdatesFromHeaders(t *testing.T) {
	resetAt := time.Now().Add(30 * time.Minute).Unix()

	// A synthetic server that returns rate headers on every response
	// (both 200 and 429). We tick the response status through the
	// requests to prove the transport observes both.
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount++
		w.Header().Set("X-RateLimit-Limit", "5000")
		if reqCount == 1 {
			w.Header().Set("X-RateLimit-Remaining", "4321")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
			w.WriteHeader(http.StatusOK)
			return
		}
		// Second request: budget exhausted with headers still present.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var snap rateSnapshot
	tr := &rateTrackingTransport{next: http.DefaultTransport, last: &snap}
	client := &http.Client{Transport: tr}

	// First request: 200 with remaining=4321.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	_ = resp.Body.Close()

	rem, lim, reset, updated := snap.Snapshot()
	if rem != 4321 {
		t.Errorf("remaining = %d, want 4321", rem)
	}
	if lim != 5000 {
		t.Errorf("limit = %d, want 5000", lim)
	}
	if reset.Unix() != resetAt {
		t.Errorf("reset = %v, want unix %d", reset, resetAt)
	}
	if updated.IsZero() {
		t.Error("updated timestamp should be set after a request")
	}

	// Second request: 429 with remaining=0 — the transport must still
	// update the snapshot from headers (this is the whole point: an
	// error response is exactly when we most need fresh data).
	resp, err = client.Get(srv.URL)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	_ = resp.Body.Close()

	rem, _, _, updated2 := snap.Snapshot()
	if rem != 0 {
		t.Errorf("after 429, remaining = %d, want 0", rem)
	}
	if !updated2.After(updated.Add(-time.Second)) {
		t.Errorf("updated should have advanced: prev=%v new=%v", updated, updated2)
	}
}

// TestRateTrackingTransport_NoHeaders_LeavesSnapshotAlone pins that a
// response missing the rate headers doesn't clobber the last known
// value with zero — otherwise a stray endpoint that doesn't emit rate
// headers would zero the gauge and confuse operators.
func TestRateTrackingTransport_NoHeaders_LeavesSnapshotAlone(t *testing.T) {
	var snap rateSnapshot
	snap.remaining.Store(999)
	snap.limit.Store(5000)
	snap.updatedUnix.Store(time.Now().Unix())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No X-RateLimit-* headers at all.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &rateTrackingTransport{next: http.DefaultTransport, last: &snap}
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if got := snap.remaining.Load(); got != 999 {
		t.Errorf("remaining clobbered to %d — should have stayed at 999 when headers absent", got)
	}
}
