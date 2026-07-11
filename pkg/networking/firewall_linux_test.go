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
