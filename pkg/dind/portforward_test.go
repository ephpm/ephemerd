//go:build linux

package dind

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestForwardConn_Bidirectional verifies that forwardConn proxies bytes in
// both directions between an accepted connection and a freshly dialed target.
// The two directions are independent — closing one half should let the
// goroutine still drain the other.
func TestForwardConn_Bidirectional(t *testing.T) {
	// Set up a target server that echoes what the client says then sends an
	// extra "BYE" before closing.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer func() { _ = target.Close() }()

	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		_, _ = c.Write(buf[:n])
		_, _ = c.Write([]byte("BYE"))
	}()

	// Use net.Pipe to simulate the client side of an Accept that forwardConn
	// would normally see. forwardConn(client, target) dials target itself.
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan struct{})
	go func() {
		forwardConn(server, target.Addr().String())
		close(done)
	}()

	// Write "HI" through the client side. forwardConn relays it to target.
	if _, err := client.Write([]byte("HI")); err != nil {
		t.Fatalf("write client: %v", err)
	}

	// Read the echo + BYE back.
	buf := make([]byte, 8)
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	var got bytes.Buffer
	for got.Len() < len("HIBYE") {
		n, err := client.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if got.String() != "HIBYE" {
		t.Errorf("got %q, want HIBYE", got.String())
	}

	// Close client; forwardConn should return shortly.
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("forwardConn did not return after client close")
	}
}

// TestForwardConn_TargetUnreachable ensures forwardConn closes the client
// connection cleanly when the target cannot be dialed, rather than leaking
// the goroutine or panicking.
func TestForwardConn_TargetUnreachable(t *testing.T) {
	client, server := net.Pipe()

	done := make(chan struct{})
	go func() {
		forwardConn(server, "127.0.0.1:1") // port 1 is reserved → ECONNREFUSED
		close(done)
	}()

	// Reading from client should see the close once forwardConn gives up.
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err := client.Read(buf)
	if err == nil {
		t.Error("expected error reading from client after target fails")
	}
	// Either io.EOF or io.ErrClosedPipe is acceptable: forwardConn closed
	// `server` after the target dial failed, which propagates to client as
	// either of those depending on timing. Any *other* error is unexpected.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("unexpected error from client.Read: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("forwardConn did not return after dial failure")
	}
	_ = client.Close()
}

// TestStartPortForwardProxy_NoNetns verifies that the proxy returns an error
// when called without a netns path — guarding against misconfiguration where
// the dind server hasn't been told about the runner's namespace yet.
func TestStartPortForwardProxy_NoNetns(t *testing.T) {
	stop, err := startPortForwardProxy("", "127.0.0.1", "1234", "10.0.0.1", "80")
	if err == nil {
		if stop != nil {
			stop()
		}
		t.Fatal("expected error for empty netns path")
	}
}

// TestStartPortForwardProxy_InvalidNetns confirms a bad netns path produces
// an error rather than a partially-initialized proxy.
func TestStartPortForwardProxy_InvalidNetns(t *testing.T) {
	stop, err := startPortForwardProxy("/proc/self/ns/nonexistent", "127.0.0.1", "1234", "10.0.0.1", "80")
	if err == nil {
		if stop != nil {
			stop()
		}
		t.Fatal("expected error for invalid netns path")
	}
}

// TestStartPortForwardProxy_OwnNetns exercises the full setns + listen + dial
// path against the current process's own netns. We can't truly switch into a
// different namespace as a non-root test, but pointing setns at our own ns is
// a no-op that still exercises the listener + accept + forwardConn flow.
func TestStartPortForwardProxy_OwnNetns(t *testing.T) {
	// Target server that responds with a fixed greeting.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer func() { _ = target.Close() }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write([]byte("ok"))
	}()

	// Pick an arbitrary free port for the proxy to listen on.
	freePort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("picking free port: %v", err)
	}
	addr := freePort.Addr().(*net.TCPAddr)
	if err := freePort.Close(); err != nil {
		t.Fatalf("close free-port stash: %v", err)
	}

	targetAddr := target.Addr().(*net.TCPAddr)
	stop, err := startPortForwardProxy(
		"/proc/self/ns/net",
		"127.0.0.1", itoa(addr.Port),
		"127.0.0.1", itoa(targetAddr.Port),
	)
	if err != nil {
		t.Skipf("setns into own ns failed (likely sandbox restriction): %v", err)
	}
	defer stop()

	// Dial the proxy and verify we receive the target's greeting.
	conn, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 8)
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "ok" {
		t.Errorf("got %q, want ok", buf[:n])
	}
	wg.Wait()

	// stop() should be idempotent.
	stop()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
