//go:build windows

package networking

import (
	"slices"
	"strings"
	"testing"
)

// specValue extracts the value of a key=value pair from a rule spec, or ""
// when the key is absent.
func specValue(r winFirewallRule, key string) string {
	for _, s := range r.spec {
		if v, ok := strings.CutPrefix(s, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestSubtractCIDR(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		exclude string
		want    []string
		wantErr bool
	}{
		{
			name:    "subnet inside range splits it",
			cidr:    "10.0.0.0/8",
			exclude: "10.88.0.0/16",
			want:    []string{"10.0.0.0-10.87.255.255", "10.89.0.0-10.255.255.255"},
		},
		{
			name:    "no overlap keeps the CIDR",
			cidr:    "172.16.0.0/12",
			exclude: "10.88.0.0/16",
			want:    []string{"172.16.0.0/12"},
		},
		{
			name:    "exclude equals the range",
			cidr:    "10.88.0.0/16",
			exclude: "10.88.0.0/16",
			want:    nil,
		},
		{
			name:    "exclude covers the range",
			cidr:    "10.88.0.0/16",
			exclude: "10.0.0.0/8",
			want:    nil,
		},
		{
			name:    "exclude at the start leaves one range",
			cidr:    "10.0.0.0/8",
			exclude: "10.0.0.0/16",
			want:    []string{"10.1.0.0-10.255.255.255"},
		},
		{
			name:    "exclude at the end leaves one range",
			cidr:    "10.0.0.0/8",
			exclude: "10.255.0.0/16",
			want:    []string{"10.0.0.0-10.254.255.255"},
		},
		{
			name:    "malformed CIDR errors",
			cidr:    "not-a-cidr",
			exclude: "10.88.0.0/16",
			wantErr: true,
		},
		{
			name:    "IPv6 CIDR errors",
			cidr:    "fc00::/7",
			exclude: "10.88.0.0/16",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := subtractCIDR(tt.cidr, tt.exclude)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("subtractCIDR(%q, %q) = %v, want error", tt.cidr, tt.exclude, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("subtractCIDR(%q, %q): %v", tt.cidr, tt.exclude, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("subtractCIDR(%q, %q) = %v, want %v", tt.cidr, tt.exclude, got, tt.want)
			}
		})
	}
}

// TestHostFirewallRules_BlocksEveryDeniedRange verifies each RFC1918 +
// link-local range produces an outbound block rule; a missing range would let
// a job reach that slice of the LAN.
func TestHostFirewallRules_BlocksEveryDeniedRange(t *testing.T) {
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, nil)
	if err != nil {
		t.Fatalf("hostFirewallRules: %v", err)
	}
	if len(rules) != len(egressBlockedCIDRs) {
		t.Fatalf("got %d rules, want %d (one outbound block per denied range)", len(rules), len(egressBlockedCIDRs))
	}
	for i, cidr := range egressBlockedCIDRs {
		r := rules[i]
		wantName := firewallRulePrefix + "-block-" + strings.ReplaceAll(cidr, "/", "_")
		if r.name != wantName {
			t.Errorf("rule[%d].name = %q, want %q", i, r.name, wantName)
		}
		if specValue(r, "dir") != "out" || specValue(r, "action") != "block" {
			t.Errorf("rule %s is not an outbound block: %v", r.name, r.spec)
		}
		if specValue(r, "remoteip") == "" {
			t.Errorf("rule %s has no remoteip scope", r.name)
		}
	}
}

// TestHostFirewallRules_GatewayAndSubnetNeverBlocked pins the safety property:
// the container subnet — which contains the NAT gateway (DNS, default route,
// GatewayPorts) and the other containers — must never appear inside a blocked
// range. Windows Firewall has no rule ordering and Block beats Allow, so a
// blocked gateway could not be rescued by an allow rule: it would brick all
// container networking.
func TestHostFirewallRules_GatewayAndSubnetNeverBlocked(t *testing.T) {
	rules, err := hostFirewallRules("10.88.0.0/16", "10.88.0.1", nil)
	if err != nil {
		t.Fatalf("hostFirewallRules: %v", err)
	}

	for _, r := range rules {
		remote := specValue(r, "remoteip")
		if strings.Contains(remote, "10.88.") {
			t.Errorf("rule %s blocks the container subnet: remoteip=%s", r.name, remote)
		}
		// Outbound blocks must be scoped to container-sourced traffic so the
		// host's own LAN access can never match.
		if specValue(r, "localip") != "10.88.0.0/16" {
			t.Errorf("rule %s not scoped to the container subnet: localip=%s", r.name, specValue(r, "localip"))
		}
	}

	// The 10.0.0.0/8 block must be split exactly around the subnet.
	want := "10.0.0.0-10.87.255.255,10.89.0.0-10.255.255.255"
	for _, r := range rules {
		if r.name == firewallRulePrefix+"-block-10.0.0.0_8" {
			if got := specValue(r, "remoteip"); got != want {
				t.Errorf("10/8 block remoteip = %q, want %q", got, want)
			}
			return
		}
	}
	t.Error("no block rule found for 10.0.0.0/8")
}

// TestHostFirewallRules_ControlPortRules confirms the container→gateway
// control-plane blocks mirror the Linux INPUT drops: inbound, TCP, one
// specific port each, source = container subnet, destination = gateway —
// never a blanket gateway block and never port 53.
func TestHostFirewallRules_ControlPortRules(t *testing.T) {
	ports := []int{10000, 10001, 10002} // containerd, dispatch, debug exec
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, ports)
	if err != nil {
		t.Fatalf("hostFirewallRules: %v", err)
	}

	var control []winFirewallRule
	for _, r := range rules {
		if specValue(r, "dir") == "in" {
			control = append(control, r)
		}
	}
	if len(control) != len(ports) {
		t.Fatalf("got %d inbound rules, want %d (one per control port)", len(control), len(ports))
	}

	for i, port := range []string{"10000", "10001", "10002"} {
		r := control[i]
		if specValue(r, "action") != "block" || specValue(r, "protocol") != "TCP" {
			t.Errorf("rule %s is not a TCP block: %v", r.name, r.spec)
		}
		if specValue(r, "localport") != port {
			t.Errorf("rule %s localport = %s, want %s", r.name, specValue(r, "localport"), port)
		}
		if specValue(r, "remoteip") != DefaultSubnet {
			t.Errorf("rule %s missing source-subnet scope: %v", r.name, r.spec)
		}
		if specValue(r, "localip") != defaultGateway {
			t.Errorf("rule %s missing gateway-dest scope: %v", r.name, r.spec)
		}
		if specValue(r, "localport") == "53" {
			t.Errorf("rule %s blocks DNS (port 53) — must not", r.name)
		}
	}
}

// TestHostFirewallRules_NamesAndArgs pins the naming and argv contract: every
// rule carries the ephemerd prefix (so the set is findable and removable),
// and delete targets exactly the name add created (that is what makes
// delete-before-add idempotent).
func TestHostFirewallRules_NamesAndArgs(t *testing.T) {
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, []int{10000})
	if err != nil {
		t.Fatalf("hostFirewallRules: %v", err)
	}
	for _, r := range rules {
		if !strings.HasPrefix(r.name, firewallRulePrefix) {
			t.Errorf("rule name %q missing %q prefix", r.name, firewallRulePrefix)
		}
		add := r.addArgs()
		wantAdd := []string{"advfirewall", "firewall", "add", "rule", "name=" + r.name}
		if !slices.Equal(add[:5], wantAdd) {
			t.Errorf("addArgs()[:5] = %v, want %v", add[:5], wantAdd)
		}
		if !slices.Equal(add[5:], r.spec) {
			t.Errorf("addArgs() spec = %v, want %v", add[5:], r.spec)
		}
		wantDel := []string{"advfirewall", "firewall", "delete", "rule", "name=" + r.name}
		if !slices.Equal(r.deleteArgs(), wantDel) {
			t.Errorf("deleteArgs() = %v, want %v", r.deleteArgs(), wantDel)
		}
	}
}
