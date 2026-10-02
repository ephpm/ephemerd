package proxies

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// The env the fleet's proxies ACTUALLY emit, copied verbatim off
// mfl-mac-arm64's log on 2026-10-02 — note GOPROXY carries Go's "|direct"
// fallback list, which the first version of this test did not.
//
// That omission is why a half-working fix shipped: with a bare
// GOPROXY=http://host:port the tests passed, while production left GOPROXY
// unrewritten and pointed at an unreachable address. Keep this fixture
// byte-identical to observed output; do not "tidy" it.
func fleetEnv() []string {
	return []string{
		"GOPROXY=http://10.88.0.1:8082|direct",
		"GOSUMDB=off",
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse",
		"RUSTUP_DIST_SERVER=http://10.88.0.1:8083/rustup",
		"GHREL_PROXY=http://10.88.0.1:8087",
		"COMPOSER_REPO_PACKAGIST=http://10.88.0.1:8088",
	}
}

func TestRewriteEnvHostFixesTheAdvertisedAddress(t *testing.T) {
	got := RewriteEnvHost(fleetEnv(), "192.168.64.1")

	want := []string{
		"GOPROXY=http://192.168.64.1:8082|direct",
		"GOSUMDB=off",
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse",
		"RUSTUP_DIST_SERVER=http://192.168.64.1:8083/rustup",
		"GHREL_PROXY=http://192.168.64.1:8087",
		"COMPOSER_REPO_PACKAGIST=http://192.168.64.1:8088",
	}
	if !slices.Equal(got, want) {
		t.Errorf("RewriteEnvHost mismatch\n got: %q\nwant: %q", got, want)
	}
}

// Ports and paths carry real meaning — RUSTUP_DIST_SERVER's /rustup route and
// each proxy's distinct port. Clobbering either would swap one broken address
// for another, which is the failure mode this whole change exists to end.
func TestRewriteEnvHostPreservesPortAndPath(t *testing.T) {
	got := RewriteEnvHost([]string{"RUSTUP_DIST_SERVER=http://10.88.0.1:8083/rustup"}, "172.20.0.1")
	if got[0] != "RUSTUP_DIST_SERVER=http://172.20.0.1:8083/rustup" {
		t.Errorf("got %q", got[0])
	}
}

// Not every emitted var is a URL (GOSUMDB=off, CARGO_..._PROTOCOL=sparse).
// Mangling those would break the toolchain in a way unrelated to networking.
func TestRewriteEnvHostLeavesNonURLsAlone(t *testing.T) {
	in := []string{"GOSUMDB=off", "GOFLAGS=-mod=mod", "NOT_AN_ENTRY", "X=not://a real url"}
	got := RewriteEnvHost(in, "10.0.0.1")
	if !slices.Equal(got, in) {
		t.Errorf("non-URL entries were modified\n got: %q\nwant: %q", got, in)
	}
}

// An empty host must be a no-op, so a caller that could not determine the
// address leaves env untouched rather than producing "http://:8082".
func TestRewriteEnvHostEmptyHostIsNoOp(t *testing.T) {
	in := fleetEnv()
	if got := RewriteEnvHost(in, ""); !slices.Equal(got, in) {
		t.Errorf("empty host rewrote env: %q", got)
	}
}

// Reachable must distinguish a live listener from a dead address. This is the
// check the old loopback health gate could not perform, and the reason the
// macOS caches sat at 0 bytes for 169 days while every proxy reported healthy.
func TestReachableDistinguishesLiveFromDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	live := srv.Listener.Addr().String()

	if !Reachable(live, 2*time.Second) {
		t.Errorf("Reachable(%s) = false for a live listener", live)
	}

	// Bind then close to get a port nothing is listening on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	_ = l.Close()

	if Reachable(dead, 500*time.Millisecond) {
		t.Errorf("Reachable(%s) = true for a closed port", dead)
	}
}

// 10.88.0.1 is not routable from the host on macOS, so the probe must fail
// rather than hang past its timeout — the gate has to be cheap enough to run
// at every daemon start.
func TestReachableRespectsTimeout(t *testing.T) {
	start := time.Now()
	if Reachable("10.88.0.1:8082", 300*time.Millisecond) {
		t.Skip("10.88.0.1:8082 is reachable on this host; nothing to assert")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("probe took %s, far beyond its 300ms timeout", el)
	}
}

func TestEnvHostPortsCollectsAdvertisedAddresses(t *testing.T) {
	got := EnvHostPorts(fleetEnv())
	want := []string{"10.88.0.1:8082", "10.88.0.1:8083", "10.88.0.1:8087", "10.88.0.1:8088"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

// Derivation, not a hardcoded hypervisor constant: the host address a peer can
// reach us on is whichever of our interfaces shares its subnet. A peer on no
// local subnet must report false so the caller declines to advertise instead of
// guessing — guessing is precisely what produced the outage.
func TestHostAddrVisibleTo(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	var probed bool
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		// Our own address is trivially on our own subnet.
		got, ok2 := HostAddrVisibleTo(ipnet.IP.String())
		if !ok2 {
			t.Errorf("HostAddrVisibleTo(%s) = not found, but it is our own address", ipnet.IP)
			continue
		}
		if net.ParseIP(got) == nil {
			t.Errorf("HostAddrVisibleTo(%s) returned non-IP %q", ipnet.IP, got)
		}
		probed = true
		break
	}
	if !probed {
		t.Skip("no non-loopback IPv4 interface to test against")
	}

	if _, ok := HostAddrVisibleTo("203.0.113.7"); ok {
		t.Error("HostAddrVisibleTo(public IP) returned a match; it should decline")
	}
	if _, ok := HostAddrVisibleTo("not-an-ip"); ok {
		t.Error("HostAddrVisibleTo(garbage) returned a match")
	}
}

// HostAddrInSubnet is what the darwin path uses to find the Vz NAT host
// address. It is tested here, not there, because the darwin build needs macOS
// cgo and cannot be compiled on CI's other platforms — logic in that file is
// unverifiable, logic here is not.
func TestHostAddrInSubnet(t *testing.T) {
	// A subnet containing one of our own addresses must return that address.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		got, found := HostAddrInSubnet(ipnet)
		if !found {
			t.Errorf("HostAddrInSubnet(%s) = not found for a subnet we are on", ipnet)
		} else if net.ParseIP(got) == nil {
			t.Errorf("returned non-IP %q", got)
		}
		break
	}

	// Apple's Vz range, when we are not on it, must decline rather than guess.
	vz := &net.IPNet{IP: net.IPv4(192, 168, 64, 0), Mask: net.CIDRMask(24, 32)}
	if got, ok := HostAddrInSubnet(vz); ok && !vz.Contains(net.ParseIP(got)) {
		t.Errorf("returned %q which is not inside %s", got, vz)
	}

	// TEST-NET-3: nothing is ever on it.
	bogus := &net.IPNet{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)}
	if _, ok := HostAddrInSubnet(bogus); ok {
		t.Error("HostAddrInSubnet(203.0.113.0/24) matched; it should decline")
	}
	if _, ok := HostAddrInSubnet(nil); ok {
		t.Error("HostAddrInSubnet(nil) matched")
	}
}

// Regression for the half-working fix shipped on 2026-10-02: GOPROXY is a
// fallback LIST, not a URL. Production emits "http://host:port|direct"; the
// single-URL rewrite silently skipped it, so Go — the heaviest consumer in a Go
// codebase — kept pointing at an unreachable proxy while three other vars were
// correctly rewritten and the log line claimed success.
func TestRewriteEnvHostHandlesGoProxyLists(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"pipe direct", "http://10.88.0.1:8082|direct", "http://192.168.64.1:8082|direct"},
		{"comma direct", "http://10.88.0.1:8082,direct", "http://192.168.64.1:8082,direct"},
		{"two proxies then direct", "http://10.88.0.1:8082|https://proxy.golang.org|direct",
			"http://192.168.64.1:8082|https://192.168.64.1|direct"},
		{"off literal", "off", "off"},
		{"direct only", "direct", "direct"},
		{"trailing separator kept", "http://10.88.0.1:8082|", "http://192.168.64.1:8082|"},
	} {
		got := RewriteEnvHost([]string{"GOPROXY=" + tc.in}, "192.168.64.1")
		if got[0] != "GOPROXY="+tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got[0], "GOPROXY="+tc.want)
		}
	}
}

// The probe must see the Go proxy's address too, or it advertises a host it
// never verified — defeating the entire point of the gate.
func TestEnvHostPortsSeesListElements(t *testing.T) {
	got := EnvHostPorts([]string{"GOPROXY=http://10.88.0.1:8082|direct"})
	if len(got) != 1 || got[0] != "10.88.0.1:8082" {
		t.Errorf("got %q, want [10.88.0.1:8082] — the probe is blind to list elements", got)
	}
}
