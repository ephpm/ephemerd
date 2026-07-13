//go:build windows

package networking

import (
	"encoding/json"
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
