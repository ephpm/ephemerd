//go:build windows

package networking

import (
	"slices"
	"strings"
	"testing"
)

// A concrete container VMCreatorId for the tests. The real value is discovered
// at runtime from Get-NetFirewallHyperVVMCreator (excluding WSL); the rule
// construction is identical whatever GUID it resolves to.
const testContainerVMCreatorID = "{9E9E4CB2-1B2C-4D3E-8F90-ABCDEF012345}"

// TestHyperVEgressRules_BlocksEveryDeniedRange verifies each RFC1918 +
// link-local range produces exactly one outbound Block rule scoped to the
// container VMCreatorId; a missing range would let a job reach that slice of
// the LAN.
func TestHyperVEgressRules_BlocksEveryDeniedRange(t *testing.T) {
	rules, err := hyperVEgressRules(testContainerVMCreatorID, DefaultSubnet, defaultGateway, nil)
	if err != nil {
		t.Fatalf("hyperVEgressRules: %v", err)
	}
	if len(rules) != len(egressBlockedCIDRs) {
		t.Fatalf("got %d rules, want %d (one outbound block per denied range)", len(rules), len(egressBlockedCIDRs))
	}
	for i, cidr := range egressBlockedCIDRs {
		r := rules[i]
		if r.direction != "Outbound" || r.action != "Block" {
			t.Errorf("rule %s is not an outbound block: dir=%s action=%s", r.name, r.direction, r.action)
		}
		if r.vmCreatorID != testContainerVMCreatorID {
			t.Errorf("rule %s not scoped to the container creator: %q", r.name, r.vmCreatorID)
		}
		if len(r.remoteAddrs) == 0 {
			t.Errorf("rule %s has no RemoteAddresses scope", r.name)
		}
		wantName := firewallRulePrefix + "-" + creatorTag(testContainerVMCreatorID) + "-block-" + strings.ReplaceAll(cidr, "/", "_")
		if r.name != wantName {
			t.Errorf("rule[%d].name = %q, want %q", i, r.name, wantName)
		}
	}
}

// TestHyperVEgressRules_GatewayAndSubnetNeverBlocked pins the safety property:
// the container subnet — which contains the NAT gateway (DNS, default route,
// GatewayPorts) and the other containers — must never appear inside a block
// rule's RemoteAddresses. Blocking the gateway would brick all container
// networking. The 10/8 block must be split exactly around the subnet.
func TestHyperVEgressRules_GatewayAndSubnetNeverBlocked(t *testing.T) {
	rules, err := hyperVEgressRules(testContainerVMCreatorID, "10.88.0.0/16", "10.88.0.1", nil)
	if err != nil {
		t.Fatalf("hyperVEgressRules: %v", err)
	}

	for _, r := range rules {
		for _, addr := range r.remoteAddrs {
			if strings.Contains(addr, "10.88.") {
				t.Errorf("rule %s blocks the container subnet: RemoteAddresses contains %q", r.name, addr)
			}
		}
	}

	tag := creatorTag(testContainerVMCreatorID)
	wantName := firewallRulePrefix + "-" + tag + "-block-10.0.0.0_8"
	want := []string{"10.0.0.0-10.87.255.255", "10.89.0.0-10.255.255.255"}
	for _, r := range rules {
		if r.name == wantName {
			if !slices.Equal(r.remoteAddrs, want) {
				t.Errorf("10/8 block RemoteAddresses = %v, want %v", r.remoteAddrs, want)
			}
			return
		}
	}
	t.Errorf("no block rule found for 10.0.0.0/8 (name %q)", wantName)
}

// TestHyperVEgressRules_ControlPortRules confirms the container->gateway
// control-plane blocks mirror the Linux drops: outbound, TCP, one specific port
// each, RemoteAddresses = gateway only — never a blanket gateway block and
// never port 53 (DNS must survive).
func TestHyperVEgressRules_ControlPortRules(t *testing.T) {
	ports := []int{10000, 10001, 10002} // containerd, dispatch, debug exec
	rules, err := hyperVEgressRules(testContainerVMCreatorID, DefaultSubnet, defaultGateway, ports)
	if err != nil {
		t.Fatalf("hyperVEgressRules: %v", err)
	}

	var control []hyperVRule
	for _, r := range rules {
		if len(r.remotePorts) > 0 {
			control = append(control, r)
		}
	}
	if len(control) != len(ports) {
		t.Fatalf("got %d control rules, want %d (one per control port)", len(control), len(ports))
	}

	for i, port := range []string{"10000", "10001", "10002"} {
		r := control[i]
		if r.direction != "Outbound" || r.action != "Block" || r.protocol != "TCP" {
			t.Errorf("rule %s is not an outbound TCP block: dir=%s action=%s proto=%s", r.name, r.direction, r.action, r.protocol)
		}
		if !slices.Equal(r.remotePorts, []string{port}) {
			t.Errorf("rule %s RemotePorts = %v, want [%s]", r.name, r.remotePorts, port)
		}
		if !slices.Equal(r.remoteAddrs, []string{defaultGateway}) {
			t.Errorf("rule %s RemoteAddresses = %v, want [%s]", r.name, r.remoteAddrs, defaultGateway)
		}
		if port == "53" {
			t.Errorf("rule %s blocks DNS (port 53) — must not", r.name)
		}
	}
}

// TestHyperVRuleCommand_Rendering pins the exact PowerShell the install path
// runs: New-NetFirewallHyperVRule with the VMCreatorId scoping, Outbound/Block,
// and RemoteAddresses/RemotePorts rendered as quoted PowerShell arrays (so a
// string[] parameter receives distinct elements, not one comma-joined string).
func TestHyperVRuleCommand_Rendering(t *testing.T) {
	block := hyperVRule{
		name:        "ephemerd-egress-9e9e4cb2-block-10.0.0.0_8",
		displayName: "ephemerd egress block 10.0.0.0/8",
		direction:   "Outbound",
		action:      "Block",
		vmCreatorID: testContainerVMCreatorID,
		remoteAddrs: []string{"10.0.0.0-10.87.255.255", "10.89.0.0-10.255.255.255"},
	}
	got := block.command()
	for _, want := range []string{
		"New-NetFirewallHyperVRule",
		"-Name 'ephemerd-egress-9e9e4cb2-block-10.0.0.0_8'",
		"-DisplayName 'ephemerd egress block 10.0.0.0/8'",
		"-Direction Outbound",
		"-Action Block",
		"-VMCreatorId '{9E9E4CB2-1B2C-4D3E-8F90-ABCDEF012345}'",
		"-RemoteAddresses '10.0.0.0-10.87.255.255','10.89.0.0-10.255.255.255'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("command() = %q\n  missing %q", got, want)
		}
	}
	// An all-protocol block must not emit -Protocol (default Any) and must not
	// emit -RemotePorts.
	if strings.Contains(got, "-Protocol") {
		t.Errorf("all-protocol block should omit -Protocol: %q", got)
	}
	if strings.Contains(got, "-RemotePorts") {
		t.Errorf("block without ports should omit -RemotePorts: %q", got)
	}

	control := hyperVRule{
		name:        "ephemerd-egress-9e9e4cb2-control-10000",
		displayName: "ephemerd egress block control tcp/10000",
		direction:   "Outbound",
		action:      "Block",
		vmCreatorID: testContainerVMCreatorID,
		protocol:    "TCP",
		remoteAddrs: []string{"10.88.0.1"},
		remotePorts: []string{"10000"},
	}
	gotC := control.command()
	for _, want := range []string{
		"-Protocol TCP",
		"-RemoteAddresses '10.88.0.1'",
		"-RemotePorts '10000'",
	} {
		if !strings.Contains(gotC, want) {
			t.Errorf("control command() = %q\n  missing %q", gotC, want)
		}
	}
}

// TestHyperVRuleRemoveCommand verifies removal targets exactly the name add
// created (that is what makes remove-before-add idempotent) and stays quiet on
// a fresh host.
func TestHyperVRuleRemoveCommand(t *testing.T) {
	r := hyperVRule{name: "ephemerd-egress-9e9e4cb2-block-192.168.0.0_16"}
	got := r.removeCommand()
	want := "Remove-NetFirewallHyperVRule -Name 'ephemerd-egress-9e9e4cb2-block-192.168.0.0_16' -ErrorAction SilentlyContinue"
	if got != want {
		t.Errorf("removeCommand() = %q, want %q", got, want)
	}
}

// TestHyperVRuleNames pins the naming contract: every rule carries the ephemerd
// prefix (so the set is findable and removable by removeByPrefixScript) and is
// scoped to the creator via a short tag so multiple creators do not collide.
func TestHyperVRuleNames(t *testing.T) {
	rules, err := hyperVEgressRules(testContainerVMCreatorID, DefaultSubnet, defaultGateway, []int{10000})
	if err != nil {
		t.Fatalf("hyperVEgressRules: %v", err)
	}
	tag := creatorTag(testContainerVMCreatorID)
	if tag != "9e9e4cb2" {
		t.Errorf("creatorTag = %q, want %q", tag, "9e9e4cb2")
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.name, firewallRulePrefix+"-") {
			t.Errorf("rule name %q missing %q prefix", r.name, firewallRulePrefix)
		}
		if !strings.Contains(r.name, tag) {
			t.Errorf("rule name %q missing creator tag %q", r.name, tag)
		}
		if r.displayName == "" {
			t.Errorf("rule %q has empty DisplayName (mandatory for New-NetFirewallHyperVRule)", r.name)
		}
	}

	// removeByPrefixScript must match those names.
	if !strings.Contains(removeByPrefixScript(), firewallRulePrefix+"-*") {
		t.Errorf("removeByPrefixScript does not match the rule-name prefix: %q", removeByPrefixScript())
	}
}

// TestCreatorTag covers normalization: hex-only, lowercased, first 8, with a
// safe fallback for a GUID that yields no hex.
func TestCreatorTag(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"{9E9E4CB2-1B2C-4D3E-8F90-ABCDEF012345}", "9e9e4cb2"},
		{"{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}", "40e0ac32"},
		{"{GGGG}", "any"},
		{"", "any"},
	}
	for _, tt := range tests {
		if got := creatorTag(tt.in); got != tt.want {
			t.Errorf("creatorTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestPSArrayAndQuote covers the PowerShell rendering helpers, including the
// embedded-quote escape that keeps a crafted value from breaking out of the
// argument.
func TestPSArrayAndQuote(t *testing.T) {
	if got := psQuote("a'b"); got != "'a''b'" {
		t.Errorf("psQuote = %q, want %q", got, "'a''b'")
	}
	if got := psArray([]string{"x", "y"}); got != "'x','y'" {
		t.Errorf("psArray = %q, want %q", got, "'x','y'")
	}
}
