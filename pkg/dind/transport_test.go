package dind

import (
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
)

func TestResolveTransport(t *testing.T) {
	tests := []struct {
		name      string
		requested Transport
		goos      string
		want      Transport
	}{
		{
			name:      "linux defaults to the unix socket",
			requested: TransportAuto,
			goos:      "linux",
			want:      TransportUnixSocket,
		},
		{
			name:      "darwin defaults to the unix socket",
			requested: TransportAuto,
			goos:      "darwin",
			want:      TransportUnixSocket,
		},
		{
			name:      "linux honours an explicit TCP request",
			requested: TransportTCP,
			goos:      "linux",
			want:      TransportTCP,
		},
		{
			name:      "linux honours an explicit unix request",
			requested: TransportUnixSocket,
			goos:      "linux",
			want:      TransportUnixSocket,
		},
		{
			// runhcs supports neither the bind nor named-pipe sharing into an
			// isolated container, so a caller must not be able to ask for a
			// transport that has never worked there.
			name:      "windows is TCP even when unix is requested",
			requested: TransportUnixSocket,
			goos:      "windows",
			want:      TransportTCP,
		},
		{
			name:      "windows is TCP by default",
			requested: TransportAuto,
			goos:      "windows",
			want:      TransportTCP,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveTransport(tt.requested, tt.goos); got != tt.want {
				t.Errorf("resolveTransport(%q, %q) = %q, want %q", tt.requested, tt.goos, got, tt.want)
			}
		})
	}
}

func newTransportTestServer(t *testing.T, transport Transport) *Server {
	t.Helper()
	s, err := New(Config{
		JobID:     "transport-test",
		DataDir:   t.TempDir(),
		Transport: transport,
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// On the TCP transport there must be no socket to mount: SocketPath is the
// runtime's signal for which of the two exposure mechanisms to use, so a
// non-empty value here would make it bind-mount a socket into a guest that
// cannot connect to it.
func TestTCPTransport_NoSocketPathAndTCPEndpoint(t *testing.T) {
	if platformGOOS == "windows" {
		t.Skip("windows is always TCP; the unix comparison below does not apply")
	}
	s := newTransportTestServer(t, TransportTCP)

	if got := s.SocketPath(); got != "" {
		t.Errorf("SocketPath() = %q on the TCP transport, want empty", got)
	}
	if got := s.Transport(); got != TransportTCP {
		t.Errorf("Transport() = %q, want %q", got, TransportTCP)
	}
	if !strings.HasPrefix(s.Endpoint(), "tcp://") {
		t.Errorf("Endpoint() = %q, want a tcp:// URI", s.Endpoint())
	}
	if s.EndpointPort() == 0 {
		t.Error("EndpointPort() = 0, want the bound port (SetRunnerIP keys the firewall scope on it)")
	}

	// The listener must actually serve on the advertised endpoint — the
	// whole point of the transport is that a guest can reach it over IP.
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.Endpoint(), "tcp://"))
	if err != nil {
		t.Fatalf("dialing advertised endpoint %s: %v", s.Endpoint(), err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("closing probe connection: %v", err)
	}
}

// The default on Linux must stay the unix socket, byte for byte what runc
// jobs got before the transport became selectable.
func TestDefaultTransport_UnixSocketUnchanged(t *testing.T) {
	if platformGOOS == "windows" {
		t.Skip("the unix socket transport does not apply on Windows")
	}
	s := newTransportTestServer(t, TransportAuto)

	if got := s.Transport(); got != TransportUnixSocket {
		t.Errorf("Transport() = %q, want %q", got, TransportUnixSocket)
	}
	sock := s.SocketPath()
	if sock == "" {
		t.Fatal("SocketPath() is empty on the default transport, want the per-job socket path")
	}
	if got := s.Endpoint(); got != "unix://"+sock {
		t.Errorf("Endpoint() = %q, want %q", got, "unix://"+sock)
	}
	if s.EndpointPort() != 0 {
		t.Errorf("EndpointPort() = %d on the unix transport, want 0 (nothing to firewall)", s.EndpointPort())
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Errorf("socket mode = %v, want 0666 (non-root container users must be able to connect)", info.Mode().Perm())
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dialing unix socket: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("closing probe connection: %v", err)
	}
}

// SetRunnerIP is a no-op without a networking manager, and must not report
// success in a way that hides a missing scope: with no manager there is no
// port to scope, because tests bind loopback.
func TestSetRunnerIP_NoNetworkManagerIsNoOp(t *testing.T) {
	s := newTransportTestServer(t, TransportTCP)
	if err := s.SetRunnerIP("10.88.0.7"); err != nil {
		t.Errorf("SetRunnerIP with no network manager: %v", err)
	}
	if s.hostPort != 0 {
		t.Errorf("hostPort = %d, want 0 — nothing was opened", s.hostPort)
	}
}

// Under the TCP transport the job container is VM-isolated, so a -v source
// inside its own filesystem is invisible to this daemon. Mounting it anyway
// hands the sibling an empty directory with no error, which is worse than a
// refusal — so it must be refused.
func TestRejectUnbackedGuestBind(t *testing.T) {
	if platformGOOS == "windows" {
		t.Skip("the guard is Linux-only; Windows binds are rejected by the translator itself")
	}
	runnerBinds := map[string]string{
		"/actions-runner":      "/var/lib/ephemerd/runners/job-x",
		"/etc/hosts":           "/var/lib/ephemerd/hosts/x.hosts",
		"/var/run/docker.sock": "/var/lib/ephemerd/jobs/x/docker/d.sock",
	}

	tcp := &Server{transport: TransportTCP}
	for _, src := range []string{"/tmp/ctx", "/home/runner/_work/repo", "/"} {
		err := tcp.rejectUnbackedGuestBind(src, runnerBinds)
		if err == nil {
			t.Errorf("rootfs-relative source %q was accepted on the TCP transport; it must be refused", src)
			continue
		}
		if !strings.Contains(err.Error(), src) {
			t.Errorf("error %q does not name the offending source %q", err, src)
		}
	}

	// Host-backed mounts are shared into the guest by the runtime, so both
	// sides see the same bytes and the bind is genuinely fine.
	for _, src := range []string{"/actions-runner", "/actions-runner/_work/repo", "/etc/hosts"} {
		if err := tcp.rejectUnbackedGuestBind(src, runnerBinds); err != nil {
			t.Errorf("host-backed source %q was refused: %v", src, err)
		}
	}

	// The unix transport means a kernel-sharing container, where the existing
	// rootfs translation is correct — the guard must not touch it.
	unix := &Server{transport: TransportUnixSocket}
	for _, src := range []string{"/tmp/ctx", "/home/runner/_work/repo"} {
		if err := unix.rejectUnbackedGuestBind(src, runnerBinds); err != nil {
			t.Errorf("source %q refused on the unix transport: %v", src, err)
		}
	}
}
