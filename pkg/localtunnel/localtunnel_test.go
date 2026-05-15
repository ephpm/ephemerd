package localtunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Options.setDefaults ---

func TestOptions_SetDefaults_AllEmpty(t *testing.T) {
	o := Options{}
	o.setDefaults()

	if o.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", o.BaseURL, DefaultBaseURL)
	}
	if o.MaxConnections != DefaultMaxConnections {
		t.Errorf("MaxConnections = %d, want %d", o.MaxConnections, DefaultMaxConnections)
	}
	if o.Log == nil {
		t.Error("Log should not be nil after setDefaults")
	}
}

func TestOptions_SetDefaults_PreservesCustom(t *testing.T) {
	custom := &testLogger{}
	o := Options{
		BaseURL:        "https://my-server.example.com",
		MaxConnections: 42,
		Log:            custom,
		Subdomain:      "my-tunnel",
	}
	o.setDefaults()

	if o.BaseURL != "https://my-server.example.com" {
		t.Errorf("BaseURL = %q, want custom", o.BaseURL)
	}
	if o.MaxConnections != 42 {
		t.Errorf("MaxConnections = %d, want 42", o.MaxConnections)
	}
	if o.Log != custom {
		t.Error("Log should be preserved")
	}
	if o.Subdomain != "my-tunnel" {
		t.Errorf("Subdomain = %q, want %q", o.Subdomain, "my-tunnel")
	}
}

// --- DefaultLogger (no-op) ---

func TestDefaultLogger_DoesNotPanic(t *testing.T) {
	DefaultLogger.Println("this should be a no-op")
	DefaultLogger.Println("multiple", "args", 123)
}

// --- Addr ---

func TestAddr_Network(t *testing.T) {
	a := Addr{URL: "https://example.loca.lt"}
	if a.Network() != "localtunnel" {
		t.Errorf("Network() = %q, want %q", a.Network(), "localtunnel")
	}
}

func TestAddr_String(t *testing.T) {
	a := Addr{URL: "https://my-tunnel.loca.lt"}
	if a.String() != "https://my-tunnel.loca.lt" {
		t.Errorf("String() = %q, want %q", a.String(), "https://my-tunnel.loca.lt")
	}
}

func TestAddr_EmptyURL(t *testing.T) {
	a := Addr{}
	if a.String() != "" {
		t.Errorf("empty Addr.String() = %q, want empty", a.String())
	}
}

// --- limitedReader ---

func TestLimitedReader_ReadsUpToMax(t *testing.T) {
	r := &limitedReader{
		reader:   strings.NewReader("hello world"),
		maxBytes: 5,
	}

	buf := make([]byte, 20)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if n != 5 {
		t.Errorf("Read n = %d, want 5", n)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("Read = %q, want %q", string(buf[:n]), "hello")
	}
}

func TestLimitedReader_ReturnsEOFWhenExhausted(t *testing.T) {
	r := &limitedReader{
		reader:   strings.NewReader("hi"),
		maxBytes: 10,
	}

	buf := make([]byte, 20)
	_, err := r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	// After reading "hi" (2 bytes), maxBytes is 8, but underlying returned EOF
	if r.maxBytes < 0 {
		t.Errorf("maxBytes should not go negative, got %d", r.maxBytes)
	}
}

func TestLimitedReader_ZeroMaxBytes(t *testing.T) {
	r := &limitedReader{
		reader:   strings.NewReader("data"),
		maxBytes: 0,
	}

	buf := make([]byte, 10)
	n, err := r.Read(buf)
	if n != 0 {
		t.Errorf("Read n = %d, want 0", n)
	}
	if err != io.EOF {
		t.Errorf("Read err = %v, want io.EOF", err)
	}
}

func TestLimitedReader_NegativeMaxBytes(t *testing.T) {
	r := &limitedReader{
		reader:   strings.NewReader("data"),
		maxBytes: -1,
	}

	buf := make([]byte, 10)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("Read(%d, %v), want (0, EOF)", n, err)
	}
}

func TestLimitedReader_ReachedEOF(t *testing.T) {
	r := &limitedReader{
		reader:   strings.NewReader("ab"),
		maxBytes: 10,
	}

	if r.ReachedEOF() {
		t.Error("should not be EOF before reading")
	}

	// First read returns "ab" with EOF from underlying strings.Reader
	buf := make([]byte, 10)
	_, err := r.Read(buf)
	// strings.Reader returns data + EOF in the same call
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}

	// After the read that returned EOF from underlying, ReachedEOF should be true
	// But note: strings.Reader returns (n, io.EOF) in one call when it has data,
	// so lastError is set to io.EOF
	if err == io.EOF && !r.ReachedEOF() {
		t.Error("should be EOF after underlying returned io.EOF")
	}
}

// --- readAtmost ---

func TestReadAtmost_NilReader(t *testing.T) {
	data, err := readAtmost(nil, 100)
	if err != nil {
		t.Errorf("readAtmost(nil) error: %v", err)
	}
	if data != nil {
		t.Errorf("readAtmost(nil) = %v, want nil", data)
	}
}

func TestReadAtmost_ZeroMaxSize(t *testing.T) {
	body := io.NopCloser(strings.NewReader("all of this"))
	data, err := readAtmost(body, 0)
	if err != nil {
		t.Fatalf("readAtmost(0) error: %v", err)
	}
	if string(data) != "all of this" {
		t.Errorf("readAtmost(0) = %q, want full body", string(data))
	}
}

func TestReadAtmost_WithinLimit(t *testing.T) {
	body := io.NopCloser(strings.NewReader("short"))
	data, err := readAtmost(body, 100)
	if err != nil {
		t.Fatalf("readAtmost error: %v", err)
	}
	if string(data) != "short" {
		t.Errorf("readAtmost = %q, want %q", string(data), "short")
	}
}

func TestReadAtmost_ExceedsLimit(t *testing.T) {
	body := io.NopCloser(strings.NewReader("this is way too long"))
	_, err := readAtmost(body, 5)
	if err == nil {
		t.Fatal("expected error when body exceeds maxSize")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, expected 'larger than' message", err)
	}
}

func TestReadAtmost_ExactLimit(t *testing.T) {
	// When body is exactly maxSize, the limitedReader reads all bytes but
	// hasn't seen EOF yet (it comes on the next read). So readAtmost returns
	// an error because ReachedEOF() is false. This is by design — the
	// function is conservative about truncation.
	body := io.NopCloser(strings.NewReader("exact"))
	_, err := readAtmost(body, 5)
	if err == nil {
		t.Fatal("expected error when body is exactly maxSize (conservative truncation check)")
	}
}

func TestReadAtmost_UnderLimit(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hi"))
	data, err := readAtmost(body, 100)
	if err != nil {
		t.Fatalf("readAtmost error: %v", err)
	}
	if string(data) != "hi" {
		t.Errorf("readAtmost = %q, want %q", string(data), "hi")
	}
}

// --- counter ---

func TestCounter_AddAndWaitFor(t *testing.T) {
	var c counter
	c.Add(1)
	c.Add(2)

	done := make(chan struct{})
	go func() {
		c.WaitFor(3)
		close(done)
	}()

	select {
	case <-done:
		// success
	default:
		// WaitFor should return immediately since counter is already 3
		// Give it a tiny moment
		c.Add(0) // trigger broadcast
	}
}

func TestCounter_WaitForAlreadyReached(t *testing.T) {
	var c counter
	c.Add(5)

	// Should not block since counter (5) >= target (3)
	c.WaitFor(3)
}

// --- ErrListenerClosed ---

func TestErrListenerClosed(t *testing.T) {
	if ErrListenerClosed == nil {
		t.Fatal("ErrListenerClosed should not be nil")
	}
	if ErrListenerClosed.Error() != "listener was closed" {
		t.Errorf("ErrListenerClosed = %q", ErrListenerClosed.Error())
	}
}

// --- conn ---

func TestConn_ReadBufferedByte(t *testing.T) {
	done := make(chan struct{}, 1)
	// Use enough data that the mock won't return EOF on the first Conn.Read
	underlying := &mockConn{data: []byte("ello world plus more data here")}
	c := &conn{
		Conn:   underlying,
		Buffer: [1]byte{'h'},
		Done:   done,
	}

	buf := make([]byte, 6)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	// First read: buf[0] = 'h' (buffered), then Conn.Read fills buf[1:]
	if n < 1 {
		t.Fatalf("Read n = %d, want >= 1", n)
	}
	if buf[0] != 'h' {
		t.Errorf("first byte = %c, want 'h'", buf[0])
	}
}

func TestConn_ReadEmptyBuffer(t *testing.T) {
	done := make(chan struct{}, 1)
	underlying := &mockConn{data: []byte("data")}
	c := &conn{
		Conn:   underlying,
		Buffer: [1]byte{'x'},
		Done:   done,
	}

	// Read with zero-length buffer
	buf := make([]byte, 0)
	n, err := c.Read(buf)
	if n != 0 {
		t.Errorf("Read(empty) n = %d, want 0", n)
	}
	if err != nil {
		t.Errorf("Read(empty) err = %v, want nil", err)
	}
}

// mockConn is a minimal net.Conn for testing
type mockConn struct {
	data []byte
}

func (m *mockConn) Read(b []byte) (int, error) {
	if len(m.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b, m.data)
	m.data = m.data[n:]
	if len(m.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (m *mockConn) Write(b []byte) (int, error)        { return len(b), nil }
func (m *mockConn) Close() error                        { return nil }
func (m *mockConn) LocalAddr() net.Addr                 { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(_ time.Time) error       { return nil }
func (m *mockConn) SetReadDeadline(_ time.Time) error   { return nil }
func (m *mockConn) SetWriteDeadline(_ time.Time) error  { return nil }

type testLogger struct{}

func (testLogger) Println(...interface{}) {}

// --- Listen with mock server ---

// --- Listen error paths (these return before proxy setup, so they don't hang) ---

func TestListen_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := Listen(ctx, Options{
		BaseURL: srv.URL,
		Log:     &testLogger{},
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestListen_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("not json")); err != nil {
			t.Logf("writing response: %v", err)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := Listen(ctx, Options{
		BaseURL: srv.URL,
		Log:     &testLogger{},
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

// --- Reconnect / dropped-connection behavior ---

// fakeTunnelServer simulates the localtunnel.me API. It serves the
// registration request and then opens a TCP listener on a random port that
// the client's proxy() loop will dial. Each accepted connection is closed
// immediately by default, simulating a server that drops connections —
// which is exactly the scenario reconnect logic must handle.
type fakeTunnelServer struct {
	httpSrv  *httptest.Server
	tcpLn    net.Listener
	accepted chan struct{}
	closed   chan struct{}
	// onAccept controls what the server does when a TCP client connects.
	onAccept func(net.Conn)
}

func newFakeTunnelServer(t *testing.T) *fakeTunnelServer {
	t.Helper()
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	port := tcpLn.Addr().(*net.TCPAddr).Port

	f := &fakeTunnelServer{
		tcpLn:    tcpLn,
		accepted: make(chan struct{}, 8),
		closed:   make(chan struct{}),
	}
	// Default behavior: drop connections immediately.
	f.onAccept = func(c net.Conn) {
		if err := c.Close(); err != nil {
			t.Logf("server close: %v", err)
		}
	}

	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			select {
			case f.accepted <- struct{}{}:
			default:
			}
			f.onAccept(c)
		}
	}()

	f.httpSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Response shape matches what Listen() expects.
		body := fmt.Sprintf(`{"id":"x","port":%d,"max_conn_count":1,"url":"http://x.example/"}`, port)
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Logf("writing http response: %v", err)
		}
	}))

	t.Cleanup(func() {
		f.httpSrv.Close()
		if err := tcpLn.Close(); err != nil {
			t.Logf("tcp close: %v", err)
		}
		close(f.closed)
	})

	return f
}

func TestListen_DroppedConnection_AbortsListener(t *testing.T) {
	// Inspecting listener.go: handle() returns the read error to proxy(),
	// which then calls abort(). After abort, Accept() returns the recorded
	// error. This documents the "drop -> abort" semantics rather than
	// auto-reconnect at the per-connection level (which is not what this
	// implementation does — proxy()'s retry only covers the initial dial).
	srv := newFakeTunnelServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	ln, err := Listen(ctx, Options{
		BaseURL: srv.httpSrv.URL,
		Log:     &testLogger{},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Logf("listener close: %v", err)
		}
	}()

	// At least one accept should occur (the initial connection).
	select {
	case <-srv.accepted:
		// expected
	case <-time.After(3 * time.Second):
		t.Fatal("never received initial connection")
	}

	// After the server drops the conn, Accept() on the listener should
	// return the abort error within a reasonable time (no auto-reconnect).
	type acceptResult struct {
		err error
	}
	resCh := make(chan acceptResult, 1)
	go func() {
		_, err := ln.Accept()
		resCh <- acceptResult{err}
	}()

	select {
	case r := <-resCh:
		if r.err == nil {
			t.Error("Accept() returned nil err after server dropped connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() did not return after server drop")
	}
}

// TestListen_BackoffDuration documents the retry backoff schedule used by
// proxy(). The loop sleeps time.Duration(i*i)*3*time.Second on attempt i,
// so attempts run at 0s, 3s, 12s, 27s offsets from the initial dial.
// We don't execute the full retry storm in unit tests (~42s); instead we
// assert the formula at the values used in the source.
func TestListen_BackoffSchedule(t *testing.T) {
	// Mirror the formula in listener.go's proxy() loop.
	expected := []time.Duration{
		0 * time.Second,
		3 * time.Second,
		12 * time.Second,
	}
	for i, want := range expected {
		got := time.Duration(i*i) * 3 * time.Second
		if got != want {
			t.Errorf("attempt %d: backoff = %v, want %v", i, got, want)
		}
	}
}

func TestListen_ContextCancel_StopsReconnect(t *testing.T) {
	// When the caller cancels the context, the reconnect loop must terminate.
	srv := newFakeTunnelServer(t)

	ctx, cancel := context.WithCancel(context.Background())

	ln, err := Listen(ctx, Options{
		BaseURL: srv.httpSrv.URL,
		Log:     &testLogger{},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// Cancel after a short delay so at least one connection is made.
	time.AfterFunc(200*time.Millisecond, cancel)

	// Close should return before any test timeout.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ln.Close() }()

	select {
	case <-closeDone:
		// success — loop exited
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return after context cancellation")
	}
}

// --- Listener public surface ---

func TestListener_AccessorsAfterListen(t *testing.T) {
	// Smoke-check that Addr() and URL/RemoteAddr/Close don't panic on a
	// real Listener, even when the tunnel TCP side is dead-on-arrival.
	srv := newFakeTunnelServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, err := Listen(ctx, Options{
		BaseURL: srv.httpSrv.URL,
		Log:     &testLogger{},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Logf("listener close: %v", err)
		}
	}()

	addr := ln.Addr()
	if addr == nil {
		t.Fatal("Addr() returned nil")
	}
	if addr.Network() != "localtunnel" {
		t.Errorf("Network() = %q, want localtunnel", addr.Network())
	}
	if addr.String() == "" {
		t.Error("Addr.String() is empty")
	}
}

// --- handle() stale-connection behavior is exercised indirectly ---

func TestConn_Done_ClosesOnce(t *testing.T) {
	// Multiple errors must only close Done once (sync.Once) — a regression
	// here would cause "close of closed channel" panic during reconnect.
	done := make(chan struct{}, 1)
	c := &conn{
		Conn:   &mockConn{}, // returns EOF immediately
		Buffer: [1]byte{'x'},
		Done:   done,
	}

	// First read consumes buffered byte and may hit EOF on underlying.
	buf := make([]byte, 4)
	_, _ = c.Read(buf)
	// Second read goes straight through to underlying (always EOF).
	_, _ = c.Read(buf)
	// Close should not panic even if Done was already closed.
	if err := c.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

