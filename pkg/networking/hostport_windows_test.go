//go:build windows

package networking

import (
	"strings"
	"testing"
)

// TestHostPortAllowRule_ScopedToPoolAndPort proves the dind-over-L2Bridge fix
// opens exactly one host port to exactly the container pool. The VFP host /32
// allow lets a container's packet leave toward the host, but the host's own
// inbound Windows Firewall default-denies it — so the per-job dind listener is
// unreachable without this rule. It must stay tightly scoped: a broad inbound
// allow would expose RDP/SMB/RPC on the host to job containers.
func TestHostPortAllowRule_ScopedToPoolAndPort(t *testing.T) {
	const (
		hostIP = "192.0.2.10"
		pool   = "192.0.2.192/27"
		port   = 63933
	)
	r := hostPortAllowRule(hostIP, pool, port)
	spec := strings.Join(r.spec, " ")

	for _, want := range []string{
		"dir=in", "action=allow", "protocol=TCP",
		"localip=" + hostIP, "localport=63933", "remoteip=" + pool,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("host-port allow rule missing %q; spec = %q", want, spec)
		}
	}
	// Must be port-scoped, not a blanket host allow.
	if strings.Contains(spec, "localport=any") || !strings.Contains(spec, "localport=63933") {
		t.Errorf("rule is not scoped to the single dind port: %q", spec)
	}
	// The rule name must carry the port so concurrent jobs get distinct rules.
	if !strings.Contains(r.name, "63933") {
		t.Errorf("rule name %q must be port-specific so concurrent jobs do not collide", r.name)
	}
	// The name must fall under the prefix the shutdown sweep matches, or a
	// hard-killed job leaks its allow forever.
	if !strings.HasPrefix(r.name, hostPortRulePrefix) {
		t.Errorf("rule name %q must start with the sweep prefix %q", r.name, hostPortRulePrefix)
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
