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
// So on the NAT path ephemerd installs no EGRESS filtering and logs that
// container egress is unfiltered, pointing the operator at
// network.l2bridge_egress. Enforcing NAT egress requires a network-level
// control (an isolated VLAN whose uplink denies RFC1918), not a host-side
// software filter.
//
// The one host-firewall rule the NAT path DOES install is inbound, not egress:
// the per-job dind host-port allow (openHostPort). That is a different
// direction and a different question. Container -> LAN egress is forwarded and
// SNAT'd, so the host firewall cannot match on the container source; container
// -> gateway (10.88.0.1) terminates ON the host, so it is ordinary inbound
// traffic carrying the container's own 10.88.x.y source, which the host
// firewall both sees and default-denies. That deny is what broke every Windows
// docker command on a NAT node (#162), and a source-scoped inbound allow is
// what fixes it — verified on mfl-win-amd64-101 by admitting 10.88.0.0/16 to
// 10.88.0.1 and watching a previously timing-out `docker login` succeed.
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

// windowsHostHardenUDPPorts are the UDP counterparts of the management set.
// The harden rules used to be TCP-only (issue #153), which left the host's UDP
// listeners on the same services reachable from the pool even though the
// documentation claimed "the dangerous management ports are blocked". Several
// of these are real UDP services, not just symmetry: RDP negotiates a UDP
// transport on 3389, and the RPC/NetBIOS/SMB stack has historically listened on
// UDP 135/138/445. Blocking the rest costs nothing — the host has no reason to
// serve a job container on any of them.
var windowsHostHardenUDPPorts = []int{135, 139, 445, 3389, 5985, 5986, 47001}

// windowsNameResolutionUDPPorts are the broadcast/multicast name-resolution
// protocols a job container can use from its LAN-peer position on L2Bridge:
// NBNS/NBT-NS (137), the NetBIOS datagram service (138), mDNS (5353) and LLMNR
// (5355). This is the classic responder-style poisoning position — a container
// that answers one of these first can capture a Net-NTLMv2 challenge/response
// from whoever asked.
//
// These get rules with NO localip scope, unlike everything else in this file.
// That is load-bearing: LLMNR and mDNS arrive at the multicast groups
// (224.0.0.252, 224.0.0.251) and NBNS at the subnet broadcast, so the local
// address on the packet is the group/broadcast address, not the host's unicast
// address. A localip=<hostIP> rule would not match them at all — which is
// exactly how a "we block those ports" rule set can block nothing. remoteip is
// still the pool, so the block applies only to container-sourced traffic.
//
// Scope note: this stops the HOST from answering (and from being poisoned by) a
// job container. It cannot stop a container from poisoning OTHER hosts on the
// segment — a host firewall does not see traffic between two other L2 peers.
// An isolated VLAN for the pool is the only thing that removes that; see
// docs/guides/security.md.
var windowsNameResolutionUDPPorts = []int{
	137,  // NetBIOS name service (NBT-NS)
	138,  // NetBIOS datagram service
	5353, // mDNS
	5355, // LLMNR
}

// hardenRulePrefix is the DisplayName prefix of every host-harden block. Swept
// by prefix on removal so rules written by an older ephemerd — which named them
// without a protocol segment — do not survive an upgrade.
const hardenRulePrefix = firewallRulePrefix + "-l2b-harden-"

// l2BridgeHostHardenRules returns the inbound host-firewall blocks that fence
// the dangerous Windows management ports off from the container pool, on TCP
// and UDP, plus the unscoped-localip blocks for the multicast/broadcast name
// resolution protocols. Pure so the set is unit-testable without netsh.
func l2BridgeHostHardenRules(hostIP, ipPool string) []winFirewallRule {
	if hostIP == "" || ipPool == "" {
		return nil
	}

	// Host management services: unicast to the host's bridged LAN address.
	unicast := func(proto string, ports []int) []winFirewallRule {
		out := make([]winFirewallRule, 0, len(ports))
		for _, port := range ports {
			out = append(out, winFirewallRule{
				name: fmt.Sprintf("%s%s-%d", hardenRulePrefix, strings.ToLower(proto), port),
				spec: []string{
					"dir=in",
					"action=block",
					"protocol=" + proto,
					"localip=" + hostIP,
					"localport=" + strconv.Itoa(port),
					"remoteip=" + ipPool,
					"profile=any",
					"enable=yes",
				},
			})
		}
		return out
	}

	rules := make([]winFirewallRule, 0,
		len(windowsHostHardenPorts)+len(windowsHostHardenUDPPorts)+len(windowsNameResolutionUDPPorts))
	rules = append(rules, unicast("TCP", windowsHostHardenPorts)...)
	rules = append(rules, unicast("UDP", windowsHostHardenUDPPorts)...)

	// Name resolution: no localip, so the multicast/broadcast destinations
	// these actually arrive on are covered. See the var comment.
	for _, port := range windowsNameResolutionUDPPorts {
		rules = append(rules, winFirewallRule{
			name: fmt.Sprintf("%snameres-udp-%d", hardenRulePrefix, port),
			spec: []string{
				"dir=in",
				"action=block",
				"protocol=UDP",
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
		"rules", installed,
		"hardened_tcp_ports", windowsHostHardenPorts,
		"hardened_udp_ports", windowsHostHardenUDPPorts,
		"name_resolution_udp_ports", windowsNameResolutionUDPPorts)
	return nil
}

// hostPortRuleName is the unique DisplayName for one job's host-port allow.
//
// It keys on BOTH the port and the owning container's address. The port alone
// is not enough: two concurrent jobs each hold a distinct rule, and every
// open/close does an idempotent delete-by-name first, so a name that collided
// would let one job's teardown (or its delete-before-add) silently revoke
// another job's allow and break a running build. The address is also what makes
// a leaked rule attributable to the container it was opened for.
//
// Dots become dashes so the name is a single unquoted token everywhere it is
// handled (netsh argv, the PowerShell -DisplayName prefix sweep).
func hostPortRuleName(containerIP string, port int) string {
	return fmt.Sprintf("%s%d-%s", hostPortRulePrefix, port, strings.ReplaceAll(containerIP, ".", "-"))
}

// normalizeContainerIP validates the address an allow is about to be scoped to
// and renders it in canonical dotted-quad form.
//
// This is the fail-closed gate for the whole host-port mechanism: callers MUST
// propagate the error rather than substituting a wider scope. See the comment
// on hostPortAllowRule for why widening is not an option here.
func normalizeContainerIP(containerIP string) (string, error) {
	v, err := parseIPv4(strings.TrimSpace(containerIP))
	if err != nil {
		return "", fmt.Errorf("container address %q is not a usable IPv4 address: %w", containerIP, err)
	}
	return u32ToIPv4(v), nil
}

// hostPortAllowRule is the scoped inbound allow that makes one host TCP port
// reachable from ONE job container (see openHostPort). Used on both Windows
// paths; only hostIP differs (the host's LAN address on L2Bridge, the bridge
// gateway on NAT).
//
// remoteip is that container's own /32 — never the pool. This is a security
// boundary, not a tidiness preference. The per-job service behind the port is
// the fake Docker API (pkg/dind), which performs NO authentication: anything
// that can open a TCP connection to it gets full control of that job's Docker
// daemon — run and exec containers, bind-mount arbitrary host paths, read the
// job's build context and secrets. An earlier revision scoped this allow to
// remoteip=<ip_pool>, i.e. to every job container on the host, so container A
// could port-scan the host's LAN address across the ephemeral range, find
// container B's dind endpoint and drive B's daemon — a cross-job
// confidentiality and integrity break bounded only by port guessing. With the
// /32 the host firewall is the authenticator the dind API does not have.
//
// caller contract: containerIP has already been through normalizeContainerIP.
func hostPortAllowRule(hostIP, containerIP string, port int) winFirewallRule {
	return winFirewallRule{
		name: hostPortRuleName(containerIP, port),
		spec: []string{
			"dir=in",
			"action=allow",
			"protocol=TCP",
			"localip=" + hostIP,
			"localport=" + strconv.Itoa(port),
			// /32 to match the CIDR form the rest of the rule set uses.
			"remoteip=" + containerIP + "/32",
			"profile=any",
			"enable=yes",
		},
	}
}

// hostPortLocalIP returns the host address a per-job dind allow must be scoped
// to, and whether this path needs an allow at all.
//
// It MUST return the address dind actually bound its listener to, which is
// Manager.GatewayIP():
//
//   - L2Bridge: hostAddr() reports plan.HostIP, the host's own LAN address.
//   - NAT: hostAddr() reports "", so GatewayIP falls through to the subnet
//     derivation — the .1 of the container subnet, i.e. 10.88.0.1 by default.
//     gatewayForSubnet is that same derivation, shared rather than duplicated
//     so the two cannot drift into scoping the allow at an address nothing is
//     listening on.
//
// The false return is reserved for the one genuinely unresolvable case:
// L2Bridge egress enabled but no address plan (init failed, or the daemon is
// mid-teardown). NAT always resolves.
func (w *windowsNetworking) hostPortLocalIP() (string, bool) {
	if w.cfg.L2BridgeEgress {
		if w.plan == nil || w.plan.HostIP == "" {
			return "", false
		}
		return w.plan.HostIP, true
	}
	return gatewayForSubnet(w.cfg.Subnet), true
}

// hostPortRuleFor resolves the one firewall rule an open/close pair acts on.
//
// Both openHostPort and closeHostPort go through it, and that is what
// guarantees teardown deletes exactly the rule setup added: a single function
// decides the host address, the container /32, and therefore the rule name that
// netsh matches on. The bool reports whether this path installs a rule at all.
func (w *windowsNetworking) hostPortRuleFor(port int, containerIP string) (winFirewallRule, bool, error) {
	hostIP, needed := w.hostPortLocalIP()
	if !needed {
		return winFirewallRule{}, false, nil
	}
	ip, err := normalizeContainerIP(containerIP)
	if err != nil {
		return winFirewallRule{}, false, err
	}
	return hostPortAllowRule(hostIP, ip, port), true, nil
}

// openHostPort adds a scoped inbound allow so ONE job container — the one at
// containerIP — can reach one host TCP port (its own dind Docker API listener).
// Without it the host's default inbound deny drops the connection and every
// docker command in the job fails with an i/o timeout.
//
// Applies to BOTH Windows paths. On L2Bridge the VFP host /32 allow lets the
// container's packet leave its vSwitch port, but the host firewall still
// default-denies it. On NAT there is no VFP layer at all, yet the same host
// firewall still default-denies the container's inbound connection to the
// bridge gateway — which is #162: a NAT node took the old L2Bridge-only early
// return, installed nothing, and silently dropped every job's dind SYN.
//
// Scoped to remoteip=<containerIP>/32 and localport=<port>, so neither another
// port on the host nor another job's container is admitted. The /32 matters as
// much on NAT as on L2Bridge: every job container on a NAT node shares
// 10.88.0.0/16 and can address 10.88.0.1, so a subnet-wide allow would let one
// job port-scan the gateway's ephemeral range and drive another job's
// unauthenticated Docker API — the exact cross-job break #152 closed.
//
// Fails closed on a missing or malformed containerIP: it opens nothing and
// returns an error. The tempting fallback — open the subnet when the exact
// address is unknown — is precisely the cross-job hole this scoping exists to
// close, so an unknown address must cost the job its docker access, not cost
// every other job its isolation.
func (w *windowsNetworking) openHostPort(port int, containerIP string) error {
	r, needed, err := w.hostPortRuleFor(port, containerIP)
	if err != nil {
		return fmt.Errorf("refusing to open host port %d without a specific container address (a subnet-wide allow would expose this job's unauthenticated Docker API to every other job): %w", port, err)
	}
	if !needed {
		return nil
	}
	_ = netsh(r.deleteArgs()...) // idempotent
	if err := netsh(r.addArgs()...); err != nil {
		return fmt.Errorf("opening host port %d for container %s: %w", port, containerIP, err)
	}
	w.cfg.Log.Info("opened host port for a single job container",
		"port", port, "container_ip", containerIP, "rule", r.name, "l2bridge", w.cfg.L2BridgeEgress)
	return nil
}

// closeHostPort removes the allow added by openHostPort. containerIP must be
// the same address the allow was opened for — it is half the rule name, which
// is what keeps one job's teardown from deleting another job's rule.
func (w *windowsNetworking) closeHostPort(port int, containerIP string) {
	r, needed, err := w.hostPortRuleFor(port, containerIP)
	if err != nil {
		// Nothing was opened for an address we cannot parse, so there is
		// nothing to close; the shutdown prefix sweep is the backstop.
		w.cfg.Log.Debug("skipping host-port close for an unusable container address", "port", port, "container_ip", containerIP, "error", err)
		return
	}
	if !needed {
		return
	}
	if err := netsh(r.deleteArgs()...); err != nil {
		w.cfg.Log.Debug("failed to remove host-port allow", "port", port, "container_ip", containerIP, "error", err)
	}
}

// hostPortRulePrefix is the DisplayName prefix of every per-job host-port allow
// (see hostPortAllowRule). Swept by prefix on shutdown so a hard kill — which
// skips dind's per-job CloseHostPort — cannot leak stale inbound allows.
//
// The "-l2b-" token is historical: these allows are now installed on the NAT
// path too. It is deliberately NOT renamed. The prefix is the ONLY handle the
// shutdown sweep has on a leaked rule, so changing it would strand every allow
// left behind by the previously-running build — the one moment the sweep exists
// for. The value is a wire format between versions, not a description.
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
	// Prefix-sweep the host-harden blocks too. The delete-by-name loop above
	// only knows the CURRENT names; an ephemerd that predates the UDP work
	// wrote them as "<prefix>-l2b-harden-<port>" with no protocol segment, and
	// those would otherwise survive every subsequent Cleanup.
	if err := powershell("Get-NetFirewallRule -DisplayName '" + hardenRulePrefix + "*' -ErrorAction SilentlyContinue | Remove-NetFirewallRule -ErrorAction SilentlyContinue"); err != nil {
		w.cfg.Log.Debug("failed to prefix-sweep L2Bridge host-harden blocks", "error", err)
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

	// NAT path: there is no host-side mechanism that can filter container EGRESS
	// on this stack (WFP, the Hyper-V firewall, and netsh were all disproven on
	// metal — see the file header). Installing egress rules here would be
	// security theater: they match nothing. Log the gap and install nothing.
	//
	// Note this is the daemon-wide install only. The NAT path still installs the
	// per-job dind INBOUND allow in openHostPort, which does match — that
	// traffic terminates on the host rather than being forwarded and SNAT'd.
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
