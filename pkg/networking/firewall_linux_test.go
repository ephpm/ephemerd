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

// ---------------------------------------------------------------------------
// EPHEMERD-INPUT — the container→host policy
// ---------------------------------------------------------------------------

const (
	testSubnet  = "10.88.0.0/16"
	testGateway = "10.88.0.1"
)

// findInputRule returns the first EPHEMERD-INPUT rule containing every given
// fragment, and whether one was found.
func findInputRule(rules [][]string, fragments ...string) (string, bool) {
	for _, r := range rules {
		s := joinRule(r)
		ok := true
		for _, f := range fragments {
			if !strings.Contains(s, f) {
				ok = false
				break
			}
		}
		if ok {
			return s, true
		}
	}
	return "", false
}

// TestContainerInputRules_DefaultDeny is the core of the fix for the
// container→gateway bypass. Before EPHEMERD-INPUT existed, container traffic
// addressed to a local host address was delivered over INPUT, where a
// bare-metal node had no ephemerd rules at all — so the host's ACCEPT policy
// decided the outcome and GatewayPorts constrained nothing. The chain must
// therefore END in an unconditional DROP, not rely on any policy.
func TestContainerInputRules_DefaultDeny(t *testing.T) {
	rules := containerInputRules(testGateway, []int{8082}, nil)
	if len(rules) == 0 {
		t.Fatal("no rules emitted")
	}

	last := joinRule(rules[len(rules)-1])
	if last != inputChainName+" -j DROP" {
		t.Errorf("last rule = %q, want an unconditional %s -j DROP — anything else inherits the host's INPUT policy", last, inputChainName)
	}

	// The DROP must be unconditional: no address, protocol or port match that
	// would let traffic slip past it.
	for _, forbidden := range []string{"-d ", "-p ", "--dport", "-s "} {
		if strings.Contains(last, forbidden) {
			t.Errorf("default deny %q is qualified by %q; it must catch everything", last, forbidden)
		}
	}
}

// TestContainerInputRules_AllowsOnlyConfiguredGatewayPorts pins the property
// the GatewayPorts allow-list was supposed to have all along: a configured port
// is allowed to the gateway, and a port that is not configured is denied.
func TestContainerInputRules_AllowsOnlyConfiguredGatewayPorts(t *testing.T) {
	rules := containerInputRules(testGateway, []int{8082}, nil)

	// Configured: an explicit ACCEPT to the gateway on that TCP port.
	if _, ok := findInputRule(rules, "-d "+testGateway, "-p tcp", "--dport 8082", "-j ACCEPT"); !ok {
		t.Errorf("configured gateway port 8082 is not allowed; rules = %v", rules)
	}

	// Not configured: no ACCEPT mentions it, so it falls through to the DROP.
	// 10000 is containerd, 22 is sshd, 9090 is a plausible host metrics port.
	for _, port := range []string{"22", "9090", "10000", "10001"} {
		if s, ok := findInputRule(rules, "--dport "+port+" ", "-j ACCEPT"); ok {
			t.Errorf("unconfigured port %s is allowed by %q; only GatewayPorts and DNS may be", port, s)
		}
	}
}

// TestContainerInputRules_AllowsDNSToGateway keeps the carve-out the FORWARD
// chain makes: DNS to the bridge gateway, udp and tcp. A container whose
// resolv.conf points at the gateway must keep resolving.
func TestContainerInputRules_AllowsDNSToGateway(t *testing.T) {
	rules := containerInputRules(testGateway, nil, nil)
	for _, proto := range []string{"udp", "tcp"} {
		if _, ok := findInputRule(rules, "-d "+testGateway, "-p "+proto, "--dport 53", "-j ACCEPT"); !ok {
			t.Errorf("DNS over %s to the gateway is not allowed; rules = %v", proto, rules)
		}
	}
}

// TestContainerInputRules_AllowsLoopbackAndEstablished guards the two carve-outs
// that keep the default deny from breaking the host itself.
//
// Loopback: the host's own processes reach services they bind on the bridge
// address (the module proxy) over `lo` with a source inside the container
// subnet, so the jump matches them. A container cannot forge this — it lives in
// its own netns and its packets arrive on the bridge, never on `lo`.
//
// RELATED,ESTABLISHED: replies to connections the HOST opened toward a
// container. It cannot be used to reach a denied port, because the opening SYN
// is ctstate NEW and hits the DROP.
func TestContainerInputRules_AllowsLoopbackAndEstablished(t *testing.T) {
	rules := containerInputRules(testGateway, nil, nil)

	if got := joinRule(rules[0]); got != inputChainName+" -i lo -j ACCEPT" {
		t.Errorf("first rule = %q, want the loopback accept — the host must not filter its own traffic", got)
	}
	if _, ok := findInputRule(rules, "-m conntrack", "--ctstate RELATED,ESTABLISHED", "-j ACCEPT"); !ok {
		t.Errorf("no RELATED,ESTABLISHED accept; host→container replies would be dropped. rules = %v", rules)
	}
}

// TestContainerInputRules_NeverOpensAControlPort proves a misconfiguration
// cannot undo the control-plane block: listing containerd or the dispatch gRPC
// port under GatewayPorts must not produce an allow for it. Without this the
// outcome would depend on whether the control-plane DROPs happened to be
// ordered above the chain jump.
func TestContainerInputRules_NeverOpensAControlPort(t *testing.T) {
	control := []int{10000, 10001, 10002}
	// An operator lists the dispatch port alongside the module proxy.
	rules := containerInputRules(testGateway, []int{8082, 10001}, control)

	for _, port := range []string{"10000", "10001", "10002"} {
		if s, ok := findInputRule(rules, "--dport "+port+" ", "-j ACCEPT"); ok {
			t.Errorf("control port %s was opened by GatewayPorts: %q", port, s)
		}
	}
	// The legitimate port still comes through.
	if _, ok := findInputRule(rules, "--dport 8082", "-j ACCEPT"); !ok {
		t.Error("the module proxy port was dropped along with the control ports")
	}
}

// TestContainerInputRules_AllowsAreGatewayScoped pins that no allow is written
// without a destination. An allow scoped only by port would also cover the
// host's LAN address — which is the other half of what INPUT delivery exposes,
// since the FORWARD RFC1918 REJECTs never see locally delivered traffic.
func TestContainerInputRules_AllowsAreGatewayScoped(t *testing.T) {
	rules := containerInputRules(testGateway, []int{8082, 5000}, nil)
	for _, r := range rules {
		s := joinRule(r)
		if !strings.Contains(s, "-j ACCEPT") {
			continue
		}
		if strings.Contains(s, "-i lo") || strings.Contains(s, "--ctstate") {
			continue // the two deliberate non-address carve-outs
		}
		if !strings.Contains(s, "-d "+testGateway) {
			t.Errorf("accept %q is not scoped to the gateway; it would also open the host's LAN address", s)
		}
	}
}

// TestContainerInputRules_SkipsGarbagePortsAndDuplicates keeps the emitted set
// clean when the config is not.
func TestContainerInputRules_SkipsGarbagePortsAndDuplicates(t *testing.T) {
	rules := containerInputRules(testGateway, []int{8082, 8082, 0, -1, 70000, 53}, nil)

	count := func(fragment string) int {
		n := 0
		for _, r := range rules {
			if strings.Contains(joinRule(r), fragment) {
				n++
			}
		}
		return n
	}
	if got := count("--dport 8082"); got != 1 {
		t.Errorf("port 8082 emitted %d times, want 1", got)
	}
	// 53 is already allowed for udp+tcp; it must not get a third rule.
	if got := count("--dport 53"); got != 2 {
		t.Errorf("port 53 emitted %d times, want 2 (udp + tcp)", got)
	}
	for _, bad := range []string{"--dport 0", "--dport -1", "--dport 70000"} {
		if count(bad) != 0 {
			t.Errorf("out-of-range port emitted: %q", bad)
		}
	}
}

// TestContainerInputRules_EmptyWithoutGateway guards the nil case.
func TestContainerInputRules_EmptyWithoutGateway(t *testing.T) {
	if rules := containerInputRules("", []int{8082}, nil); rules != nil {
		t.Errorf("rules emitted with no gateway: %v", rules)
	}
}

// TestContainerInputJumpRule_ScopedToTheContainerSubnet pins the jump. Matching
// on source (not on the bridge interface) also catches a packet arriving from
// the LAN with a forged container address, which would otherwise reach INPUT
// unfiltered.
func TestContainerInputJumpRule_ScopedToTheContainerSubnet(t *testing.T) {
	got := joinRule(containerInputJumpRule(testSubnet))
	want := "INPUT -s " + testSubnet + " -j " + inputChainName
	if got != want {
		t.Errorf("jump rule = %q, want %q", got, want)
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
