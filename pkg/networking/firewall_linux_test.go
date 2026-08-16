//go:build linux

package networking

import (
	"strings"
	"testing"
)

// joinRule renders an argv slice back to a space-joined string for
// substring assertions.
func joinRule(r []string) string { return strings.Join(r, " ") }

func TestControlPlaneInputRules_EmitsDropPerControlPort(t *testing.T) {
	subnet := "10.88.0.0/16"
	gateway := "10.88.0.1"
	ports := []int{10000, 10001, 10002} // containerd, dispatch, debug exec

	rules := controlPlaneInputRules(subnet, gateway, ports)
	if len(rules) != len(ports) {
		t.Fatalf("got %d rules, want %d (one DROP per control port)", len(rules), len(ports))
	}

	for i, port := range []string{"10000", "10001", "10002"} {
		got := joinRule(rules[i])
		want := "INPUT -s " + subnet + " -d " + gateway + " -p tcp --dport " + port + " -j DROP"
		if got != want {
			t.Errorf("rule[%d] = %q, want %q", i, got, want)
		}
	}
}

// TestControlPlaneInputRules_NarrowScope pins the safety property: every
// control-plane DROP is scoped to source=subnet, dest=gateway, a specific
// TCP dport — never a blanket gateway block and never touching port 53. That
// is what keeps container→gateway NAT and DNS working.
func TestControlPlaneInputRules_NarrowScope(t *testing.T) {
	rules := controlPlaneInputRules("10.88.0.0/16", "10.88.0.1", []int{10000, 10001, 10002})
	for _, r := range rules {
		s := joinRule(r)
		if !strings.Contains(s, "-s 10.88.0.0/16") {
			t.Errorf("rule %q missing source-subnet scope", s)
		}
		if !strings.Contains(s, "-d 10.88.0.1") {
			t.Errorf("rule %q missing gateway-dest scope", s)
		}
		if !strings.Contains(s, "-p tcp") || !strings.Contains(s, "--dport") {
			t.Errorf("rule %q is not TCP-dport-scoped (would over-block)", s)
		}
		if strings.Contains(s, "--dport 53") {
			t.Errorf("rule %q blocks DNS (dport 53) — must not", s)
		}
	}
}

func TestControlPlaneInputRules_EmptyWhenNoPorts(t *testing.T) {
	if rules := controlPlaneInputRules("10.88.0.0/16", "10.88.0.1", nil); rules != nil {
		t.Errorf("expected nil rules with no control ports, got %v", rules)
	}
	if rules := controlPlaneInputRules("", "10.88.0.1", []int{10000}); rules != nil {
		t.Errorf("expected nil rules with empty subnet, got %v", rules)
	}
	if rules := controlPlaneInputRules("10.88.0.0/16", "", []int{10000}); rules != nil {
		t.Errorf("expected nil rules with empty gateway, got %v", rules)
	}
}

func TestIPv6FirewallRules_ForwardDenies(t *testing.T) {
	rules := ipv6FirewallRules(nil)
	// With no control ports, only the FORWARD private-range denies are emitted.
	if len(rules) != len(deniedRanges6) {
		t.Fatalf("got %d rules, want %d FORWARD denies", len(rules), len(deniedRanges6))
	}
	wantCIDRs := map[string]bool{"fc00::/7": false, "fe80::/10": false}
	for _, r := range rules {
		if r.chain != "FORWARD" {
			t.Errorf("rule chain = %q, want FORWARD", r.chain)
		}
		if r.insert {
			t.Errorf("FORWARD deny should append, not insert: %v", r)
		}
		s := joinRule(r.match)
		if !strings.Contains(s, "-j REJECT") {
			t.Errorf("FORWARD deny %q should REJECT", s)
		}
		for cidr := range wantCIDRs {
			if strings.Contains(s, "-d "+cidr) {
				wantCIDRs[cidr] = true
			}
		}
	}
	for cidr, seen := range wantCIDRs {
		if !seen {
			t.Errorf("missing IPv6 FORWARD deny for %s", cidr)
		}
	}
}

// TestIPv6FirewallRules_ControlPlaneInputDrops confirms the v6 control-plane
// INPUT drops are emitted for each control port × denied v6 range, inserted
// first, and TCP-dport-scoped (mirroring the IPv4 posture).
func TestIPv6FirewallRules_ControlPlaneInputDrops(t *testing.T) {
	ports := []int{10000, 10001, 10002}
	rules := ipv6FirewallRules(ports)

	var inputDrops int
	for _, r := range rules {
		if r.chain != "INPUT" {
			continue
		}
		inputDrops++
		if !r.insert {
			t.Errorf("INPUT drop should insert-first: %v", r)
		}
		s := joinRule(r.match)
		if !strings.Contains(s, "-p tcp") || !strings.Contains(s, "--dport") || !strings.Contains(s, "-j DROP") {
			t.Errorf("v6 INPUT rule %q not a TCP-dport DROP", s)
		}
	}
	want := len(ports) * len(deniedRanges6)
	if inputDrops != want {
		t.Errorf("got %d v6 INPUT drops, want %d (ports × ranges)", inputDrops, want)
	}
}

// TestIPv6FirewallRules_PortsPresent asserts each control port shows up in
// the emitted v6 INPUT rule set.
func TestIPv6FirewallRules_PortsPresent(t *testing.T) {
	rules := ipv6FirewallRules([]int{10000, 10001, 10002})
	for _, port := range []string{"10000", "10001", "10002"} {
		found := false
		for _, r := range rules {
			if r.chain == "INPUT" && strings.Contains(joinRule(r.match), "--dport "+port+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no v6 INPUT drop found for control port %s", port)
		}
	}
}

// The dind port rules are the authenticator for an unauthenticated Docker
// API, so both their content and their ORDER are load-bearing: iptables takes
// the first match, and an ACCEPT that lands after the DROP authorizes nobody
// while a DROP that lands after a subnet-wide ACCEPT denies nobody.
func TestDindPortRules_AcceptsOwnerThenDeniesEveryoneElse(t *testing.T) {
	gateway := "10.88.0.1"
	containerIP := "10.88.0.7"
	port := 41235

	rules := dindPortRules(gateway, containerIP, port)
	if len(rules) != 2 {
		t.Fatalf("dindPortRules returned %d rules, want 2 (accept owner, deny rest)", len(rules))
	}

	accept := joinRule(rules[0])
	for _, want := range []string{"-s 10.88.0.7/32", "-d 10.88.0.1", "-p tcp", "--dport 41235", "-j ACCEPT"} {
		if !strings.Contains(accept, want) {
			t.Errorf("accept rule %q missing %q", accept, want)
		}
	}

	deny := joinRule(rules[1])
	for _, want := range []string{"-d 10.88.0.1", "-p tcp", "--dport 41235", "-j DROP"} {
		if !strings.Contains(deny, want) {
			t.Errorf("deny rule %q missing %q", deny, want)
		}
	}
	// A source-scoped deny would leave the port open to anything outside the
	// container subnet that can route to the gateway. The port serves exactly
	// one container, so the deny is total.
	if strings.Contains(deny, "-s ") {
		t.Errorf("deny rule %q is source-scoped; it must deny every source but the carved-out /32", deny)
	}
}

// Two concurrent jobs must not be able to reach each other's daemon: each
// job's pair names its own port, so neither job's ACCEPT can match the other
// job's port and each port keeps its own total DROP.
func TestDindPortRules_ConcurrentJobsCannotCrossOver(t *testing.T) {
	gateway := "10.88.0.1"
	jobA := dindPortRules(gateway, "10.88.0.7", 41235)
	jobB := dindPortRules(gateway, "10.88.0.8", 41236)

	acceptA := joinRule(jobA[0])
	if strings.Contains(acceptA, "--dport 41236") {
		t.Errorf("job A's accept %q matches job B's port", acceptA)
	}
	acceptB := joinRule(jobB[0])
	if strings.Contains(acceptB, "10.88.0.7/32") {
		t.Errorf("job B's accept %q admits job A's container", acceptB)
	}
	// Job B's container hitting job A's port matches no ACCEPT (wrong source
	// on the only rule that names 41235) and falls to job A's DROP.
	denyA := joinRule(jobA[1])
	if !strings.Contains(denyA, "--dport 41235") || !strings.Contains(denyA, "-j DROP") {
		t.Errorf("job A's deny %q does not cover job A's port", denyA)
	}
}

// The port is the whole scope on the deny side, so a caller passing an
// address the CNI result did not produce must fail rather than open
// something wider or something wrong.
func TestOpenHostPort_RejectsUnusableContainerIP(t *testing.T) {
	l := &linuxNetworking{cfg: Config{Subnet: "10.88.0.0/16", Log: testLogger()}}
	for _, ip := range []string{"", "   ", "not-an-ip", "fd00::1", "10.88.0.7/16"} {
		if err := l.openHostPort(41235, ip); err == nil {
			t.Errorf("openHostPort with container IP %q returned nil; it must fail closed", ip)
		}
	}
}

func TestOpenHostPort_RejectsOutOfRangePort(t *testing.T) {
	l := &linuxNetworking{cfg: Config{Subnet: "10.88.0.0/16", Log: testLogger()}}
	for _, port := range []int{0, -1, 70000} {
		if err := l.openHostPort(port, "10.88.0.7"); err == nil {
			t.Errorf("openHostPort with port %d returned nil; it must fail closed", port)
		}
	}
}
