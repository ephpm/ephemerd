package pkgcache

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestFetcher(t *testing.T, maxBytes int64) *Fetcher {
	t.Helper()
	c := newTestCache(t, t.TempDir(), maxBytes)
	return NewFetcher(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestDecideFreshness(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name      string
		cached    bool
		immutable bool
		age       time.Duration
		ttl       time.Duration
		want      freshness
	}{
		{"cold", false, false, 0, time.Minute, fetchFresh},
		{"cold immutable", false, true, 0, 0, fetchFresh},
		{"immutable never expires", true, true, 10000 * time.Hour, time.Minute, serveCached},
		{"mutable within ttl", true, false, 30 * time.Second, time.Minute, serveCached},
		{"mutable past ttl", true, false, 2 * time.Minute, time.Minute, revalidate},
		{"mutable zero ttl always revalidates", true, false, 0, 0, revalidate},
		{"mutable negative ttl always revalidates", true, false, 0, -time.Second, revalidate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decide(tc.cached, tc.immutable, now.Add(-tc.age), now, tc.ttl)
			if got != tc.want {
				t.Errorf("decide = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDocumentCachesAndRevalidates(t *testing.T) {
	t.Parallel()
	var hits, conditional atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	req := Request{Key: "listing/x", URL: upstream.URL + "/x", TTL: time.Hour}

	// Miss.
	body, meta, err := f.Document(t.Context(), req)
	if err != nil {
		t.Fatalf("first Document: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if meta.ETag != `"v1"` {
		t.Errorf("ETag = %q, want %q", meta.ETag, `"v1"`)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}

	// Hit within TTL: no network at all.
	if _, _, err := f.Document(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("a within-TTL hit went upstream: hits = %d", hits.Load())
	}

	// Past TTL: one conditional GET, answered 304, body still served.
	req.TTL = -time.Second
	body, _, err = f.Document(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("revalidated body = %q", body)
	}
	if conditional.Load() != 1 {
		t.Errorf("conditional requests = %d, want 1", conditional.Load())
	}
}

// TestDocumentServesStaleOnUpstreamFailure is fail-open layer one: a
// registry that starts erroring must not take the cached answer away.
func TestDocumentServesStaleOnUpstreamFailure(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "good")
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	req := Request{Key: "listing/x", URL: upstream.URL + "/x", TTL: -time.Second} // always revalidate

	if _, _, err := f.Document(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	body, _, err := f.Document(t.Context(), req)
	if err != nil {
		t.Fatalf("a 5xx upstream must serve the stale copy, got %v", err)
	}
	if string(body) != "good" {
		t.Errorf("stale body = %q, want %q", body, "good")
	}

	// Same again with the upstream entirely gone.
	upstream.Close()
	body, _, err = f.Document(t.Context(), req)
	if err != nil {
		t.Fatalf("an unreachable upstream must serve the stale copy, got %v", err)
	}
	if string(body) != "good" {
		t.Errorf("stale body = %q, want %q", body, "good")
	}
}

func TestDocumentColdUpstreamFailureIsAnError(t *testing.T) {
	t.Parallel()
	f := newTestFetcher(t, 0)
	// Nothing cached and nothing listening: the caller must be told, so it
	// can fail open with a redirect to the origin.
	_, _, err := f.Document(t.Context(), Request{Key: "listing/x", URL: "http://127.0.0.1:1/x"})
	if err == nil {
		t.Fatal("expected an error with an empty cache and a dead upstream")
	}
	if IsNotFound(err) {
		t.Error("a connection failure must not be reported as a 404")
	}
}

func TestDocumentPassesThrough404(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	_, _, err := f.Document(t.Context(), Request{Key: "listing/x", URL: upstream.URL + "/x"})
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want a NotFoundError: a missing package is a real answer", err)
	}
}

func TestServeArtifactCachesAndReserves(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("tarball-bytes", 5000)
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	req := Request{Key: "dl/aa/bb/x", URL: upstream.URL + "/x", Immutable: true}

	for i := range 3 {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		if err := f.ServeArtifact(rec, r, req); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, rec.Code)
		}
		if rec.Body.String() != payload {
			t.Fatalf("attempt %d: body mismatch (%d bytes)", i, rec.Body.Len())
		}
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1: the artifact was not cached", hits.Load())
	}
	if f.Cache.Len() != 1 {
		t.Errorf("cache entries = %d, want 1", f.Cache.Len())
	}
}

// TestServeArtifactWritesNothingBeforeUpstreamIsGood is what makes the
// caller's redirect fail-open possible: an error must leave the response
// untouched so a 307 can still be written.
func TestServeArtifactWritesNothingBeforeUpstreamIsGood(t *testing.T) {
	t.Parallel()
	f := newTestFetcher(t, 0)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/whatever", nil)

	err := f.ServeArtifact(rec, r, Request{Key: "dl/aa/bb/x", URL: "http://127.0.0.1:1/x", Immutable: true})
	if err == nil {
		t.Fatal("expected an error from a dead upstream")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes before knowing the upstream was good", rec.Body.Len())
	}
	if len(rec.Result().Header) != 0 {
		t.Errorf("set headers before knowing the upstream was good: %v", rec.Result().Header)
	}
	if f.Cache.Len() != 0 {
		t.Error("a failed fetch created a cache entry")
	}
}

// TestServeArtifactDoesNotCacheTruncatedBodies guards the worst failure a
// pull-through cache can have: permanently serving a half-downloaded
// tarball to every future job.
func TestServeArtifactDoesNotCacheTruncatedBodies(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 10))
		// Close without sending the rest.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	_ = f.ServeArtifact(rec, r, Request{Key: "dl/aa/bb/x", URL: upstream.URL + "/x", Immutable: true})

	if f.Cache.Len() != 0 {
		t.Error("a truncated upstream body was cached")
	}
}

func TestServeArtifactCollapsesConcurrentMisses(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "payload")
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	req := Request{Key: "dl/aa/bb/x", URL: upstream.URL + "/x", Immutable: true}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
			if err := f.ServeArtifact(rec, r, req); err != nil {
				t.Errorf("ServeArtifact: %v", err)
			}
			if rec.Body.String() != "payload" {
				t.Errorf("body = %q", rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1: concurrent misses were not collapsed", hits.Load())
	}
}

// TestServeArtifactEvictsUnderBudget is the end-to-end disk bound: pulling
// far more artifact bytes than the budget must not grow the cache past it.
func TestServeArtifactEvictsUnderBudget(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 8192)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	const budget = 10 * 8192
	f := newTestFetcher(t, budget)
	for i := range 40 {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		req := Request{
			Key:       ArtifactKey(upstream.URL + "/" + string(rune('a'+i%26)) + string(rune('a'+i/26))),
			URL:       upstream.URL + "/x",
			Immutable: true,
		}
		if err := f.ServeArtifact(rec, r, req); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if f.Cache.Bytes() > budget {
		t.Errorf("cache grew to %d bytes, past its %d byte budget", f.Cache.Bytes(), budget)
	}
	if f.Cache.Len() == 0 {
		t.Error("eviction emptied the cache entirely")
	}
}

func TestServeArtifactHeadDoesNotSpendBytes(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "payload")
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodHead, "/whatever", nil)
	if err := f.ServeArtifact(rec, r, Request{Key: "dl/aa/bb/x", URL: upstream.URL + "/x", Immutable: true}); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a body of %d bytes", rec.Body.Len())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d", rec.Code)
	}
}

// TestFetcherForwardsNoCredentials pins the rule that keeps a shared,
// node-wide cache from leaking one job's secrets into another's downloads.
func TestFetcherForwardsNoCredentials(t *testing.T) {
	t.Parallel()
	var sawAuth, sawCookie atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		if r.Header.Get("Cookie") != "" {
			sawCookie.Store(true)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	f := newTestFetcher(t, 0)
	ctx := context.Background()
	if _, _, err := f.Document(ctx, Request{Key: "listing/x", URL: upstream.URL + "/x"}); err != nil {
		t.Fatal(err)
	}
	if sawAuth.Load() || sawCookie.Load() {
		t.Error("credentials reached the upstream registry")
	}
}
