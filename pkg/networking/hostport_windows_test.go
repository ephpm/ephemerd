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
