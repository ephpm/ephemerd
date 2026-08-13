//go:build windows

package networking

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/Microsoft/hcsshim/hcn"
)

// TestBuildEgressBlockPolicies verifies the fail-closed egress rule set (WIN-4).
// Every RFC1918 + link-local range must produce an outbound Block ACL; a
// partial or empty set would let a job reach the host LAN / other tenants /
// cloud metadata.
func TestBuildEgressBlockPolicies(t *testing.T) {
	policies, err := buildEgressBlockPolicies()
	if err != nil {
		t.Fatalf("buildEgressBlockPolicies: %v", err)
	}

	// Collect the CIDRs that actually became block rules.
	got := map[string]bool{}
	for _, p := range policies {
		if p.Type != hcn.ACL {
			t.Errorf("policy type = %v, want ACL", p.Type)
		}
		var acl hcn.AclPolicySetting
		if err := json.Unmarshal(p.Settings, &acl); err != nil {
			t.Fatalf("unmarshal ACL setting: %v", err)
		}
		if acl.Action != hcn.ActionTypeBlock {
			t.Errorf("CIDR %s action = %v, want Block", acl.RemoteAddresses, acl.Action)
		}
		if acl.Direction != hcn.DirectionTypeOut {
			t.Errorf("CIDR %s direction = %v, want Out", acl.RemoteAddresses, acl.Direction)
		}
		got[acl.RemoteAddresses] = true
	}

	// Every configured range (minus the container's own subnet) must be blocked.
	for _, cidr := range egressBlockedCIDRs {
		if cidr == DefaultSubnet {
			continue
		}
		if !got[cidr] {
			t.Errorf("egress range %s was not turned into a block rule", cidr)
		}
	}

	// Link-local / metadata range must always be present.
	if !got["169.254.0.0/16"] {
		t.Error("link-local/metadata range 169.254.0.0/16 not blocked")
	}
}

// decodeACLs unmarshals every EndpointPolicy back into an AclPolicySetting,
// failing the test on a non-ACL policy or a malformed setting.
func decodeACLs(t *testing.T, policies []hcn.EndpointPolicy) []hcn.AclPolicySetting {
	t.Helper()
	acls := make([]hcn.AclPolicySetting, 0, len(policies))
	for i, p := range policies {
		if p.Type != hcn.ACL {
			t.Errorf("policy[%d] type = %v, want ACL", i, p.Type)
		}
		var acl hcn.AclPolicySetting
		if err := json.Unmarshal(p.Settings, &acl); err != nil {
			t.Fatalf("unmarshal ACL[%d]: %v", i, err)
		}
		acls = append(acls, acl)
	}
	return acls
}

// TestL2BridgeEgressACLPolicies_LadderShape pins the full router-safe VFP
// ladder: the two mandatory allow-any rules (Out+In) and a whole-supernet block
// for every RFC1918 + link-local range — with the exact actions, directions,
// protocols, and priorities that were proven on metal.
func TestL2BridgeEgressACLPolicies_LadderShape(t *testing.T) {
	acls := decodeACLs(t, mustBuildL2Bridge(t, nil, ""))

	var (
		allowAnyOut, allowAnyIn bool
		blockedSupernets        = map[string]hcn.AclPolicySetting{}
	)
	for _, a := range acls {
		switch {
		case a.Action == hcn.ActionTypeAllow && a.RemoteAddresses == "0.0.0.0/0" && a.Direction == hcn.DirectionTypeOut:
			allowAnyOut = true
			assertACL(t, "allow-any-out", a, aclAnyProtocol, aclPriorityAllowAny)
		case a.Action == hcn.ActionTypeAllow && a.RemoteAddresses == "0.0.0.0/0" && a.Direction == hcn.DirectionTypeIn:
			allowAnyIn = true
			assertACL(t, "allow-any-in", a, aclAnyProtocol, aclPriorityAllowAny)
		case a.Action == hcn.ActionTypeBlock:
			blockedSupernets[a.RemoteAddresses] = a
		}
	}

	if !allowAnyOut || !allowAnyIn {
		t.Errorf("missing mandatory allow-any rule: out=%v in=%v (both required or the port default-denies / drops SYN-ACK)", allowAnyOut, allowAnyIn)
	}

	// Every RFC1918 + link-local supernet must be blocked, whole, Out, at the
	// block priority.
	for _, cidr := range egressBlockedCIDRs {
		a, ok := blockedSupernets[cidr]
		if !ok {
			t.Errorf("supernet %s is not blocked", cidr)
			continue
		}
		assertACL(t, "block "+cidr, a, aclAnyProtocol, aclPriorityBlock)
		if a.Direction != hcn.DirectionTypeOut {
			t.Errorf("block %s direction = %v, want Out", cidr, a.Direction)
		}
	}
	if len(blockedSupernets) != len(egressBlockedCIDRs) {
		t.Errorf("got %d block rules, want %d (one per supernet, no extras)", len(blockedSupernets), len(egressBlockedCIDRs))
	}
}

// TestL2BridgeEgressACLPolicies_NoGatewayOrSubnetCarveOut pins the load-bearing
// safety property that differs from NAT: on L2Bridge the container is a LAN
// peer, so there must be NO carve-out — no allow for any RFC1918 address, and no
// block that excludes a subnet. The router and the management plane stay
// unreachable, matching the Linux end-state.
func TestL2BridgeEgressACLPolicies_NoGatewayOrSubnetCarveOut(t *testing.T) {
	acls := decodeACLs(t, mustBuildL2Bridge(t, nil, ""))

	for _, a := range acls {
		// The only allows permitted with default (empty) extraAllowed are the
		// two allow-any (0.0.0.0/0) rules.
		if a.Action != hcn.ActionTypeAllow {
			continue
		}
		if a.RemoteAddresses != "0.0.0.0/0" {
			t.Errorf("unexpected allow carve-out to %q (only 0.0.0.0/0 is permitted with no extra-allowed configured)", a.RemoteAddresses)
		}
	}

	// Blocks must be the whole supernets, never a range/exclusion.
	for _, a := range acls {
		if a.Action == hcn.ActionTypeBlock && a.RemoteAddresses != "" {
			if _, _, err := net.ParseCIDR(a.RemoteAddresses); err != nil {
				t.Errorf("block RemoteAddresses %q is not a plain CIDR (a carve-out range leaked in): %v", a.RemoteAddresses, err)
			}
		}
	}
}

// TestL2BridgeEgressACLPolicies_Precedence pins the priority ladder: operator
// carve-outs must sit ABOVE the RFC1918 block (lower number = higher
// precedence), which must sit above the allow-any floor.
func TestL2BridgeEgressACLPolicies_Precedence(t *testing.T) {
	if !(aclPriorityExtraAllow < aclPriorityBlock &&
		aclPriorityBlock < aclPriorityAllowAny) {
		t.Fatalf("priority ladder broken: extra=%d block=%d allowany=%d (want extra < block < allowany)",
			aclPriorityExtraAllow, aclPriorityBlock, aclPriorityAllowAny)
	}
}

// TestL2BridgeEgressACLPolicies_EveryRuleIsAddressScoped is a regression guard
// for the metal finding that motivated removing the DHCP allows: a Switch ACL
// carrying only a port scope (no RemoteAddresses/LocalAddresses) blackholes the
// entire VFP port. HNS accepts such a rule, then nothing egresses at all — not
// the internet, not even an explicitly allowed destination. Keep every rule in
// this ladder address-scoped.
func TestL2BridgeEgressACLPolicies_EveryRuleIsAddressScoped(t *testing.T) {
	for _, extra := range [][]string{nil, {"203.0.113.0/24"}} {
		for _, a := range decodeACLs(t, mustBuildL2Bridge(t, extra, "198.51.100.7")) {
			if a.RemoteAddresses == "" && a.LocalAddresses == "" {
				t.Errorf("ACL with no address scope: %+v (blackholes the whole port on metal)", a)
			}
			if a.RemotePorts != "" || a.LocalPorts != "" {
				t.Errorf("port-scoped ACL in the L2Bridge ladder: %+v", a)
			}
		}
	}
}

// TestL2BridgeEgressACLPolicies_ExtraAllowed verifies configured carve-outs are
// emitted as Out allows ABOVE the block so they win over the RFC1918 deny.
func TestL2BridgeEgressACLPolicies_ExtraAllowed(t *testing.T) {
	extra := []string{"192.168.50.10/32", "172.20.0.0/16"}
	acls := decodeACLs(t, mustBuildL2Bridge(t, extra, ""))

	found := map[string]hcn.AclPolicySetting{}
	for _, a := range acls {
		if a.Action == hcn.ActionTypeAllow && a.RemoteAddresses != "" && a.RemoteAddresses != "0.0.0.0/0" {
			found[a.RemoteAddresses] = a
		}
	}
	for _, cidr := range extra {
		a, ok := found[cidr]
		if !ok {
			t.Errorf("extra-allowed %s not emitted", cidr)
			continue
		}
		if a.Direction != hcn.DirectionTypeOut || a.Priority != aclPriorityExtraAllow {
			t.Errorf("extra-allowed %s = %+v, want Out priority %d", cidr, a, aclPriorityExtraAllow)
		}
		if a.Priority >= aclPriorityBlock {
			t.Errorf("extra-allowed %s priority %d not above block %d (would not win)", cidr, a.Priority, aclPriorityBlock)
		}
	}
}

func mustBuildL2Bridge(t *testing.T, extra []string, hostIP string) []hcn.EndpointPolicy {
	t.Helper()
	policies, err := buildL2BridgeEgressACLPolicies(extra, hostIP)
	if err != nil {
		t.Fatalf("buildL2BridgeEgressACLPolicies: %v", err)
	}
	return policies
}

func assertACL(t *testing.T, name string, a hcn.AclPolicySetting, wantProto string, wantPrio uint16) {
	t.Helper()
	if a.Protocols != wantProto {
		t.Errorf("%s protocol = %q, want %q", name, a.Protocols, wantProto)
	}
	if a.Priority != wantPrio {
		t.Errorf("%s priority = %d, want %d", name, a.Priority, wantPrio)
	}
	if a.RuleType != hcn.RuleTypeSwitch {
		t.Errorf("%s ruletype = %q, want Switch", name, a.RuleType)
	}
}

// TestL2BridgeEgressACLPolicies_HostAllow verifies the one carve-out that keeps
// jobs provisionable: with a host address supplied, the ladder emits an
// address-scoped /32 Allow ABOVE the RFC1918 block. Without it the container
// cannot reach the per-job dind Docker API (DOCKER_HOST) or the module proxy,
// both of which listen on the host address — the host is inside 192.168/16 or
// 10/8 like every other LAN peer.
func TestL2BridgeEgressACLPolicies_HostAllow(t *testing.T) {
	const hostIP = "198.51.100.7"
	acls := decodeACLs(t, mustBuildL2Bridge(t, nil, hostIP))

	var found *hcn.AclPolicySetting
	for i, a := range acls {
		if a.Action == hcn.ActionTypeAllow && a.RemoteAddresses == hostIP+"/32" {
			found = &acls[i]
		}
	}
	if found == nil {
		t.Fatalf("no /32 allow for the host address %s (dind and the module proxy would be unreachable)", hostIP)
	}
	assertACL(t, "host-allow", *found, aclAnyProtocol, aclPriorityHostAllow)
	if found.Direction != hcn.DirectionTypeOut {
		t.Errorf("host allow direction = %v, want Out", found.Direction)
	}
	if found.Priority >= aclPriorityBlock {
		t.Errorf("host allow priority %d is not above the block priority %d (it would never win)", found.Priority, aclPriorityBlock)
	}
	// It must stay address-scoped. Narrowing it to the dind port would look
	// safer and would in fact blackhole the whole VFP port (see the metal note
	// on the removed DHCP rules).
	if found.RemotePorts != "" || found.LocalPorts != "" {
		t.Errorf("host allow is port-scoped (%+v); port-scoped Switch ACLs blackhole the VFP port", *found)
	}
}

// TestL2BridgeEgressACLPolicies_NoHostAllowByDefault pins the strict default:
// with no host address supplied (nothing ephemerd serves to containers), the
// ephemerd host is blocked along with the rest of RFC1918, exactly as verified
// on metal.
func TestL2BridgeEgressACLPolicies_NoHostAllowByDefault(t *testing.T) {
	for _, a := range decodeACLs(t, mustBuildL2Bridge(t, nil, "")) {
		if a.Action == hcn.ActionTypeAllow && a.RemoteAddresses != "0.0.0.0/0" {
			t.Errorf("unexpected allow %q with no host access configured (want only 0.0.0.0/0)", a.RemoteAddresses)
		}
	}
}

// TestL2BridgeControlPlaneRules verifies the host-firewall backstop that closes
// the control-plane ports back off after the host /32 allow opens the host.
// These are INBOUND rules scoped to the container pool as the remote — which
// only matches because L2Bridge does not NAT (on NAT the host sees its own
// address as the source; that is what sank #136).
func TestL2BridgeControlPlaneRules(t *testing.T) {
	const (
		hostIP = "198.51.100.7"
		pool   = "198.51.100.200-198.51.100.230"
	)
	rules := l2BridgeControlPlaneRules(hostIP, pool, []int{10000, 10001, 10002})
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3 (one per control port)", len(rules))
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.name, firewallRulePrefix) {
			t.Errorf("rule %q lacks the %q prefix, so Cleanup would not find it", r.name, firewallRulePrefix)
		}
		if specValue(r, "dir") != "in" {
			t.Errorf("rule %s dir = %q, want in (container->host is inbound at the host)", r.name, specValue(r, "dir"))
		}
		if specValue(r, "action") != "block" {
			t.Errorf("rule %s action = %q, want block", r.name, specValue(r, "action"))
		}
		if specValue(r, "localip") != hostIP {
			t.Errorf("rule %s localip = %q, want %s", r.name, specValue(r, "localip"), hostIP)
		}
		if specValue(r, "remoteip") != pool {
			t.Errorf("rule %s remoteip = %q, want the container pool %s", r.name, specValue(r, "remoteip"), pool)
		}
		if specValue(r, "localport") == "" {
			t.Errorf("rule %s has no localport; a portless block would cut the host off the container entirely", r.name)
		}
	}

	// Rule names must not collide with the NAT netsh rules, or removing one set
	// would delete the other.
	natRules, err := hostFirewallRules(DefaultSubnet, defaultGateway, []int{10000, 10001, 10002})
	if err != nil {
		t.Fatalf("hostFirewallRules: %v", err)
	}
	natNames := map[string]bool{}
	for _, r := range natRules {
		natNames[r.name] = true
	}
	for _, r := range rules {
		if natNames[r.name] {
			t.Errorf("L2Bridge rule name %q collides with a NAT rule name", r.name)
		}
	}
}

// TestL2BridgeControlPlaneRules_NoPlanNoRules verifies the backstop stays silent
// when there is nothing to scope it to, rather than emitting a rule with an
// empty address that would match everything.
func TestL2BridgeControlPlaneRules_NoPlanNoRules(t *testing.T) {
	if rules := l2BridgeControlPlaneRules("", "198.51.100.0/24", []int{10000}); rules != nil {
		t.Errorf("rules emitted with no host address: %+v", rules)
	}
	if rules := l2BridgeControlPlaneRules("198.51.100.7", "", []int{10000}); rules != nil {
		t.Errorf("rules emitted with no pool: %+v", rules)
	}
	if rules := l2BridgeControlPlaneRules("198.51.100.7", "198.51.100.0/24", nil); len(rules) != 0 {
		t.Errorf("rules emitted with no control ports: %+v", rules)
	}
}

// TestWindowsHostAddr_L2BridgeReportsHostIP is the regression guard for the
// binding that took down provisioning on the ICS experiment: on L2Bridge there
// is no 10.88.0.1 on any interface, so anything that binds GatewayIP() — the
// per-job dind listener and the Go module proxy — must be handed the host's own
// LAN address instead.
func TestWindowsHostAddr_L2BridgeReportsHostIP(t *testing.T) {
	w := &windowsNetworking{
		cfg:  Config{L2BridgeEgress: true},
		plan: &l2BridgePlan{HostIP: "198.51.100.7"},
	}
	if got := w.hostAddr(); got != "198.51.100.7" {
		t.Errorf("hostAddr() = %q, want the host's L2Bridge address", got)
	}
	m := &Manager{cfg: Config{}, platform: w}
	if got := m.GatewayIP(); got != "198.51.100.7" {
		t.Errorf("GatewayIP() = %q, want the host's L2Bridge address (10.88.0.1 binds to nothing on L2Bridge)", got)
	}

	// NAT path unchanged: no plan, no override, the old gateway still applies.
	nat := &windowsNetworking{cfg: Config{}}
	if got := nat.hostAddr(); got != "" {
		t.Errorf("NAT hostAddr() = %q, want empty (generic derivation)", got)
	}
	if got := (&Manager{cfg: Config{}, platform: nat}).GatewayIP(); got != defaultGateway {
		t.Errorf("NAT GatewayIP() = %q, want %q", got, defaultGateway)
	}
}

// TestWindowsHostAllowIP_GatedOnAllowHostAccess pins that the host carve-out is
// opt-in: it appears only when ephemerd actually serves something to containers.
func TestWindowsHostAllowIP_GatedOnAllowHostAccess(t *testing.T) {
	plan := &l2BridgePlan{HostIP: "198.51.100.7"}
	off := &windowsNetworking{cfg: Config{L2BridgeEgress: true}, plan: plan}
	if got := off.hostAllowIP(); got != "" {
		t.Errorf("hostAllowIP() = %q with AllowHostAccess=false, want empty (strict posture)", got)
	}
	on := &windowsNetworking{cfg: Config{L2BridgeEgress: true, AllowHostAccess: true}, plan: plan}
	if got := on.hostAllowIP(); got != "198.51.100.7" {
		t.Errorf("hostAllowIP() = %q with AllowHostAccess=true, want the host address", got)
	}
	// No plan (init failed or NAT) must never produce a carve-out.
	none := &windowsNetworking{cfg: Config{L2BridgeEgress: true, AllowHostAccess: true}}
	if got := none.hostAllowIP(); got != "" {
		t.Errorf("hostAllowIP() = %q with no resolved plan, want empty", got)
	}
}
