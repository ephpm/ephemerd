//go:build windows

package networking

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Egress firewall for Windows job containers.
//
// The ONLY working host-side egress enforcement on Windows is the L2Bridge VFP
// Switch-ACL path (opt-in via network.l2bridge_egress): per-endpoint ACLs on
// the container's vSwitch port, built by buildL2BridgeEgressACLPolicies in
// network_windows.go and applied in setup(), plus the small host-firewall
// backstop below that fences the control-plane ports back off. This was proven
// on metal.
//
// The default NAT network CANNOT be egress-filtered by any host-side mechanism,
// and ephemerd does not try. Every candidate was disproven on real hardware:
//
//   - Host Windows Defender Firewall (netsh/MPSSVC): evaluates the forwarded
//     container->LAN traffic POST-NAT at the host endpoint, where the source is
//     the host's own LAN address, so a rule scoped to the container subnet
//     matches nothing (verified on a live Windows node — every management-plane
//     probe still succeeded).
//   - The Hyper-V firewall (New-NetFirewallHyperVRule, -VMCreatorId scoped):
//     runhcs NAT containers register NO Hyper-V VM creator, so there is no
//     creator to scope a rule to; the rules install but filter nothing.
//   - A host-side WFP filter at IPFORWARD_V4 (the Linux FORWARD analogue): the
//     container's forwarded + SNAT'd egress is never classified at ANY host
//     IPv4 WFP layer — the packets travel the Hyper-V vSwitch/HNS datapath,
//     out-of-band of the host tcpip.sys WFP hooks. Full evidence:
//     docs/arch/windows-egress-wfp-investigation.md.
//
// So on the NAT path ephemerd installs nothing and logs that container egress
// is unfiltered, pointing the operator at network.l2bridge_egress. Enforcing
// NAT egress requires a network-level control (an isolated VLAN whose uplink
// denies RFC1918), not a host-side software filter.
//
// IPv4 only, deliberately: the HCN NAT network is IPv4-only (no v6 IPAM), so
// containers have no IPv6 path.

// firewallRulePrefix names every rule ephemerd installs (the L2Bridge
// host-firewall backstop) so the set is findable and removable on Cleanup.
const firewallRulePrefix = "ephemerd-egress"

// psQuote single-quotes a value for PowerShell, doubling embedded quotes.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// powershellArgs wraps a script for non-interactive execution.
func powershellArgs(script string) []string {
	return []string{"-NonInteractive", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script}
}

// powershell runs a PowerShell script, returning combined output on error.
func powershell(script string) error {
	out, err := exec.Command("powershell", powershellArgs(script)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("powershell: %w: %s", err, out)
	}
	return nil
}

// powershellOutput runs a PowerShell script and returns its stdout.
func powershellOutput(script string) (string, error) {
	out, err := exec.Command("powershell", powershellArgs(script)...).Output()
	if err != nil {
		return "", fmt.Errorf("powershell: %w", err)
	}
	return string(out), nil
}

// -------------------------------------------------------------------------
// L2Bridge host-firewall backstop
// -------------------------------------------------------------------------
//
// On the L2Bridge path the primary egress enforcement is the per-endpoint VFP
// ACL ladder (buildL2BridgeEgressACLPolicies), which was proven on metal.
//
// What the host firewall CAN do here, and could not on NAT, is match on the
// container's source address. On NAT the host sees container egress post-NAT
// with its own address as the source, so a host rule scoped to the container
// source matches nothing. L2Bridge does not NAT: a container's packet reaches
// the host carrying the container's own pool address, as ordinary inbound LAN
// traffic. So an INBOUND rule scoped remoteip=<ip_pool> matches exactly the job
// containers and nothing else.
//
// That is used for one job: closing the control-plane ports back off after the
// VFP ladder's host /32 allow opens the host (AllowHostAccess). The allow has to
// be address-scoped — a port-scoped Switch ACL blackholes the entire VFP port —
// so the port precision lives here instead.

// l2BridgeControlPlaneRules returns the inbound host-firewall blocks for
// container -> ephemerd control-plane traffic on the L2Bridge path: one rule per
// control port, scoped to the host address on the bridged LAN and to the source
// range containers are allocated from.
//
// Pure (no side effects) so the exact rule set is unit-testable without netsh.
func l2BridgeControlPlaneRules(hostIP, ipPool string, controlPorts []int) []winFirewallRule {
	if hostIP == "" || ipPool == "" {
		return nil
	}
	rules := make([]winFirewallRule, 0, len(controlPorts))
	for _, port := range controlPorts {
		rules = append(rules, winFirewallRule{
			name: fmt.Sprintf("%s-l2b-control-%d", firewallRulePrefix, port),
			spec: []string{
				"dir=in",
				"action=block",
				"protocol=TCP",
				"localip=" + hostIP,
				"localport=" + strconv.Itoa(port),
				"remoteip=" + ipPool,
				"profile=any",
				"enable=yes",
			},
		})
	}
	return rules
}

// windowsHostHardenPorts are host TCP ports that MUST NOT be reachable from a
// job container even though the VFP host /32 allow (AllowHostAccess) opens the
// host at the vSwitch layer for dind/module-proxy access.
//
// VFP Switch ACLs cannot be port-scoped — a port-scoped Switch rule blackholes
// the whole endpoint — so the host /32 allow is all-ports at the VFP layer, and
// port precision falls to the host's OWN Windows Firewall. On a stock Windows
// host that firewall ambiently permits WinRM and the RPC endpoint mapper
// inbound from the LAN, so a job container (a LAN peer on L2Bridge) can reach
// them. A job that reached WinRM could attempt host RCE. Block them explicitly
// from the pool — a host-firewall Block beats the ambient Allow — leaving only
// the scoped dind/module-proxy allows (high ephemeral ports) open. Found by the
// 2026-08-15 pen test: WinRM 5985 and RPC 135 were reachable from a job.
//
// Note: the RPC dynamic port range (49152-65535) overlaps the ephemeral port
// dind binds, so it cannot be blanket-blocked here; blocking the endpoint
// mapper (135) removes the discovery path, which is the material mitigation.
var windowsHostHardenPorts = []int{
	135,   // RPC endpoint mapper (the RPC discovery vector)
	139,   // NetBIOS session
	445,   // SMB
	3389,  // RDP
	5985,  // WinRM HTTP
	5986,  // WinRM HTTPS
	47001, // WSMan / WinRM listener
}

// l2BridgeHostHardenRules returns the inbound host-firewall blocks that fence
// the dangerous Windows management ports off from the container pool. Pure so
// the set is unit-testable without netsh.
func l2BridgeHostHardenRules(hostIP, ipPool string) []winFirewallRule {
	if hostIP == "" || ipPool == "" {
		return nil
	}
	rules := make([]winFirewallRule, 0, len(windowsHostHardenPorts))
	for _, port := range windowsHostHardenPorts {
		rules = append(rules, winFirewallRule{
			name: fmt.Sprintf("%s-l2b-harden-%d", firewallRulePrefix, port),
			spec: []string{
				"dir=in",
				"action=block",
				"protocol=TCP",
				"localip=" + hostIP,
				"localport=" + strconv.Itoa(port),
				"remoteip=" + ipPool,
				"profile=any",
				"enable=yes",
			},
		})
	}
	return rules
}

// installL2BridgeFirewallRules programs the L2Bridge host-firewall backstop:
// blocks for the ephemerd control-plane ports (if any) and, always, blocks for
// the dangerous Windows management ports (windowsHostHardenPorts) so the VFP
// host /32 allow cannot expose WinRM/RPC/RDP/SMB to job containers. Best-effort
// like the rest of installFirewallRules: the VFP ladder is the enforcement
// point and a host that cannot program netsh must not fail daemon startup.
func (w *windowsNetworking) installL2BridgeFirewallRules() error {
	if w.plan == nil {
		return nil
	}
	rules := l2BridgeControlPlaneRules(w.plan.HostIP, w.plan.PoolSpec, w.cfg.ControlPorts)
	rules = append(rules, l2BridgeHostHardenRules(w.plan.HostIP, w.plan.PoolSpec)...)
	if len(rules) == 0 {
		return nil
	}
	installed := 0
	for _, r := range rules {
		_ = netsh(r.deleteArgs()...) // idempotent: clear any rule of this name first
		if err := netsh(r.addArgs()...); err != nil {
			w.cfg.Log.Warn("failed to add L2Bridge host-firewall rule", "rule", r.name, "error", err)
			continue
		}
		installed++
	}
	w.cfg.Log.Info("L2Bridge host-firewall backstop installed",
		"rules", installed, "hardened_ports", windowsHostHardenPorts)
	return nil
}

// hostPortAllowRule is the scoped inbound allow that makes one host TCP port
// reachable from the container pool on the L2Bridge path (see openHostPort).
func hostPortAllowRule(hostIP, ipPool string, port int) winFirewallRule {
	return winFirewallRule{
		name: fmt.Sprintf("%s-l2b-hostport-%d", firewallRulePrefix, port),
		spec: []string{
			"dir=in",
			"action=allow",
			"protocol=TCP",
			"localip=" + hostIP,
			"localport=" + strconv.Itoa(port),
			"remoteip=" + ipPool,
			"profile=any",
			"enable=yes",
		},
	}
}

// openHostPort adds a scoped inbound allow so job containers can reach one host
// TCP port (a per-job dind Docker API listener, or the module proxy). Without
// it the host's default inbound deny drops the connection even though the VFP
// host /32 allow permits the container to send. Scoped to remoteip=<pool> and
// localport=<port>, so nothing else on the host opens. No-op unless the
// L2Bridge egress path is active with a resolved plan.
func (w *windowsNetworking) openHostPort(port int) error {
	if !w.cfg.L2BridgeEgress || w.plan == nil || w.plan.HostIP == "" || w.plan.PoolSpec == "" {
		return nil
	}
	r := hostPortAllowRule(w.plan.HostIP, w.plan.PoolSpec, port)
	_ = netsh(r.deleteArgs()...) // idempotent
	if err := netsh(r.addArgs()...); err != nil {
		return fmt.Errorf("opening host port %d for the container pool: %w", port, err)
	}
	w.cfg.Log.Info("opened L2Bridge host port for container pool", "port", port, "pool", w.plan.PoolSpec)
	return nil
}

// closeHostPort removes the allow added by openHostPort.
func (w *windowsNetworking) closeHostPort(port int) {
	if !w.cfg.L2BridgeEgress || w.plan == nil || w.plan.HostIP == "" || w.plan.PoolSpec == "" {
		return
	}
	r := hostPortAllowRule(w.plan.HostIP, w.plan.PoolSpec, port)
	if err := netsh(r.deleteArgs()...); err != nil {
		w.cfg.Log.Debug("failed to remove L2Bridge host-port allow", "port", port, "error", err)
	}
}

// hostPortRulePrefix is the DisplayName prefix of every per-job host-port allow
// (see hostPortAllowRule). Swept by prefix on shutdown so a hard kill — which
// skips dind's per-job CloseHostPort — cannot leak stale inbound allows.
const hostPortRulePrefix = firewallRulePrefix + "-l2b-hostport-"

// removeL2BridgeFirewallRules deletes the backstop rules by name, and sweeps any
// leaked per-job host-port allows by prefix.
func (w *windowsNetworking) removeL2BridgeFirewallRules() {
	if w.plan != nil {
		backstop := l2BridgeControlPlaneRules(w.plan.HostIP, w.plan.PoolSpec, w.cfg.ControlPorts)
		backstop = append(backstop, l2BridgeHostHardenRules(w.plan.HostIP, w.plan.PoolSpec)...)
		for _, r := range backstop {
			if err := netsh(r.deleteArgs()...); err != nil {
				w.cfg.Log.Debug("failed to remove L2Bridge host-firewall rule", "rule", r.name, "error", err)
			}
		}
	}
	// Prefix-sweep the per-job host-port allows. netsh delete-by-name has no
	// wildcard, so go through the firewall cmdlets. Runs regardless of w.plan —
	// a hard kill can leave these behind for a pool that no longer resolves, and
	// startup Cleanup must still reclaim them.
	if err := powershell("Get-NetFirewallRule -DisplayName '" + hostPortRulePrefix + "*' -ErrorAction SilentlyContinue | Remove-NetFirewallRule -ErrorAction SilentlyContinue"); err != nil {
		w.cfg.Log.Debug("failed to prefix-sweep L2Bridge host-port allows", "error", err)
	}
}

// defaultGatewayForAdapter returns the IPv4 default-route next hop reachable via
// the named adapter, used to fill in network.gateway when the operator has not
// pinned it. The lowest-metric route wins, matching what Windows itself would
// pick. Returns an empty string (no error) when the adapter has no default
// route, which the caller turns into a "set network.gateway" message.
func defaultGatewayForAdapter(name string) (string, error) {
	// Try the configured name and its vEthernet-renamed form (creating the
	// L2Bridge network renames the NIC — see adapterNameCandidates).
	var lastErr error
	for _, candidate := range adapterNameCandidates(name) {
		out, err := powershellOutput(fmt.Sprintf(
			"(Get-NetRoute -InterfaceAlias %s -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue "+
				"| Sort-Object -Property RouteMetric | Select-Object -First 1).NextHop",
			psQuote(candidate),
		))
		if err != nil {
			lastErr = err
			continue
		}
		if hop := strings.TrimSpace(out); hop != "" {
			return hop, nil
		}
	}
	return "", lastErr
}

func (w *windowsNetworking) installFirewallRules() error {
	// L2Bridge: the VFP ACL ladder (applied per-endpoint in setup()) is the
	// enforcement point; this installs only the host-firewall backstop that
	// fences the control-plane ports back off.
	if w.cfg.L2BridgeEgress {
		return w.installL2BridgeFirewallRules()
	}

	// NAT path: there is no host-side mechanism that can filter container egress
	// on this stack (WFP, the Hyper-V firewall, and netsh were all disproven on
	// metal — see the file header). Installing rules here would be security
	// theater: they match nothing. Log the gap and install nothing.
	w.cfg.Log.Warn("Windows container egress is NOT filtered on the default NAT network — no host-side mechanism can filter it on this stack; set network.l2bridge_egress to enforce egress (see docs/guides/security.md)")
	return nil
}

func (w *windowsNetworking) removeFirewallRules() {
	// removeL2BridgeFirewallRules is safe on either path — on NAT nothing
	// matches — and also sweeps any leaked per-job host-port allows by prefix,
	// so it must run unconditionally regardless of which path installed rules.
	w.removeL2BridgeFirewallRules()
}

// -------------------------------------------------------------------------
// netsh host-firewall helpers (shared by the L2Bridge backstop above)
// -------------------------------------------------------------------------

func netsh(args ...string) error {
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh: %w: %s", err, out)
	}
	return nil
}

// winFirewallRule is one netsh host-firewall rule: its unique name (used for
// the idempotent delete-before-add and for removal on Cleanup) and the
// key=value spec that creates it.
type winFirewallRule struct {
	name string
	spec []string
}

// addArgs returns the netsh argv that creates the rule.
func (r winFirewallRule) addArgs() []string {
	return append([]string{"advfirewall", "firewall", "add", "rule", "name=" + r.name}, r.spec...)
}

// deleteArgs returns the netsh argv that deletes every rule with this name.
func (r winFirewallRule) deleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + r.name}
}
