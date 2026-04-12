package networking

import (
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
