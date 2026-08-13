package networking

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

// Addresses in these tests come from RFC 5737 documentation ranges
// (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24). Nothing here — and nothing in
// the shipped defaults — encodes a real site's LAN.

func mustPool(t *testing.T, spec string) ipRange {
	t.Helper()
	p, err := parseIPPool(spec)
	if err != nil {
		t.Fatalf("parseIPPool(%q): %v", spec, err)
	}
	return p
}

func TestParseIPPool_Forms(t *testing.T) {
	tests := []struct {
		spec      string
		wantLo    string
		wantHi    string
		wantCount uint64
	}{
		// Inclusive start-end range, used verbatim.
		{"192.0.2.200-192.0.2.230", "192.0.2.200", "192.0.2.230", 31},
		{" 192.0.2.10 - 192.0.2.10 ", "192.0.2.10", "192.0.2.10", 1},
		// CIDR: network and broadcast addresses are not handed to containers.
		{"192.0.2.192/27", "192.0.2.193", "192.0.2.222", 30},
		{"192.0.2.0/24", "192.0.2.1", "192.0.2.254", 254},
		// /31 and /32 have no network/broadcast to reserve.
		{"192.0.2.8/31", "192.0.2.8", "192.0.2.9", 2},
		{"192.0.2.8/32", "192.0.2.8", "192.0.2.8", 1},
	}
	for _, tt := range tests {
		got := mustPool(t, tt.spec)
		if u32ToIPv4(got.lo) != tt.wantLo || u32ToIPv4(got.hi) != tt.wantHi {
			t.Errorf("parseIPPool(%q) = %s-%s, want %s-%s",
				tt.spec, u32ToIPv4(got.lo), u32ToIPv4(got.hi), tt.wantLo, tt.wantHi)
		}
		if got.size() != tt.wantCount {
			t.Errorf("parseIPPool(%q).size() = %d, want %d", tt.spec, got.size(), tt.wantCount)
		}
	}
}

func TestParseIPPool_Rejects(t *testing.T) {
	for _, spec := range []string{
		"",                         // unset
		"192.0.2.5",                // bare address, neither form
		"192.0.2.230-192.0.2.200",  // reversed
		"192.0.2.999-192.0.2.1000", // not addresses
		"not-an-address",           // garbage
		"2001:db8::/64",            // IPv6: no v6 path on this stack
		"192.0.2.0/33",             // impossible mask
		"192.0.2.200-",             // half a range
	} {
		if _, err := parseIPPool(spec); err == nil {
			t.Errorf("parseIPPool(%q) accepted an invalid pool", spec)
		}
	}
}

// TestIPAllocator_AllocatesWithinPool is the core guarantee: every address a
// container gets comes out of the operator's reserved range, so it can never
// collide with an address the site's DHCP server hands out.
func TestIPAllocator_AllocatesWithinPool(t *testing.T) {
	pool := mustPool(t, "192.0.2.200-192.0.2.203")
	a := newIPAllocator(pool)

	seen := map[string]bool{}
	for i := range 4 {
		got, err := a.allocate(fmt.Sprintf("job-%d", i))
		if err != nil {
			t.Fatalf("allocate #%d: %v", i, err)
		}
		v, err := parseIPv4(got)
		if err != nil {
			t.Fatalf("allocate returned %q: %v", got, err)
		}
		if !pool.contains(v) {
			t.Errorf("allocated %s outside the pool %s", got, pool)
		}
		if seen[got] {
			t.Errorf("allocated %s twice", got)
		}
		seen[got] = true
	}

	// Exhaustion must be an error, never a silent fallback to an
	// HNS-chosen address that could collide with a DHCP lease.
	if _, err := a.allocate("job-overflow"); err == nil {
		t.Fatal("allocate succeeded on an exhausted pool; want an error so the job is refused")
	} else if !strings.Contains(err.Error(), "network.ip_pool") {
		t.Errorf("exhaustion error %q does not name the config key to raise", err)
	}
}

func TestIPAllocator_ReservesHostAndGateway(t *testing.T) {
	pool := mustPool(t, "192.0.2.1-192.0.2.3")
	a := newIPAllocator(pool, "192.0.2.1", "192.0.2.2")

	got, err := a.allocate("job")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if got != "192.0.2.3" {
		t.Errorf("allocate = %s, want 192.0.2.3 (the reserved host and gateway must be skipped)", got)
	}
}

func TestIPAllocator_ReleaseAndIdempotence(t *testing.T) {
	a := newIPAllocator(mustPool(t, "192.0.2.10-192.0.2.11"))

	first, err := a.allocate("job-a")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// Re-allocating for the same id must return the same address, so a retried
	// setup does not burn a second one.
	again, err := a.allocate("job-a")
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if again != first {
		t.Errorf("re-allocate for the same id = %s, want the original %s", again, first)
	}

	if _, err := a.allocate("job-b"); err != nil {
		t.Fatalf("allocate second: %v", err)
	}
	if _, err := a.allocate("job-c"); err == nil {
		t.Fatal("pool of 2 handed out a third address")
	}

	// After release the address returns to the pool — otherwise a long-lived
	// daemon leaks the pool one job at a time.
	a.release("job-a")
	reused, err := a.allocate("job-c")
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if reused != first {
		t.Errorf("allocate after release = %s, want the freed %s", reused, first)
	}

	a.release("never-allocated") // must not panic
}

// fakeLookup builds a hostNetLookup that reports a fixed adapter address and
// default route, so the plan can be exercised without a Windows host.
func fakeLookup(hostCIDR, gateway string) hostNetLookup {
	return hostNetLookup{
		iface: func(string) (net.IP, *net.IPNet, error) {
			ip, ipnet, err := net.ParseCIDR(hostCIDR)
			if err != nil {
				return nil, nil, err
			}
			return ip.To4(), ipnet, nil
		},
		gateway: func(string) (string, error) { return gateway, nil },
	}
}

// TestResolveL2BridgePlan_DerivesFromHost verifies the auto-derivation contract:
// with only host_nic and ip_pool set, the subnet and gateway come off the host's
// real adapter. Nothing is defaulted to a fixed network.
func TestResolveL2BridgePlan_DerivesFromHost(t *testing.T) {
	plan, err := resolveL2BridgePlan(
		Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.230"},
		fakeLookup("198.51.100.7/24", "198.51.100.1"),
	)
	if err != nil {
		t.Fatalf("resolveL2BridgePlan: %v", err)
	}
	if plan.Subnet != "198.51.100.0/24" || !plan.DerivedSubnet {
		t.Errorf("subnet = %q derived=%v, want 198.51.100.0/24 derived", plan.Subnet, plan.DerivedSubnet)
	}
	if plan.PrefixLen != 24 {
		t.Errorf("prefix len = %d, want 24", plan.PrefixLen)
	}
	if plan.Gateway != "198.51.100.1" || !plan.DerivedGateway {
		t.Errorf("gateway = %q derived=%v, want 198.51.100.1 derived", plan.Gateway, plan.DerivedGateway)
	}
	if plan.HostIP != "198.51.100.7" {
		t.Errorf("host ip = %q, want 198.51.100.7", plan.HostIP)
	}
	if plan.PoolSpec != "198.51.100.200-198.51.100.230" {
		t.Errorf("pool spec = %q, want the operator's own string", plan.PoolSpec)
	}
}

// TestResolveL2BridgePlan_ExplicitOverridesWin verifies a pinned subnet/gateway
// is used as-is and reported as not derived.
func TestResolveL2BridgePlan_ExplicitOverridesWin(t *testing.T) {
	plan, err := resolveL2BridgePlan(
		Config{
			HostNIC: "Ethernet",
			IPPool:  "198.51.100.96/29",
			Subnet:  "198.51.100.0/25",
			Gateway: "198.51.100.9",
		},
		fakeLookup("198.51.100.7/24", "198.51.100.1"),
	)
	if err != nil {
		t.Fatalf("resolveL2BridgePlan: %v", err)
	}
	if plan.Subnet != "198.51.100.0/25" || plan.DerivedSubnet {
		t.Errorf("subnet = %q derived=%v, want the configured 198.51.100.0/25", plan.Subnet, plan.DerivedSubnet)
	}
	if plan.Gateway != "198.51.100.9" || plan.DerivedGateway {
		t.Errorf("gateway = %q derived=%v, want the configured 198.51.100.9", plan.Gateway, plan.DerivedGateway)
	}
}

// TestResolveL2BridgePlan_Failures walks every way the plan can be incomplete.
// Each must fail fast with a message naming the key to fix — never fall back to
// a guessed pool, which would hand containers addresses DHCP is also leasing.
func TestResolveL2BridgePlan_Failures(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		look     hostNetLookup
		wantText string
	}{
		{
			name:     "no host_nic",
			cfg:      Config{IPPool: "198.51.100.200-198.51.100.230"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "network.host_nic",
		},
		{
			name:     "no ip_pool",
			cfg:      Config{HostNIC: "Ethernet"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "network.ip_pool",
		},
		{
			name: "adapter not found",
			cfg:  Config{HostNIC: "Nope", IPPool: "198.51.100.200-198.51.100.230"},
			look: hostNetLookup{
				iface:   func(string) (net.IP, *net.IPNet, error) { return nil, nil, fmt.Errorf("no such adapter") },
				gateway: func(string) (string, error) { return "198.51.100.1", nil },
			},
			wantText: "no such adapter",
		},
		{
			name: "no default route on the adapter",
			cfg:  Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.230"},
			look: hostNetLookup{
				iface:   fakeLookup("198.51.100.7/24", "").iface,
				gateway: func(string) (string, error) { return "", nil },
			},
			wantText: "network.gateway",
		},
		{
			name:     "pool outside the subnet",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "203.0.113.200-203.0.113.230"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "not inside network.subnet",
		},
		{
			name:     "pool swallows this host",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "198.51.100.1-198.51.100.50"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "own address",
		},
		{
			name:     "pool swallows the gateway",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.254"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.201"),
			wantText: "LAN gateway",
		},
		{
			name:     "gateway outside the subnet",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.230", Gateway: "203.0.113.1"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "outside network.subnet",
		},
		{
			name:     "pinned subnet does not contain the adapter address",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "203.0.113.200-203.0.113.230", Subnet: "203.0.113.0/24"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "does not contain the address",
		},
		{
			name:     "malformed pool",
			cfg:      Config{HostNIC: "Ethernet", IPPool: "banana"},
			look:     fakeLookup("198.51.100.7/24", "198.51.100.1"),
			wantText: "network.ip_pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := resolveL2BridgePlan(tt.cfg, tt.look)
			if err == nil {
				t.Fatalf("resolveL2BridgePlan succeeded, want an error; got plan %+v", plan)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q, so an operator cannot tell what to fix", err, tt.wantText)
			}
		})
	}
}

// TestResolveL2BridgePlan_NoBuiltInNetwork is the guard against re-introducing a
// hardcoded LAN. With no adapter information available the plan must fail, not
// silently fall back to some built-in subnet, gateway, or pool.
func TestResolveL2BridgePlan_NoBuiltInNetwork(t *testing.T) {
	_, err := resolveL2BridgePlan(
		Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.230"},
		hostNetLookup{
			iface:   func(string) (net.IP, *net.IPNet, error) { return nil, nil, fmt.Errorf("adapter has no address") },
			gateway: func(string) (string, error) { return "", fmt.Errorf("no route") },
		},
	)
	if err == nil {
		t.Fatal("resolveL2BridgePlan invented an address plan with no host information")
	}
}

// TestResolveL2BridgePlan_PoolFitsMaxConcurrent documents the sizing
// relationship an operator has to satisfy, using the shipped default
// runner.max_concurrent of 4.
func TestResolveL2BridgePlan_PoolFitsMaxConcurrent(t *testing.T) {
	plan, err := resolveL2BridgePlan(
		Config{HostNIC: "Ethernet", IPPool: "198.51.100.200-198.51.100.203"},
		fakeLookup("198.51.100.7/24", "198.51.100.1"),
	)
	if err != nil {
		t.Fatalf("resolveL2BridgePlan: %v", err)
	}
	if plan.Pool.size() < 4 {
		t.Errorf("pool holds %d addresses, too few for the default max_concurrent of 4", plan.Pool.size())
	}
}
