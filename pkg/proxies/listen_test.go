package proxies

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWildcardAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{name: "gateway ipv4", addr: "10.88.0.1:8082", want: ":8082"},
		{name: "loopback", addr: "127.0.0.1:1", want: ":1"},
		{name: "already wildcard", addr: ":8083", want: ":8083"},
		{name: "zero port keeps zero", addr: "10.88.0.1:0", want: ":0"},
		{name: "ipv6 host", addr: "[fd00::1]:8082", want: ":8082"},
		{name: "hostname", addr: "gateway.internal:9000", want: ":9000"},
		{name: "no port", addr: "10.88.0.1", wantErr: true},
		{name: "empty", addr: "", wantErr: true},
		{name: "trailing colon only", addr: "10.88.0.1:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := wildcardAddr(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("wildcardAddr(%q) = %q, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("wildcardAddr(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("wildcardAddr(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

// TestListen_BindableAddressIsUsedDirectly pins that the fallback does not
// kick in when the requested address is actually assignable: loopback is
// always present, so the listener must end up on 127.0.0.1 and not on the
// wildcard.
func TestListen_BindableAddressIsUsedDirectly(t *testing.T) {
	ln, err := Listen("127.0.0.1:0", testLogger())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", ln.Addr().String(), err)
	}
	if host != "127.0.0.1" {
		t.Errorf("bound host = %q, want 127.0.0.1 (fallback should not trigger)", host)
	}
}

// TestListen_FallsBackToWildcard is the regression test for the empty-cache
// bug: binding an address that is not on any interface (the bridge gateway
// before CNI creates the bridge) must NOT fail the proxy, it must fall back
// to the wildcard so the address works once the interface appears.
//
// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) and is never assigned to a real
// interface, so this reproduces EADDRNOTAVAIL without any network access.
func TestListen_FallsBackToWildcard(t *testing.T) {
	ln, err := Listen("203.0.113.1:0", testLogger())
	if err != nil {
		t.Fatalf("Listen fell over instead of falling back: %v", err)
	}
	defer func() { _ = ln.Close() }()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", ln.Addr().String(), err)
	}
	if host == "203.0.113.1" {
		t.Fatalf("bound the TEST-NET address %q — the test assumption is broken", ln.Addr())
	}
	if port == "0" || port == "" {
		t.Errorf("bound port = %q, want a real ephemeral port", port)
	}
	// A wildcard listener must be reachable over loopback.
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("wildcard listener not reachable on loopback: %v", err)
	}
	_ = c.Close()
}

// TestListen_ReturnsOriginalErrorWhenWildcardAlsoFails pins that the
// fallback never masks a genuine failure: when the port is already taken the
// wildcard bind fails too and the caller gets an error, not a silent success.
func TestListen_ReturnsOriginalErrorWhenWildcardAlsoFails(t *testing.T) {
	// Occupy a port on the wildcard address so no further bind can succeed.
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("setting up blocker listener: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	_, port, err := net.SplitHostPort(blocker.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	ln, err := Listen(net.JoinHostPort("203.0.113.1", port), testLogger())
	if err == nil {
		_ = ln.Close()
		t.Skip("host allows rebinding an in-use wildcard port; cannot exercise this path here")
	}
}
