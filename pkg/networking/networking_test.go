package networking

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- subnetInUse tests ---

func TestSubnetInUse_InvalidCIDR(t *testing.T) {
	if subnetInUse("not-a-cidr") {
		t.Error("expected false for invalid CIDR")
	}
}

func TestSubnetInUse_EmptyString(t *testing.T) {
	if subnetInUse("") {
		t.Error("expected false for empty string")
	}
}

func TestSubnetInUse_LoopbackInUse(t *testing.T) {
	// 127.0.0.0/8 should be in use (loopback interface exists everywhere)
	if !subnetInUse("127.0.0.0/8") {
		t.Error("expected 127.0.0.0/8 to be in use (loopback)")
	}
}

func TestSubnetInUse_HighRange(t *testing.T) {
	// 10.253.0.0/16 is very unlikely to be in use on a dev machine
	// (this is a probabilistic test, but should be reliable)
	result := subnetInUse("10.253.0.0/16")
	t.Logf("10.253.0.0/16 in use: %v", result)
	// Don't assert false — some CI environments may use this range
}

func TestSubnetInUse_ValidCIDR(t *testing.T) {
	// Just verify it doesn't panic on various valid CIDRs
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"10.88.0.0/16",
		"10.199.0.0/16",
	}
	for _, cidr := range cidrs {
		subnetInUse(cidr) // should not panic
	}
}

// --- pickSubnet tests ---

func TestPickSubnet_ReturnsValidCIDR(t *testing.T) {
	result := pickSubnet(testLogger())
	if result == "" {
		t.Fatal("pickSubnet returned empty string")
	}

	_, _, err := net.ParseCIDR(result)
	if err != nil {
		t.Errorf("pickSubnet returned invalid CIDR %q: %v", result, err)
	}
}

func TestPickSubnet_ReturnsTenDotSubnet(t *testing.T) {
	result := pickSubnet(testLogger())
	if !strings.HasPrefix(result, "10.") {
		t.Errorf("pickSubnet = %q, expected 10.x.x.x/16", result)
	}
	if !strings.HasSuffix(result, "/16") {
		t.Errorf("pickSubnet = %q, expected /16 suffix", result)
	}
}

func TestPickSubnet_PrefersDefault(t *testing.T) {
	// If DefaultSubnet (10.88.0.0/16) is not in use, pickSubnet should return it.
	// If it IS in use, it should return an alternative.
	result := pickSubnet(testLogger())

	if !subnetInUse(DefaultSubnet) {
		if result != DefaultSubnet {
			t.Errorf("pickSubnet = %q, want %q (default not in use)", result, DefaultSubnet)
		}
	} else {
		if result == DefaultSubnet {
			t.Errorf("pickSubnet returned default %q even though it's in use", DefaultSubnet)
		}
	}
}

func TestPickSubnet_Deterministic_WhenDefaultFree(t *testing.T) {
	// When the default subnet is free, calling pickSubnet multiple times
	// should always return the same result
	if subnetInUse(DefaultSubnet) {
		t.Skip("default subnet in use, cannot test determinism")
	}

	result1 := pickSubnet(testLogger())
	result2 := pickSubnet(testLogger())
	if result1 != result2 {
		t.Errorf("pickSubnet not deterministic: %q vs %q", result1, result2)
	}
}

// --- pickSubnetFromAddrs (item #17) ---

// addrCIDR builds a net.Addr from a CIDR string for tests. net.IPNet
// implements net.Addr ("ip+net" / CIDR), matching what iface.Addrs() returns.
func addrCIDR(t *testing.T, cidr string) net.Addr {
	t.Helper()
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse CIDR %q: %v", cidr, err)
	}
	return ipnet
}

func TestPickSubnetFromAddrs_DefaultFreeReturnsDefault(t *testing.T) {
	// No interface addresses overlap DefaultSubnet → return it without retrying.
	addrs := []net.Addr{
		addrCIDR(t, "192.168.1.5/24"),
		addrCIDR(t, "172.16.0.10/16"),
	}
	got := pickSubnetFromAddrs(testLogger(), addrs)
	if got != DefaultSubnet {
		t.Errorf("pickSubnetFromAddrs = %q, want %q", got, DefaultSubnet)
	}
}

func TestPickSubnetFromAddrs_NoAddrs(t *testing.T) {
	got := pickSubnetFromAddrs(testLogger(), nil)
	if got != DefaultSubnet {
		t.Errorf("pickSubnetFromAddrs(nil) = %q, want %q", got, DefaultSubnet)
	}
	got = pickSubnetFromAddrs(testLogger(), []net.Addr{})
	if got != DefaultSubnet {
		t.Errorf("pickSubnetFromAddrs([]) = %q, want %q", got, DefaultSubnet)
	}
}

func TestPickSubnetFromAddrs_DefaultBusyPicksAlternative(t *testing.T) {
	// DefaultSubnet (10.88.0.0/16) is "in use", but other 10.x ranges are free.
	// pickSubnetFromAddrs should return some 10.x.0.0/16 that isn't 10.88.
	addrs := []net.Addr{
		addrCIDR(t, "10.88.5.1/16"), // overlaps DefaultSubnet
	}
	got := pickSubnetFromAddrs(testLogger(), addrs)
	if got == DefaultSubnet {
		t.Errorf("pickSubnetFromAddrs returned default %q despite conflict", got)
	}
	if !strings.HasPrefix(got, "10.") || !strings.HasSuffix(got, "/16") {
		t.Errorf("pickSubnetFromAddrs = %q, want 10.x.0.0/16", got)
	}
	_, _, err := net.ParseCIDR(got)
	if err != nil {
		t.Errorf("pickSubnetFromAddrs returned invalid CIDR %q: %v", got, err)
	}
}

func TestPickSubnetFromAddrs_AllTenRangesBusyFallsBack(t *testing.T) {
	// To exhaust the 10-retry budget, every random 10.x.0.0/16 the
	// function tries must hit a "busy" address. subnetInUseAmong checks
	// whether the candidate /16 contains any address's IP, so we need
	// one IP per /16. Place 10.<n>.0.1 for every n in 0..255, plus
	// 10.88.0.1 to fail the DefaultSubnet check up front.
	addrs := make([]net.Addr, 0, 256)
	for n := 0; n < 256; n++ {
		addrs = append(addrs, addrCIDR(t, fmt.Sprintf("10.%d.0.1/16", n)))
	}
	got := pickSubnetFromAddrs(testLogger(), addrs)
	if got != "10.199.0.0/16" {
		t.Errorf("pickSubnetFromAddrs fallback = %q, want %q", got, "10.199.0.0/16")
	}
}

func TestPickSubnetFromAddrs_SkipsBusyCandidatesUntilFree(t *testing.T) {
	// Block the default + several 10.x ranges, leaving most of the 10.x
	// space free. The function must complete within the 10-retry budget
	// (it does, because the chance of hitting one of ~10 blocked /16s
	// 10 times in a row is < 10^-10).
	addrs := []net.Addr{
		addrCIDR(t, "10.88.0.0/16"),
		addrCIDR(t, "10.0.0.0/16"),
		addrCIDR(t, "10.1.0.0/16"),
		addrCIDR(t, "10.2.0.0/16"),
		addrCIDR(t, "10.10.0.0/16"),
		addrCIDR(t, "10.50.0.0/16"),
		addrCIDR(t, "10.100.0.0/16"),
		addrCIDR(t, "10.150.0.0/16"),
		addrCIDR(t, "10.200.0.0/16"),
		addrCIDR(t, "10.255.0.0/16"),
	}
	got := pickSubnetFromAddrs(testLogger(), addrs)
	if !strings.HasPrefix(got, "10.") || !strings.HasSuffix(got, "/16") {
		t.Errorf("pickSubnetFromAddrs = %q, want 10.x.0.0/16", got)
	}
	// Either the loop succeeded (some 10.x not in our blocked set) or it
	// fell back to 10.199.0.0/16. Both are acceptable outcomes — assert
	// only that the result is not one of the explicitly blocked /16s.
	if got == DefaultSubnet {
		t.Errorf("pickSubnetFromAddrs returned blocked default %q", got)
	}
	for _, blocked := range []string{
		"10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16", "10.10.0.0/16",
		"10.50.0.0/16", "10.100.0.0/16", "10.150.0.0/16",
		"10.200.0.0/16", "10.255.0.0/16",
	} {
		if got == blocked {
			t.Errorf("pickSubnetFromAddrs returned blocked %q", got)
		}
	}
}

// --- subnetInUseAmong (item #17 helper) ---

func TestSubnetInUseAmong_NoMatch(t *testing.T) {
	addrs := []net.Addr{
		addrCIDR(t, "192.168.1.1/24"),
	}
	if subnetInUseAmong("10.0.0.0/8", addrs) {
		t.Error("expected false: 192.168.1.1 not in 10.0.0.0/8")
	}
}

func TestSubnetInUseAmong_Match(t *testing.T) {
	addrs := []net.Addr{
		addrCIDR(t, "10.88.5.1/16"),
	}
	if !subnetInUseAmong("10.88.0.0/16", addrs) {
		t.Error("expected true: 10.88.5.1 should be in 10.88.0.0/16")
	}
}

func TestSubnetInUseAmong_InvalidCIDR(t *testing.T) {
	if subnetInUseAmong("not-a-cidr", []net.Addr{addrCIDR(t, "10.0.0.1/8")}) {
		t.Error("expected false for invalid CIDR")
	}
}

// --- DefaultSubnet constant ---

func TestDefaultSubnet(t *testing.T) {
	if DefaultSubnet != "10.88.0.0/16" {
		t.Errorf("DefaultSubnet = %q, want %q", DefaultSubnet, "10.88.0.0/16")
	}

	_, _, err := net.ParseCIDR(DefaultSubnet)
	if err != nil {
		t.Errorf("DefaultSubnet is not a valid CIDR: %v", err)
	}
}

// --- Config / SetupResult types ---

func TestSetupResult_ZeroValue(t *testing.T) {
	r := SetupResult{}
	if r.NetNS != "" {
		t.Errorf("zero NetNS = %q, want empty", r.NetNS)
	}
	if r.EndpointID != "" {
		t.Errorf("zero EndpointID = %q, want empty", r.EndpointID)
	}
}

func TestConfig_Defaults(t *testing.T) {
	cfg := Config{}
	if cfg.Subnet != "" {
		t.Errorf("zero Subnet = %q, want empty", cfg.Subnet)
	}
	if cfg.MTU != 0 {
		t.Errorf("zero MTU = %d, want 0", cfg.MTU)
	}
}
