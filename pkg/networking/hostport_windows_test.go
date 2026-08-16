//go:build windows

package networking

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestHostPortAllowRule_ScopedToOneContainerAndPort proves the dind-over-
// L2Bridge allow opens exactly one host port to exactly ONE container. The VFP
// host /32 allow lets a container's packet leave toward the host, but the
// host's own inbound Windows Firewall default-denies it — so the per-job dind
// listener is unreachable without this rule. It must stay tightly scoped in
// both axes: a broad port scope would expose RDP/SMB/RPC on the host, and a
// broad address scope hands every job container the unauthenticated Docker API
// of every other job.
func TestHostPortAllowRule_ScopedToOneContainerAndPort(t *testing.T) {
	const (
		hostIP      = "192.0.2.10"
		containerIP = "192.0.2.200"
		port        = 63933
	)
	r := hostPortAllowRule(hostIP, containerIP, port)
	spec := strings.Join(r.spec, " ")

	for _, want := range []string{
		"dir=in", "action=allow", "protocol=TCP",
		"localip=" + hostIP, "localport=63933", "remoteip=" + containerIP + "/32",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("host-port allow rule missing %q; spec = %q", want, spec)
		}
	}
	// Must be port-scoped, not a blanket host allow.
	if strings.Contains(spec, "localport=any") || !strings.Contains(spec, "localport=63933") {
		t.Errorf("rule is not scoped to the single dind port: %q", spec)
	}
	// The name must fall under the prefix the shutdown sweep matches, or a
	// hard-killed job leaks its allow forever.
	if !strings.HasPrefix(r.name, hostPortRulePrefix) {
		t.Errorf("rule name %q must start with the sweep prefix %q", r.name, hostPortRulePrefix)
	}
}

// TestHostPortAllowRule_NeverScopedToThePool is the regression guard for the
// cross-job hole: the allow used to carry remoteip=<ip_pool>, so container A
// could port-scan the host's LAN address, find container B's dind endpoint and
// drive B's daemon (the fake Docker API authenticates nothing). Any rule whose
// remote scope is wider than a single address reopens that hole.
func TestHostPortAllowRule_NeverScopedToThePool(t *testing.T) {
	const (
		hostIP      = "192.0.2.10"
		pool        = "192.0.2.192/27"
		containerIP = "192.0.2.200"
	)
	remote := specValue(hostPortAllowRule(hostIP, containerIP, 63933), "remoteip")

	if remote != containerIP+"/32" {
		t.Errorf("remote scope is %q; want the owning container's /32 (%s/32)", remote, containerIP)
	}
	if strings.Contains(remote, pool) {
		t.Errorf("remote scope %q is the container pool — every job could reach every other job's Docker API", remote)
	}
	// Belt and braces: whatever form it takes, it must cover exactly one host.
	if _, ipnet, err := net.ParseCIDR(remote); err != nil {
		t.Errorf("remoteip %q is not a CIDR: %v", remote, err)
	} else if ones, bits := ipnet.Mask.Size(); ones != bits {
		t.Errorf("remoteip %q covers %d addresses; the allow must cover exactly one", remote, 1<<(bits-ones))
	}
}

// TestHostPortRuleName_UniquePerContainerAndPort proves two concurrent jobs can
// never collide on a rule name. Both open/close paths do an idempotent
// delete-by-name first, so a shared name would let one job's teardown — or its
// delete-before-add — silently revoke a running job's allow.
func TestHostPortRuleName_UniquePerContainerAndPort(t *testing.T) {
	const hostIP = "192.0.2.10"

	// Two jobs, different containers, different ephemeral ports (the usual case).
	a := hostPortAllowRule(hostIP, "192.0.2.200", 63933)
	b := hostPortAllowRule(hostIP, "192.0.2.201", 51001)
	if a.name == b.name {
		t.Errorf("concurrent jobs share the rule name %q", a.name)
	}

	// Same port, different containers: a port can repeat across a daemon
	// restart or across the pool's lifetime, so the address must disambiguate.
	c := hostPortAllowRule(hostIP, "192.0.2.201", 63933)
	if a.name == c.name {
		t.Errorf("rule name %q does not distinguish containers on the same port", a.name)
	}

	// Same container, different ports: the port must disambiguate too.
	d := hostPortAllowRule(hostIP, "192.0.2.200", 51001)
	if a.name == d.name {
		t.Errorf("rule name %q does not distinguish ports for the same container", a.name)
	}

	// Every one of them stays under the shutdown sweep prefix.
	for _, r := range []winFirewallRule{a, b, c, d} {
		if !strings.HasPrefix(r.name, hostPortRulePrefix) {
			t.Errorf("rule name %q must start with the sweep prefix %q", r.name, hostPortRulePrefix)
		}
	}
}

// TestNormalizeContainerIP_FailsClosed proves the gate in front of every allow
// rejects the inputs that would otherwise tempt a pool-wide fallback. openHostPort
// turns each of these into an error and installs nothing.
func TestNormalizeContainerIP_FailsClosed(t *testing.T) {
	for _, bad := range []string{
		"",                        // no address plumbed through at all
		"   ",                     // whitespace
		"192.0.2.192/27",          // a pool, not a host — the exact bug
		"192.0.2.200-192.0.2.230", // a range
		"not-an-ip",
		"2001:db8::1", // IPv6: the Windows container stack has no v6 path
	} {
		if got, err := normalizeContainerIP(bad); err == nil {
			t.Errorf("normalizeContainerIP(%q) accepted it as %q; must fail closed", bad, got)
		}
	}
	// A good address normalizes to canonical dotted-quad, surrounding
	// whitespace and all.
	got, err := normalizeContainerIP("  192.0.2.200 ")
	if err != nil {
		t.Fatalf("normalizeContainerIP rejected a valid address: %v", err)
	}
	if got != "192.0.2.200" {
		t.Errorf("normalizeContainerIP = %q, want %q", got, "192.0.2.200")
	}
}

// TestL2BridgeHostHardenRules_BlocksDangerousManagementPorts is the regression
// guard for the 2026-08-15 pen-test finding: with only the VFP host /32 allow,
// a job container could reach the host's WinRM (5985) and RPC endpoint mapper
// (135) — every port the host firewall ambiently permits is exposed, because
// VFP allows can't be port-scoped. The backstop must block the dangerous
// management ports from the pool as inbound TCP blocks.
func TestL2BridgeHostHardenRules_BlocksDangerousManagementPorts(t *testing.T) {
	const (
		hostIP = "192.0.2.10"
		pool   = "192.0.2.192/27"
	)
	byProtoPort := hardenRulesByProtoPort(t, hostIP, pool)

	// The two the pen test actually caught, plus the rest of the management set.
	for _, port := range []string{"135", "139", "445", "3389", "5985", "5986", "47001"} {
		spec, ok := byProtoPort["TCP/"+port]
		if !ok {
			t.Errorf("dangerous host port TCP/%s is not blocked from the pool", port)
			continue
		}
		for _, want := range []string{"dir=in", "action=block", "protocol=TCP", "localip=" + hostIP, "remoteip=" + pool} {
			if !strings.Contains(spec, want) {
				t.Errorf("host-harden rule for TCP/%s missing %q; spec=%q", port, want, spec)
			}
		}
	}
	// Must never block the ephemeral range dind binds — that would break docker.
	for _, k := range []string{"TCP/0", "UDP/0", "TCP/any", "UDP/any"} {
		if _, blocked := byProtoPort[k]; blocked {
			t.Errorf("host-harden must not contain an all-ports block (%s) — it would kill the dind listener", k)
		}
	}
}

// hardenRulesByProtoPort indexes the emitted harden set by "PROTO/port". The
// key has to carry the protocol now that the same port number appears on both
// TCP and UDP — indexing by port alone would silently hide half the set, and a
// rule NAME that collided would make each rule's idempotent delete-before-add
// remove its counterpart.
func hardenRulesByProtoPort(t *testing.T, hostIP, pool string) map[string]string {
	t.Helper()
	rules := l2BridgeHostHardenRules(hostIP, pool)

	names := map[string]bool{}
	out := map[string]string{}
	for _, r := range rules {
		if names[r.name] {
			t.Errorf("duplicate rule name %q — the delete-before-add would revoke the other rule", r.name)
		}
		names[r.name] = true
		if !strings.HasPrefix(r.name, hardenRulePrefix) {
			t.Errorf("rule name %q must start with the sweep prefix %q, or an upgrade leaks it", r.name, hardenRulePrefix)
		}
		out[specValue(r, "protocol")+"/"+specValue(r, "localport")] = strings.Join(r.spec, " ")
	}
	return out
}

// TestL2BridgeHostHardenRules_CoversUDP is the regression guard for issue #153:
// every harden rule used to be protocol=TCP, so the host's UDP listeners on the
// same management services stayed reachable from the pool while the docs claimed
// those ports were blocked.
func TestL2BridgeHostHardenRules_CoversUDP(t *testing.T) {
	const (
		hostIP = "192.0.2.10"
		pool   = "192.0.2.192/27"
	)
	byProtoPort := hardenRulesByProtoPort(t, hostIP, pool)

	for _, port := range []string{"135", "139", "445", "3389", "5985", "5986", "47001"} {
		spec, ok := byProtoPort["UDP/"+port]
		if !ok {
			t.Errorf("host port UDP/%s is not blocked from the pool (issue #153: the set was TCP-only)", port)
			continue
		}
		for _, want := range []string{"dir=in", "action=block", "protocol=UDP", "localip=" + hostIP, "remoteip=" + pool} {
			if !strings.Contains(spec, want) {
				t.Errorf("host-harden rule for UDP/%s missing %q; spec=%q", port, want, spec)
			}
		}
	}
}

// TestL2BridgeHostHardenRules_NameResolutionIsNotLocalIPScoped is the subtle
// half of issue #153. LLMNR (5355) and mDNS (5353) arrive at multicast groups
// and NBNS (137) at the subnet broadcast, so the packet's local address is the
// group/broadcast address — NOT the host's unicast address. A rule carrying
// localip=<hostIP> would not match them at all, which is how a "we block those
// ports" rule set can end up blocking nothing. These must be pool-scoped on the
// remote side and unscoped on the local side.
func TestL2BridgeHostHardenRules_NameResolutionIsNotLocalIPScoped(t *testing.T) {
	const (
		hostIP = "192.0.2.10"
		pool   = "192.0.2.192/27"
	)
	rules := l2BridgeHostHardenRules(hostIP, pool)

	want := map[string]bool{"137": false, "138": false, "5353": false, "5355": false}
	for _, r := range rules {
		port := specValue(r, "localport")
		if _, tracked := want[port]; !tracked {
			continue
		}
		if specValue(r, "protocol") != "UDP" {
			continue
		}
		if lip := specValue(r, "localip"); lip != "" {
			t.Errorf("name-resolution block for UDP/%s carries localip=%s; multicast/broadcast traffic never has the host's unicast address as its local address, so this rule matches nothing", port, lip)
		}
		if got := specValue(r, "remoteip"); got != pool {
			t.Errorf("name-resolution block for UDP/%s has remoteip=%q, want the pool %q — it must not block the host's own LAN name resolution", port, got, pool)
		}
		if got := specValue(r, "action"); got != "block" {
			t.Errorf("name-resolution rule for UDP/%s has action=%q, want block", port, got)
		}
		want[port] = true
	}
	for port, seen := range want {
		if !seen {
			t.Errorf("no name-resolution block emitted for UDP/%s (LLMNR/NBNS/mDNS poisoning position)", port)
		}
	}
}

// TestL2BridgeHostHardenRules_DoesNotTouchTheDindAllow proves the widened
// hardening cannot collide with the per-container dind allow. The allow is
// TCP-only on an ephemeral port scoped to one container's /32; nothing in the
// harden set may block that port, and no harden rule may borrow its name.
func TestL2BridgeHostHardenRules_DoesNotTouchTheDindAllow(t *testing.T) {
	const (
		hostIP      = "192.0.2.10"
		pool        = "192.0.2.192/27"
		containerIP = "192.0.2.200"
		dindPort    = 63933
	)
	allow := hostPortAllowRule(hostIP, containerIP, dindPort)

	// The dind allow is unchanged: TCP, single port, single container.
	if got := specValue(allow, "protocol"); got != "TCP" {
		t.Errorf("dind allow protocol = %q, want TCP (the Docker API is TCP only)", got)
	}
	if got := specValue(allow, "remoteip"); got != containerIP+"/32" {
		t.Errorf("dind allow remoteip = %q, want the owning container's /32", got)
	}

	for _, r := range l2BridgeHostHardenRules(hostIP, pool) {
		if specValue(r, "localport") == strconv.Itoa(dindPort) {
			t.Errorf("harden rule %q blocks the dind port %d", r.name, dindPort)
		}
		if r.name == allow.name {
			t.Errorf("harden rule shares the dind allow's name %q; the delete-before-add would revoke it", r.name)
		}
		// A harden rule must never carry a /32 remote scope: that is the
		// per-job allow's shape, and a block there would be attributed to one
		// container while the rest of the pool stayed open.
		if strings.HasSuffix(specValue(r, "remoteip"), "/32") {
			t.Errorf("harden rule %q is scoped to a single container; it must cover the whole pool", r.name)
		}
	}
}

// TestL2BridgeHostHardenRules_NeverAllows pins that the harden set is blocks
// only. An action=allow slipping in here would widen the host, not narrow it.
func TestL2BridgeHostHardenRules_NeverAllows(t *testing.T) {
	for _, r := range l2BridgeHostHardenRules("192.0.2.10", "192.0.2.192/27") {
		if got := specValue(r, "action"); got != "block" {
			t.Errorf("harden rule %q has action=%q, want block", r.name, got)
		}
		if got := specValue(r, "dir"); got != "in" {
			t.Errorf("harden rule %q has dir=%q, want in", r.name, got)
		}
	}
}

// TestL2BridgeHostHardenRules_EmptyWithoutPlan guards the nil-safety.
func TestL2BridgeHostHardenRules_EmptyWithoutPlan(t *testing.T) {
	if len(l2BridgeHostHardenRules("", "")) != 0 {
		t.Error("no rules should be produced without a host IP and pool")
	}
}
