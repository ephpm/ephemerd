//go:build windows

package networking

import (
	"encoding/json"
	"net"
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
// ladder: the two mandatory allow-any rules (Out+In), the DHCP allows, and a
// whole-supernet block for every RFC1918 + link-local range — with the exact
// actions, directions, protocols, and priorities that were proven on metal.
func TestL2BridgeEgressACLPolicies_LadderShape(t *testing.T) {
	acls := decodeACLs(t, mustBuildL2Bridge(t, nil))

	var (
		allowAnyOut, allowAnyIn bool
		dhcpOut, dhcpIn         bool
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
		case a.Action == hcn.ActionTypeAllow && a.Protocols == aclUDPProtocol && a.Direction == hcn.DirectionTypeOut:
			dhcpOut = true
			if a.RemotePorts != "67,68" || a.Priority != aclPriorityDHCP {
				t.Errorf("dhcp-out = %+v, want RemotePorts 67,68 priority %d", a, aclPriorityDHCP)
			}
		case a.Action == hcn.ActionTypeAllow && a.Protocols == aclUDPProtocol && a.Direction == hcn.DirectionTypeIn:
			dhcpIn = true
			if a.LocalPorts != "67,68" || a.Priority != aclPriorityDHCP {
				t.Errorf("dhcp-in = %+v, want LocalPorts 67,68 priority %d", a, aclPriorityDHCP)
			}
		case a.Action == hcn.ActionTypeBlock:
			blockedSupernets[a.RemoteAddresses] = a
		}
	}

	if !allowAnyOut || !allowAnyIn {
		t.Errorf("missing mandatory allow-any rule: out=%v in=%v (both required or the port default-denies / drops SYN-ACK)", allowAnyOut, allowAnyIn)
	}
	if !dhcpOut || !dhcpIn {
		t.Errorf("missing DHCP allow: out=%v in=%v", dhcpOut, dhcpIn)
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
	acls := decodeACLs(t, mustBuildL2Bridge(t, nil))

	for _, a := range acls {
		// The only allows permitted with default (empty) extraAllowed are the
		// two allow-any (0.0.0.0/0) rules and the two DHCP rules (no address).
		if a.Action != hcn.ActionTypeAllow {
			continue
		}
		if a.Protocols == aclUDPProtocol {
			continue // DHCP allow — no address scope
		}
		if a.RemoteAddresses != "0.0.0.0/0" {
			t.Errorf("unexpected allow carve-out to %q (only 0.0.0.0/0 and DHCP allows are permitted with no extra-allowed configured)", a.RemoteAddresses)
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

// TestL2BridgeEgressACLPolicies_Precedence pins the priority ladder: DHCP and
// operator carve-outs must sit ABOVE the RFC1918 block (lower number = higher
// precedence), which must sit above the allow-any floor.
func TestL2BridgeEgressACLPolicies_Precedence(t *testing.T) {
	if !(aclPriorityDHCP < aclPriorityBlock &&
		aclPriorityExtraAllow < aclPriorityBlock &&
		aclPriorityBlock < aclPriorityAllowAny) {
		t.Fatalf("priority ladder broken: dhcp=%d extra=%d block=%d allowany=%d (want dhcp,extra < block < allowany)",
			aclPriorityDHCP, aclPriorityExtraAllow, aclPriorityBlock, aclPriorityAllowAny)
	}
}

// TestL2BridgeEgressACLPolicies_ExtraAllowed verifies configured carve-outs are
// emitted as Out allows ABOVE the block so they win over the RFC1918 deny.
func TestL2BridgeEgressACLPolicies_ExtraAllowed(t *testing.T) {
	extra := []string{"192.168.50.10/32", "172.20.0.0/16"}
	acls := decodeACLs(t, mustBuildL2Bridge(t, extra))

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

func mustBuildL2Bridge(t *testing.T, extra []string) []hcn.EndpointPolicy {
	t.Helper()
	policies, err := buildL2BridgeEgressACLPolicies(extra)
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
