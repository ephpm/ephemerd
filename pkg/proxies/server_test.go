package proxies

import (
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func startTestServer(t *testing.T, h http.Handler) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := NewServer("test", ln, h, testLogger())
	s.Serve()
	return s, "http://" + ln.Addr().String()
}

// TestShutdown_NotDelayedByUnreadConnections is the regression guard for the
// bug that stalled the cargo proxy work.
//
// Go's http.Transport dials speculatively: fire a burst of parallel requests
// and it opens more connections than it needs, leaving spares in its idle
// pool that never sent a byte. Server-side those are ConnState "new", and
// http.Server.Shutdown will not close a "new" connection until it has been in
// that state for over five seconds. A caller with a five-second deadline
// therefore gets context.DeadlineExceeded rather than a clean stop — which is
// exactly what CI reported, at 5.01s.
//
// Here the unread connection is created directly rather than hoped for from
// the transport's dial races, so the test fails deterministically against the
// old behaviour instead of one run in three.
func TestShutdown_NotDelayedByUnreadConnections(t *testing.T) {
	s, base := startTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	// One real request so the server is definitely serving.
	resp, err := http.Get(base)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Now the pathological case: connections that are accepted and then say
	// nothing at all.
	for range 4 {
		c, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = c.Close() }()
	}
	// Give the accept loop a moment to register them as ConnState "new".
	waitUntil(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.unread) >= 4
	}, "connections to be accepted")

	start := time.Now()
	if err := s.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v with 4 unread connections; want well under net/http's 5s 'new' grace", elapsed)
	}
}

// TestShutdown_ForcesAfterGraceAndCancelsRequests: a handler that never
// returns must not be able to hold the daemon open. After the grace window
// Shutdown cancels the base context (which every request context descends
// from) and closes the connections, and it still returns nil — the server IS
// stopped, so callers should not log it as a failure to stop.
func TestShutdown_ForcesAfterGraceAndCancelsRequests(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once

	s, base := startTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		close(cancelled)
	}))

	go func() {
		resp, err := http.Get(base)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	<-entered

	start := time.Now()
	if err := s.Shutdown(200 * time.Millisecond); err != nil {
		t.Fatalf("Shutdown returned %v; forcing a stuck handler closed is not a failure to stop", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v; it is not bounded by the grace window", elapsed)
	}

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the request context was never cancelled; an in-flight upstream fetch would run to its own timeout")
	}
	if s.Context().Err() == nil {
		t.Error("the proxy context is still live after Shutdown")
	}
}

// TestShutdown_IsIdempotent: Stop is reachable from a defer in main and from
// a test cleanup, and both may run.
func TestShutdown_IsIdempotent(t *testing.T) {
	s, _ := startTestServer(t, http.NotFoundHandler())
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestServe_GoroutineIsJoined: Shutdown must not leave the accept loop
// running, or a daemon restart races its own listener.
func TestServe_GoroutineIsJoined(t *testing.T) {
	s, _ := startTestServer(t, http.NotFoundHandler())
	done := make(chan struct{})
	go func() {
		_ = s.Shutdown(time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned; the accept goroutine was not joined")
	}
	// accepting.Wait() has returned, so a second Wait is instant.
	s.accepting.Wait()
}

// TestProbeAddr_WildcardBecomesLoopback: a wildcard binding reports
// "[::]:port", which nothing can dial. Health probes must get an address that
// actually works, or a proxy on the wildcard fallback would report itself
// dead forever.
func TestProbeAddr_WildcardBecomesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := NewServer("test", ln, http.NotFoundHandler(), testLogger())
	s.Serve()
	defer func() { _ = s.Shutdown(time.Second) }()

	host, _, err := net.SplitHostPort(s.ProbeAddr())
	if err != nil {
		t.Fatalf("ProbeAddr() = %q: %v", s.ProbeAddr(), err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("ProbeAddr() host = %q, want a loopback address for a wildcard binding", host)
	}

	// And it must be dialable, which "[::]:port" is not.
	c, err := net.DialTimeout("tcp", s.ProbeAddr(), 5*time.Second)
	if err != nil {
		t.Fatalf("dialing ProbeAddr(): %v", err)
	}
	_ = c.Close()
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
