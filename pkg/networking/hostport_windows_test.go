//go:build windows

package networking

import (
	"net"
	"strings"
	"testing"
)

// natNetworking builds a windowsNetworking on the default NAT path: no
// L2Bridge, no address plan — exactly the shape of a node with no [network]
// section in its config, which is what mfl-win-amd64-101 was running when #162
// was reproduced.
func natNetworking(subnet string) *windowsNetworking {
	return &windowsNetworking{cfg: Config{Subnet: subnet}}
}

// l2bNetworking builds a windowsNetworking on the L2Bridge path with a
// resolved plan.
func l2bNetworking(hostIP, pool string) *windowsNetworking {
	return &windowsNetworking{
		cfg:  Config{L2BridgeEgress: true},
		plan: &l2BridgePlan{HostIP: hostIP, PoolSpec: pool},
	}
}

// TestHostPortRuleFor_NATOpensAScopedAllow is the regression test for #162.
//
// On the default NAT network the Windows dind Docker API listens on the bridge
// gateway over TCP (runhcs has no unix socket and no named-pipe sharing), so a
// job container reaching its own daemon is making an INBOUND connection to the
// host — which the host's Windows Firewall default-denies. openHostPort used to
// return early unless L2Bridge egress was on, so a NAT node installed nothing
// and every docker command in every Windows job died with
// "dial tcp 10.88.0.1:<port>: i/o timeout".
//
// Proven on metal 2026-08-16: with a temporary inbound allow admitting
// 10.88.0.0/16 to 10.88.0.1, a `docker login` that had timed out minutes
// earlier on the same node succeeded.
func TestHostPortRuleFor_NATOpensAScopedAllow(t *testing.T) {
	const (
		containerIP = "10.88.10.204" // a real address HNS handed a job on that node
		port        = 51098          // the port its dind actually bound
	)
	w := natNetworking("") // no subnet configured — the default 10.88.0.0/16

	r, needed, err := w.hostPortRuleFor(port, containerIP)
	if err != nil {
		t.Fatalf("NAT path refused to build a host-port rule: %v", err)
	}
	if !needed {
		t.Fatal("NAT path installed no host-port allow — this is #162: the container's dind SYN is silently dropped and every docker command times out")
	}

	spec := strings.Join(r.spec, " ")
	for _, want := range []string{
		"dir=in", "action=allow", "protocol=TCP",
		"localip=10.88.0.1", "localport=51098", "remoteip=" + containerIP + "/32",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("NAT host-port allow missing %q; spec = %q", want, spec)
		}
	}
	if !strings.HasPrefix(r.name, hostPortRulePrefix) {
		t.Errorf("rule name %q must start with the sweep prefix %q, or a hard-killed job leaks it", r.name, hostPortRulePrefix)
	}
}

// TestHostPortRuleFor_NATNeverScopedToTheSubnet carries #152's guarantee onto
// the NAT path. Every job container on a NAT node shares 10.88.0.0/16 and can
// address the gateway, so an allow scoped to the subnet would let one job
// port-scan the gateway's ephemeral range, find another job's dind endpoint and
// drive its daemon — the Docker API behind these ports authenticates nothing.
// Fixing #162 must not reintroduce the hole #152 closed.
func TestHostPortRuleFor_NATNeverScopedToTheSubnet(t *testing.T) {
	w := natNetworking(DefaultSubnet)

	r, needed, err := w.hostPortRuleFor(51098, "10.88.10.204")
	if err != nil || !needed {
		t.Fatalf("hostPortRuleFor(NAT) = needed %v, err %v; want a rule", needed, err)
	}
	remote := specValue(r, "remoteip")

	if strings.Contains(remote, DefaultSubnet) || remote == DefaultSubnet {
		t.Fatalf("remote scope %q is the whole container subnet — every job could reach every other job's Docker API", remote)
	}
	_, ipnet, err := net.ParseCIDR(remote)
	if err != nil {
		t.Fatalf("remoteip %q is not a CIDR: %v", remote, err)
	}
	if ones, bits := ipnet.Mask.Size(); ones != bits {
		t.Errorf("remoteip %q covers %d addresses; the allow must cover exactly one", remote, 1<<(bits-ones))
	}
	// The allow must also stay port-scoped: an all-ports allow to the gateway
	// would expose everything else ephemerd binds there (the module proxy, and
	// on a Linux-VM host the dispatch gRPC control plane).
	if p := specValue(r, "localport"); p != "51098" {
		t.Errorf("localport = %q; the allow must open exactly the dind port", p)
	}
}

// TestHostPortLocalIP_NATTracksTheConfiguredSubnet proves the allow is scoped
// to the address dind actually bound rather than a hard-coded 10.88.0.1.
// networking.pickSubnet moves the container subnet when 10.88.0.0/16 is already
// in use on the host, and Manager.GatewayIP — which is what dind binds — follows
// it. A rule scoped to the wrong address is an allow that admits nothing, which
// is indistinguishable from the bug being fixed here.
func TestHostPortLocalIP_NATTracksTheConfiguredSubnet(t *testing.T) {
	for _, tc := range []struct{ subnet, want string }{
		{"", "10.88.0.1"},
		{DefaultSubnet, "10.88.0.1"},
		{"10.199.0.0/16", "10.199.0.1"},
		{"172.20.0.0/16", "172.20.0.1"},
		{"not-a-cidr", "10.88.0.1"}, // same fallback GatewayIP uses
	} {
		w := natNetworking(tc.subnet)
		got, needed := w.hostPortLocalIP()
		if !needed {
			t.Errorf("subnet %q: NAT must always install an allow", tc.subnet)
			continue
		}
		if got != tc.want {
			t.Errorf("subnet %q: host-port localip = %q, want %q", tc.subnet, got, tc.want)
		}
		// And it must agree with what dind is told to bind to.
		m := &Manager{cfg: Config{Subnet: tc.subnet}}
		if gw := m.GatewayIP(); gw != got {
			t.Errorf("subnet %q: allow scoped to %q but dind binds %q — the rule would admit nothing", tc.subnet, got, gw)
		}
	}
}

// TestHostPortRuleFor_L2BridgeUnchanged pins the L2Bridge behaviour the NAT fix
// had to leave alone: scoped to the host's own LAN address (not any bridge
// gateway, which does not exist there), and no rule at all when the address
// plan never resolved.
func TestHostPortRuleFor_L2BridgeUnchanged(t *testing.T) {
	const (
		hostIP      = "192.0.2.10"
		containerIP = "192.0.2.200"
	)
	w := l2bNetworking(hostIP, "192.0.2.192/27")

	r, needed, err := w.hostPortRuleFor(63933, containerIP)
	if err != nil || !needed {
		t.Fatalf("hostPortRuleFor(L2Bridge) = needed %v, err %v; want a rule", needed, err)
	}
	if got := specValue(r, "localip"); got != hostIP {
		t.Errorf("L2Bridge allow localip = %q, want the host's LAN address %q", got, hostIP)
	}
	if got := specValue(r, "remoteip"); got != containerIP+"/32" {
		t.Errorf("L2Bridge allow remoteip = %q, want %s/32", got, containerIP)
	}

	// L2Bridge enabled but no plan: init failed or the daemon is tearing down.
	// Guessing the NAT gateway here would scope the rule to an address that
	// exists on no interface once the NAT network is out of the picture.
	for _, noPlan := range []*windowsNetworking{
		{cfg: Config{L2BridgeEgress: true}},
		{cfg: Config{L2BridgeEgress: true}, plan: &l2BridgePlan{}},
	} {
		if _, needed, err := noPlan.hostPortRuleFor(63933, containerIP); needed || err != nil {
			t.Errorf("L2Bridge without a resolved plan built a rule (needed=%v, err=%v); it must install nothing", needed, err)
		}
	}
}

// TestHostPortRuleFor_TeardownTargetsExactlyWhatSetupAdded proves closeHostPort
// removes the rule openHostPort added and no other. Both go through
// hostPortRuleFor, so the property to pin is that the same inputs yield the
// same rule name — which is what netsh delete matches on — and that any
// different container or port yields a different one.
func TestHostPortRuleFor_TeardownTargetsExactlyWhatSetupAdded(t *testing.T) {
	w := natNetworking(DefaultSubnet)

	opened, _, err := w.hostPortRuleFor(51098, "10.88.10.204")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closed, _, err := w.hostPortRuleFor(51098, "10.88.10.204")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.name != opened.name {
		t.Fatalf("teardown targets %q but setup added %q — the allow leaks", closed.name, opened.name)
	}
	// netsh deletes by name, so the delete argv must name exactly that rule.
	del := strings.Join(closed.deleteArgs(), " ")
	if !strings.Contains(del, "name="+opened.name) {
		t.Errorf("delete argv %q does not target the rule setup added (%q)", del, opened.name)
	}

	// A concurrent job must not be able to delete this one's allow. max_concurrent
	// is routinely > 1, and every open does a delete-by-name first.
	for _, other := range []struct {
		ip   string
		port int
	}{
		{"10.88.10.205", 51098}, // same port, different container
		{"10.88.10.204", 51099}, // same container, different port
		{"10.88.10.205", 51099},
	} {
		r, _, err := w.hostPortRuleFor(other.port, other.ip)
		if err != nil {
			t.Fatalf("hostPortRuleFor(%d, %s): %v", other.port, other.ip, err)
		}
		if r.name == opened.name {
			t.Errorf("job (%s, %d) shares the rule name %q — its teardown would revoke a running job's allow", other.ip, other.port, r.name)
		}
	}
}

// TestHostPortRuleFor_NATFailsClosed proves the NAT path inherits the
// fail-closed gate rather than quietly installing nothing (its old behaviour)
// or widening to the subnet. An unknown address costs this job its Docker
// access; it must never cost every other job its isolation.
func TestHostPortRuleFor_NATFailsClosed(t *testing.T) {
	w := natNetworking(DefaultSubnet)

	for _, bad := range []string{
		"",               // no address plumbed through from the HCN endpoint
		"   ",            //
		"10.88.0.0/16",   // the subnet, not a host — the exact bug #152 closed
		"10.88.0.1-10.88.0.9", // a range
		"not-an-ip",
		"2001:db8::1", // IPv6: the Windows container stack has no v6 path
	} {
		r, needed, err := w.hostPortRuleFor(51098, bad)
		if err == nil {
			t.Errorf("hostPortRuleFor(%q) succeeded; must fail closed", bad)
		}
		if needed {
			t.Errorf("hostPortRuleFor(%q) reported a rule is needed; it must install nothing", bad)
		}
		if r.name != "" || len(r.spec) != 0 {
			t.Errorf("hostPortRuleFor(%q) returned a rule %+v; must return none", bad, r)
		}
	}
}

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
	rules := l2BridgeHostHardenRules(hostIP, pool)

	byPort := map[string]string{} // port -> joined spec
	for _, r := range rules {
		byPort[specValue(r, "localport")] = strings.Join(r.spec, " ")
	}
	// The two the pen test actually caught, plus the rest of the management set.
	for _, port := range []string{"135", "139", "445", "3389", "5985", "5986", "47001"} {
		spec, ok := byPort[port]
		if !ok {
			t.Errorf("dangerous host port %s is not blocked from the pool", port)
			continue
		}
		for _, want := range []string{"dir=in", "action=block", "protocol=TCP", "localip=" + hostIP, "remoteip=" + pool} {
			if !strings.Contains(spec, want) {
				t.Errorf("host-harden rule for port %s missing %q; spec=%q", port, want, spec)
			}
		}
	}
	// Must never block the ephemeral range dind binds — that would break docker.
	if _, blocked := byPort["0"]; blocked {
		t.Error("host-harden must not contain an all-ports/0 block — it would kill the dind listener")
	}
}

// TestL2BridgeHostHardenRules_EmptyWithoutPlan guards the nil-safety.
func TestL2BridgeHostHardenRules_EmptyWithoutPlan(t *testing.T) {
	if len(l2BridgeHostHardenRules("", "")) != 0 {
		t.Error("no rules should be produced without a host IP and pool")
	}
}
