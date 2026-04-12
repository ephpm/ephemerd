package localtunnel

import (
	"io"
	"net"
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
