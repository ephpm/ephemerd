package proxies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultShutdownGrace bounds how long a cache proxy waits for in-flight
// requests to finish before it stops asking nicely and closes them.
const DefaultShutdownGrace = 5 * time.Second

// Server is the HTTP server every cache proxy runs.
//
// It exists because the obvious spelling —
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	err := srv.Shutdown(ctx)
//
// does not reliably stop. Two separate problems:
//
//  1. UNREAD CONNECTIONS. Go's http.Transport dials speculatively: under a
//     burst of parallel requests it opens more connections than it ends up
//     using, and the spares land in the client's idle pool having never sent
//     a single byte. On the server side those are ConnState "new" and stay
//     that way. http.Server.Shutdown will not close a "new" connection until
//     it has sat in that state for more than five seconds (net/http's
//     issue-22682 heuristic), so Shutdown busy-polls for the full five
//     seconds and a caller with a five-second deadline gets
//     context.DeadlineExceeded rather than a clean stop. That is exactly the
//     intermittent CI failure this type was written for:
//     "Stop: shutting down cargo proxy: context deadline exceeded" after
//     5.01s, with a single connection stuck in "new". A connection that has
//     not sent a request has nothing to drain, so Shutdown closes those up
//     front and finishes in microseconds instead.
//
//  2. UNBOUNDED HANDLERS. A handler blocked on a slow upstream keeps its
//     connection active, and http.Server.Shutdown neither cancels request
//     contexts nor gives up on its own. Every request context here derives
//     from Context(), so when the grace window closes, cancelling it unblocks
//     the in-flight upstream fetches, and Close() then drops what is left.
//     Shutdown always returns; the daemon never hangs on a wedged proxy.
//
// Shutdown also joins the accept goroutine, so no goroutine outlives it.
type Server struct {
	name string
	log  *slog.Logger

	http *http.Server
	ln   net.Listener

	// ctx is the proxy's lifetime context. It is the BaseContext of the HTTP
	// server, so every request context descends from it, and it is what
	// proxies hand to their upstream fetches.
	ctx    context.Context
	cancel context.CancelFunc

	accepting sync.WaitGroup

	mu       sync.Mutex
	stopped  bool
	stopping bool
	// unread holds connections that have been accepted but have not yet
	// delivered a request. See the type comment.
	unread map[net.Conn]struct{}
}

// NewServer wires a cache proxy's handler onto an already-bound listener.
// Call Serve to start accepting.
func NewServer(name string, ln net.Listener, h http.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		name:   name,
		log:    log,
		ln:     ln,
		ctx:    ctx,
		cancel: cancel,
		unread: make(map[net.Conn]struct{}),
	}
	s.http = &http.Server{
		Handler: h,
		// A client that connects and never sends a request must not pin a
		// goroutine forever.
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnState:         s.trackConn,
	}
	return s
}

// Context is the proxy's lifetime context. Request contexts descend from it,
// and Shutdown cancels it, so work started on behalf of a request — an
// upstream fetch in particular — unblocks when the proxy stops.
func (s *Server) Context() context.Context { return s.ctx }

// Addr is the address actually bound, which after a wildcard fallback is not
// the address that was requested. See Listen.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve begins accepting connections in the background and returns
// immediately. The goroutine it starts is joined by Shutdown.
func (s *Server) Serve() {
	s.accepting.Add(1)
	go func() {
		defer s.accepting.Done()
		if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("cache proxy server error", "proxy", s.name, "error", err)
		}
	}()
}

// trackConn maintains the set of accepted-but-unread connections.
func (s *Server) trackConn(c net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if state != http.StateNew {
		// The connection has carried a request (or is gone). From here on
		// http.Server.Shutdown handles it correctly on its own.
		delete(s.unread, c)
		return
	}
	if s.stopping {
		// Accepted in the window between "we decided to stop" and the
		// listener actually closing. It will never carry a request.
		_ = c.Close()
		return
	}
	s.unread[c] = struct{}{}
}

// Shutdown stops the server within a bounded time and joins its goroutines.
// It is idempotent.
//
// A non-nil return means the forced close itself failed. Merely having to
// force the shutdown is logged, not returned: the server is stopped either
// way, and callers (including tests) should not treat a slow drain as a
// failure to stop.
func (s *Server) Shutdown(grace time.Duration) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.stopping = true
	// Close the connections that never sent a request, before asking
	// http.Server to drain. Without this, Shutdown sits out net/http's
	// five-second "new" grace for every such connection. See the type
	// comment for the whole story.
	for c := range s.unread {
		_ = c.Close()
	}
	clear(s.unread)
	s.mu.Unlock()

	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	var errs []error
	if err := s.http.Shutdown(ctx); err != nil {
		s.log.Warn("cache proxy did not drain within its grace period; closing connections",
			"proxy", s.name, "grace", grace, "error", err)
		// Cancel first: handlers blocked on an upstream fetch are waiting on
		// a context derived from this one, and they will not notice a closed
		// client connection on their own.
		s.cancel()
		if cerr := s.http.Close(); cerr != nil && !errors.Is(cerr, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("closing %s proxy: %w", s.name, cerr))
		}
	}
	s.cancel()
	s.accepting.Wait()
	return errors.Join(errs...)
}

// ProbeAddr is an address on which this server can be reached from the host
// it runs on. It is NOT the address advertised to containers: after a
// wildcard fallback the bound address is "[::]:8083", which nothing can dial,
// so a wildcard binding is probed on loopback instead.
//
// Health checks must use this rather than the advertised gateway address. On
// Linux the gateway IP does not exist until CNI creates the bridge with the
// first job container, so probing the advertised address would report the
// proxy as dead on every freshly booted daemon — precisely when it is fine.
func (s *Server) ProbeAddr() string {
	addr := s.ln.Addr()
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	if tcp.IP == nil || tcp.IP.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", tcp.Port))
	}
	return addr.String()
}
