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
// Two layers restrict what a Windows job can reach:
//
//  1. Per-endpoint HNS ACL policies (applyACLPolicies in network_windows.go) —
//     VFP rules on the container's vSwitch port, applied in setup().
//  2. The Hyper-V firewall rules installed here, programmed with
//     New-NetFirewallHyperVRule and scoped to the container VMCreatorId.
//
// Layer 2 exists because layer 1 alone left the fleet reachable in practice:
// the containment suite (.github/workflows/containment.yml, "Fleet management
// planes must be unreachable") reached the Incus daemon and Grafana from a
// Hyper-V-isolated job (#135), so per-endpoint vSwitch ACLs cannot be the only
// line of defense.
//
// Why NOT the host Windows Defender Firewall (the netsh approach of #136).
// #136 installed host MPSSVC rules scoped localip=<container subnet>, on the
// theory that Windows Firewall would see the container's source IP on the
// WinNAT-forwarded flow. On real hardware it does not: the host firewall
// (WFP/MPSSVC) evaluates the forwarded container->LAN traffic POST-NAT at the
// host endpoint, where the source is the host's own LAN address, so a rule
// scoped to the 10.88/16 container source matches nothing. Verified against a
// live v0.1.6 Windows node: every management-plane probe still succeeded. The
// host firewall is the wrong enforcement point for NATed container egress.
//
// The Hyper-V firewall filters at the container's vNIC boundary, BEFORE NAT,
// where the packet still carries the container's own address — the correct
// enforcement point for Hyper-V-isolated Windows containers. Rules are scoped
// by -VMCreatorId so they apply to container ports, not to the host or to
// unrelated Hyper-V workloads (regular Hyper-V VMs are not filtered by the
// Hyper-V firewall at all; it governs container-class workloads — WSL, Windows
// Sandbox, and Windows containers).
//
// Why NOT a host-side WFP filter at IPFORWARD_V4 (the Linux FORWARD analogue).
// Tempting, since firewall_linux.go enforces in FORWARD. It was built and driven
// on a live Server 2025 node (a pure-Go tailscale/wf spike, no callout driver)
// and does not work: the container's forwarded + SNAT'd egress is never
// classified at ANY host IPv4 WFP layer — not IPFORWARD_V4, not
// INBOUND_IPPACKET_V4 (arrival-interface scoped or not), not OUTBOUND_IPPACKET_V4
// (dest-only, post-NAT). Positive controls prove WFP itself is enforced on the
// host (blocking the host's own 1.1.1.1 at ALE and OUTBOUND both worked), so the
// packets simply travel the Hyper-V vSwitch/HNS datapath, out-of-band of the
// host tcpip.sys WFP hooks (Get-NetNat is empty and per-interface Forwarding is
// Disabled). Full evidence: docs/arch/windows-egress-wfp-investigation.md.
// The definitive fix for this stack is network-level: an isolated VLAN whose
// uplink denies RFC1918, not a host-side software filter.
//
// Gateway safety (identical reasoning to #136, and to firewall_linux.go).
// The container subnet contains the NAT gateway (DNS, the default route,
// module-proxy GatewayPorts) and the other containers. Blocking the gateway
// would brick all container networking. Rather than rely on Hyper-V firewall
// rule-priority ordering (allow-above-deny) to rescue the gateway — an
// unverified semantic, and unverified assumptions are exactly what sank #136 —
// the container subnet is subtracted from every blocked range up front
// (subtractCIDR), so the gateway and the container-to-container range never
// appear inside any Block rule in the first place. Everything outside the
// blocked ranges — the internet, including GitHub/Docker Hub/registries — is
// untouched: the creator's default outbound action stays Allow and only
// RFC1918 + link-local is denied.
//
// IPv4 only, deliberately: the HCN NAT network is IPv4-only (no v6 IPAM), so
// containers have no IPv6 path.

// firewallRulePrefix names every rule ephemerd installs — Hyper-V firewall
// rules and the netsh fallback rules alike — so the set is findable and
// removable on Cleanup.
const firewallRulePrefix = "ephemerd-egress"

// wslVMCreatorID is the well-known Hyper-V firewall VMCreatorId for the Windows
// Subsystem for Linux (documented by Microsoft). WSL is not an ephemerd job
// container, so it is excluded from egress filtering — narrowing the blast
// radius to the container runtime's own creator(s).
const wslVMCreatorID = "{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}"

// -------------------------------------------------------------------------
// Hyper-V firewall (primary path)
// -------------------------------------------------------------------------

// hyperVRule is one New-NetFirewallHyperVRule invocation. Kept as structured
// fields (rather than a flat argv) so both the rendered PowerShell command and
// the unit tests derive from the same source, and so array-valued parameters
// (-RemoteAddresses, -RemotePorts) render as real PowerShell arrays.
type hyperVRule struct {
	name        string   // -Name; unique, ephemerd-prefixed; used for idempotent remove-before-add and Cleanup
	displayName string   // -DisplayName; mandatory, human-facing
	direction   string   // -Direction (Outbound)
	action      string   // -Action (Block)
	vmCreatorID string   // -VMCreatorId '{GUID}'
	protocol    string   // -Protocol; "" omits it (matches Any)
	remoteAddrs []string // -RemoteAddresses; addresses/CIDRs/ranges; empty omits it (matches Any)
	remotePorts []string // -RemotePorts; empty omits it (matches Any)
}

// psQuote single-quotes a value for PowerShell, doubling embedded quotes.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// psArray renders a slice as a PowerShell array literal of quoted strings
// ('a','b'), so a string[] parameter receives distinct elements rather than one
// comma-joined string.
func psArray(vals []string) string {
	q := make([]string, len(vals))
	for i, v := range vals {
		q[i] = psQuote(v)
	}
	return strings.Join(q, ",")
}

// command renders the New-NetFirewallHyperVRule command that creates the rule.
func (r hyperVRule) command() string {
	var b strings.Builder
	b.WriteString("New-NetFirewallHyperVRule")
	b.WriteString(" -Name " + psQuote(r.name))
	b.WriteString(" -DisplayName " + psQuote(r.displayName))
	b.WriteString(" -Direction " + r.direction)
	b.WriteString(" -Action " + r.action)
	b.WriteString(" -VMCreatorId " + psQuote(r.vmCreatorID))
	if r.protocol != "" {
		b.WriteString(" -Protocol " + r.protocol)
	}
	if len(r.remoteAddrs) > 0 {
		b.WriteString(" -RemoteAddresses " + psArray(r.remoteAddrs))
	}
	if len(r.remotePorts) > 0 {
		b.WriteString(" -RemotePorts " + psArray(r.remotePorts))
	}
	return b.String()
}

// removeCommand renders the idempotent delete for this rule by name.
func (r hyperVRule) removeCommand() string {
	return "Remove-NetFirewallHyperVRule -Name " + psQuote(r.name) + " -ErrorAction SilentlyContinue"
}

// creatorTag reduces a VMCreatorId GUID to a short, filesystem-safe token
// (first 8 hex digits, lowercased) used to keep rule names unique per creator
// when more than one container creator is present.
func creatorTag(vmCreatorID string) string {
	var b strings.Builder
	for _, r := range vmCreatorID {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r >= 'A' && r <= 'F':
			b.WriteRune(r + ('a' - 'A'))
		}
		if b.Len() >= 8 {
			break
		}
	}
	if b.Len() == 0 {
		return "any"
	}
	return b.String()
}

// hyperVEgressRules returns the Hyper-V firewall rule set for one container
// VMCreatorId: an outbound Block for every denied range (with the container
// subnet — and thus the gateway — carved out) plus one outbound Block per
// control port for container->gateway control-plane traffic.
//
// Exposed as a pure function (no side effects) so tests can assert the exact
// rule set without invoking PowerShell.
func hyperVEgressRules(vmCreatorID, subnet, gateway string, controlPorts []int) ([]hyperVRule, error) {
	tag := creatorTag(vmCreatorID)
	var rules []hyperVRule

	// Outbound RFC1918 + link-local blocks. subtractCIDR removes the container
	// subnet from any overlapping range so the gateway (DNS/NAT/default route)
	// and the container-to-container range never appear inside a Block. The
	// internet is never in these ranges, so it is left fully open.
	for _, cidr := range egressBlockedCIDRs {
		remote, err := subtractCIDR(cidr, subnet)
		if err != nil {
			return nil, fmt.Errorf("computing blocked ranges for %s: %w", cidr, err)
		}
		if len(remote) == 0 {
			continue // fully covered by the container subnet
		}
		rules = append(rules, hyperVRule{
			name:        fmt.Sprintf("%s-%s-block-%s", firewallRulePrefix, tag, strings.ReplaceAll(cidr, "/", "_")),
			displayName: "ephemerd egress block " + cidr,
			direction:   "Outbound",
			action:      "Block",
			vmCreatorID: vmCreatorID,
			remoteAddrs: remote,
		})
	}

	// Control-plane blocks: container -> gateway on the ephemerd control ports
	// (containerd, dispatch gRPC, debug exec). Intentionally narrow — the
	// gateway address, one TCP port each — so DNS (53), NAT, and the other
	// gateway services stay reachable. Mirrors controlPlaneInputRules on Linux;
	// evaluated Outbound here because the Hyper-V firewall filters at the
	// container vNIC, where container->gateway is egress.
	for _, port := range controlPorts {
		rules = append(rules, hyperVRule{
			name:        fmt.Sprintf("%s-%s-control-%d", firewallRulePrefix, tag, port),
			displayName: fmt.Sprintf("ephemerd egress block control tcp/%d", port),
			direction:   "Outbound",
			action:      "Block",
			vmCreatorID: vmCreatorID,
			protocol:    "TCP",
			remoteAddrs: []string{gateway},
			remotePorts: []string{strconv.Itoa(port)},
		})
	}

	return rules, nil
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

// hyperVFirewallAvailable reports whether the Hyper-V firewall cmdlets exist on
// this host. They ship with Windows 11 22H2 / Windows Server 2025 and are
// absent on older builds, where the netsh fallback is used instead.
func hyperVFirewallAvailable() bool {
	return powershell("if (Get-Command New-NetFirewallHyperVRule -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }") == nil
}

// discoverContainerVMCreators returns the VMCreatorIds of the Hyper-V firewall
// VM creators present on the host, excluding WSL. On a Windows container host
// the remaining creator(s) are the container runtime's, which is what job
// containers run under. Returns an empty slice (not an error) when no creators
// are registered yet.
func discoverContainerVMCreators() ([]string, error) {
	out, err := powershellOutput("Get-NetFirewallHyperVVMCreator | Select-Object -ExpandProperty VMCreatorId")
	if err != nil {
		return nil, err
	}
	var creators []string
	for _, line := range strings.Split(out, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if strings.EqualFold(id, wslVMCreatorID) {
			continue // not an ephemerd job container
		}
		creators = append(creators, id)
	}
	return creators, nil
}

// enableHyperVFirewallScript ensures the Hyper-V firewall is enabled for a
// creator with permissive defaults: internet, the gateway (DNS/NAT), and
// host<->container control traffic all stay open, and only our explicit Block
// rules restrict RFC1918. Without this the rules would exist but not enforce on
// a creator whose firewall was never enabled. Best-effort — logged, not fatal.
func enableHyperVFirewallScript(creator string) string {
	return fmt.Sprintf(
		"Set-NetFirewallHyperVVMSetting -Name %s -Enabled True -DefaultInboundAction Allow -DefaultOutboundAction Allow -ErrorAction Stop",
		psQuote(creator),
	)
}

// removeByPrefixScript deletes every Hyper-V firewall rule ephemerd installed,
// across all creators, matched by the ephemerd- name prefix. Catches stale
// rules leaked by a crashed run as well as the current set.
func removeByPrefixScript() string {
	return fmt.Sprintf(
		"Get-NetFirewallHyperVRule | Where-Object { $_.Name -like '%s-*' } | Remove-NetFirewallHyperVRule -ErrorAction SilentlyContinue",
		firewallRulePrefix,
	)
}

// -------------------------------------------------------------------------
// L2Bridge host-firewall backstop
// -------------------------------------------------------------------------
//
// On the L2Bridge path the primary egress enforcement is the per-endpoint VFP
// ACL ladder (buildL2BridgeEgressACLPolicies), which was proven on metal. The
// Hyper-V firewall blocks installed for NAT are deliberately NOT reused there:
// hyperVEgressRules subtracts the container subnet from every blocked range, and
// on L2Bridge the container subnet IS the management LAN — the subtraction would
// carve the management plane straight back out of the deny.
//
// What the host firewall CAN do here, and could not on NAT, is match on the
// container's source address. #136 established that host MPSSVC rules cannot
// filter NATed container egress, because the host sees that traffic post-NAT
// with its own address as the source. L2Bridge does not NAT: a container's
// packet reaches the host carrying the container's own pool address, as ordinary
// inbound LAN traffic. So an INBOUND rule scoped remoteip=<ip_pool> matches
// exactly the job containers and nothing else.
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

// installL2BridgeFirewallRules programs the L2Bridge backstop. Best-effort like
// the rest of installFirewallRules: the VFP ladder is the enforcement point and
// a host that cannot program netsh must not fail daemon startup.
func (w *windowsNetworking) installL2BridgeFirewallRules() error {
	if w.plan == nil {
		return nil
	}
	rules := l2BridgeControlPlaneRules(w.plan.HostIP, w.plan.PoolSpec, w.cfg.ControlPorts)
	if len(rules) == 0 {
		w.cfg.Log.Info("L2Bridge egress: no control ports to fence off at the host firewall")
		return nil
	}
	for _, r := range rules {
		_ = netsh(r.deleteArgs()...) // idempotent: clear any rule of this name first
		w.cfg.Log.Info("adding L2Bridge control-plane firewall rule", "rule", r.name)
		if err := netsh(r.addArgs()...); err != nil {
			w.cfg.Log.Warn("failed to add L2Bridge control-plane firewall rule", "rule", r.name, "error", err)
		}
	}
	w.cfg.Log.Info("L2Bridge control-plane firewall rules installed", "rules", len(rules))
	return nil
}

// removeL2BridgeFirewallRules deletes the backstop rules by name.
func (w *windowsNetworking) removeL2BridgeFirewallRules() {
	if w.plan == nil {
		return
	}
	for _, r := range l2BridgeControlPlaneRules(w.plan.HostIP, w.plan.PoolSpec, w.cfg.ControlPorts) {
		if err := netsh(r.deleteArgs()...); err != nil {
			w.cfg.Log.Debug("failed to remove L2Bridge control-plane firewall rule", "rule", r.name, "error", err)
		}
	}
}

// defaultGatewayForAdapter returns the IPv4 default-route next hop reachable via
// the named adapter, used to fill in network.gateway when the operator has not
// pinned it. The lowest-metric route wins, matching what Windows itself would
// pick. Returns an empty string (no error) when the adapter has no default
// route, which the caller turns into a "set network.gateway" message.
func defaultGatewayForAdapter(name string) (string, error) {
	out, err := powershellOutput(fmt.Sprintf(
		"(Get-NetRoute -InterfaceAlias %s -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue "+
			"| Sort-Object -Property RouteMetric | Select-Object -First 1).NextHop",
		psQuote(name),
	))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (w *windowsNetworking) installFirewallRules() error {
	// L2Bridge: the VFP ACL ladder is the enforcement point. Installing the NAT
	// Hyper-V/netsh rule set here would be worse than useless — it is scoped to
	// the 10.88/16 NAT subnet (matches nothing) and its subtract-the-container-
	// subnet logic would carve the management LAN out of the deny.
	if w.cfg.L2BridgeEgress {
		return w.installL2BridgeFirewallRules()
	}

	// Degrade gracefully at every step: a host that cannot program the
	// Hyper-V firewall must never fail daemon startup. It falls back to the
	// netsh host-firewall rules (weaker, but better than nothing on builds
	// without the Hyper-V firewall) or, failing that, to the per-endpoint HNS
	// ACLs already applied in setup().
	if !hyperVFirewallAvailable() {
		w.cfg.Log.Warn("New-NetFirewallHyperVRule unavailable on this host; falling back to netsh host-firewall rules")
		return w.installNetshFirewallRules()
	}

	creators, err := discoverContainerVMCreators()
	if err != nil {
		w.cfg.Log.Warn("failed to enumerate Hyper-V firewall VM creators; falling back to netsh host-firewall rules", "error", err)
		return w.installNetshFirewallRules()
	}
	if len(creators) == 0 {
		w.cfg.Log.Warn("no container Hyper-V firewall VM creator registered yet; falling back to netsh host-firewall rules")
		return w.installNetshFirewallRules()
	}

	// init() always creates the HCN network on DefaultSubnet with
	// defaultGateway (cfg.Subnet is not consulted on Windows), so the firewall
	// must match those.
	installed := 0
	for _, creator := range creators {
		rules, err := hyperVEgressRules(creator, DefaultSubnet, defaultGateway, w.cfg.ControlPorts)
		if err != nil {
			w.cfg.Log.Warn("failed to build Hyper-V firewall rules", "creator", creator, "error", err)
			continue
		}

		if err := powershell(enableHyperVFirewallScript(creator)); err != nil {
			w.cfg.Log.Warn("failed to enable Hyper-V firewall for creator (rules may not enforce)", "creator", creator, "error", err)
		}

		for _, r := range rules {
			// Idempotent: remove any rule carrying this name from a previous
			// run before adding, so re-running install never duplicates.
			_ = powershell(r.removeCommand())

			w.cfg.Log.Info("adding Hyper-V firewall rule", "rule", r.name, "creator", creator)
			if err := powershell(r.command()); err != nil {
				// Non-fatal: one failed rule degrades to the remaining rules
				// plus the per-endpoint ACLs, never to refusing to start.
				w.cfg.Log.Warn("failed to add Hyper-V firewall rule", "rule", r.name, "error", err)
				continue
			}
			installed++
		}
	}

	w.cfg.Log.Info("Hyper-V firewall rules installed", "rules", installed, "creators", len(creators))
	return nil
}

func (w *windowsNetworking) removeFirewallRules() {
	// Always attempt to remove every rule set ephemerd can install — harmless
	// if a given one was never installed — so a host that switched paths
	// between runs does not leak the other path's rules.
	w.removeL2BridgeFirewallRules()
	w.removeNetshFirewallRules()

	if !hyperVFirewallAvailable() {
		return
	}
	// Remove by prefix: catches every ephemerd rule across all creators,
	// including stale ones, without needing to recompute per-creator names.
	if err := powershell(removeByPrefixScript()); err != nil {
		w.cfg.Log.Debug("failed to remove Hyper-V firewall rules", "error", err)
	}
}

// -------------------------------------------------------------------------
// netsh host firewall (fallback for hosts without the Hyper-V firewall)
// -------------------------------------------------------------------------
//
// Retained only as a degraded fallback for Windows builds that lack
// New-NetFirewallHyperVRule (pre-Server 2025 / Windows 11 22H2). On such hosts
// these host-global rules are strictly better than nothing, even though — as
// #136 proved on Server 2025 — the host firewall evaluates NATed container
// egress post-NAT and cannot match on the container source. The outbound
// blocks are scoped localip=<container subnet> so a mis-scoped rule degrades to
// a no-op rather than cutting the host off its own management LAN.

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

// hostFirewallRules returns the netsh fallback rule set for the given container
// subnet, gateway, and control-plane ports: outbound blocks for every denied
// range (with the container subnet carved out) plus inbound blocks for
// container->gateway traffic on the control ports.
//
// Exposed as a pure function (no side effects) so tests can assert the exact
// rule set without invoking netsh.
func hostFirewallRules(subnet, gateway string, controlPorts []int) ([]winFirewallRule, error) {
	var rules []winFirewallRule

	for _, cidr := range egressBlockedCIDRs {
		remote, err := subtractCIDR(cidr, subnet)
		if err != nil {
			return nil, fmt.Errorf("computing blocked ranges for %s: %w", cidr, err)
		}
		if len(remote) == 0 {
			continue // fully covered by the container subnet
		}
		rules = append(rules, winFirewallRule{
			name: firewallRulePrefix + "-block-" + strings.ReplaceAll(cidr, "/", "_"),
			spec: []string{
				"dir=out",
				"action=block",
				"protocol=any",
				"localip=" + subnet,
				"remoteip=" + strings.Join(remote, ","),
				"profile=any",
				"enable=yes",
			},
		})
	}

	for _, port := range controlPorts {
		rules = append(rules, winFirewallRule{
			name: fmt.Sprintf("%s-control-%d", firewallRulePrefix, port),
			spec: []string{
				"dir=in",
				"action=block",
				"protocol=TCP",
				"localip=" + gateway,
				"localport=" + strconv.Itoa(port),
				"remoteip=" + subnet,
				"profile=any",
				"enable=yes",
			},
		})
	}

	return rules, nil
}

// subtractCIDR removes exclude from cidr and renders the remainder in
// address-range syntax: the original CIDR when the two do not overlap,
// otherwise up to two "start-end" ranges. Returns an empty slice when exclude
// covers cidr entirely. IPv4 only — the HCN NAT network has no IPv6 IPAM.
func subtractCIDR(cidr, exclude string) ([]string, error) {
	clo, chi, err := v4Range(cidr)
	if err != nil {
		return nil, err
	}
	xlo, xhi, err := v4Range(exclude)
	if err != nil {
		return nil, err
	}

	if xhi < clo || xlo > chi {
		return []string{cidr}, nil // no overlap — keep the CIDR as-is
	}

	var out []string
	if xlo > clo {
		out = append(out, u32ToIP(clo)+"-"+u32ToIP(xlo-1))
	}
	if xhi < chi {
		out = append(out, u32ToIP(xhi+1)+"-"+u32ToIP(chi))
	}
	return out, nil
}

// v4Range returns the first and last address of an IPv4 CIDR as uint32.
func v4Range(cidr string) (lo, hi uint32, err error) {
	r, _, err := cidrRange(cidr)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing %s: %w", cidr, err)
	}
	return r.lo, r.hi, nil
}

// u32ToIP renders a uint32 back to dotted-quad form.
func u32ToIP(v uint32) string { return u32ToIPv4(v) }

func (w *windowsNetworking) installNetshFirewallRules() error {
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, w.cfg.ControlPorts)
	if err != nil {
		w.cfg.Log.Warn("building netsh host firewall rules", "error", err)
		return nil
	}

	for _, r := range rules {
		// Idempotent: delete any rule carrying this name from a previous run
		// before adding. netsh delete removes every rule matching the name;
		// "no rules match" on a fresh host is expected and ignored.
		_ = netsh(r.deleteArgs()...)

		w.cfg.Log.Info("adding netsh firewall rule", "rule", r.name)
		if err := netsh(r.addArgs()...); err != nil {
			// Non-fatal: degrade to the per-endpoint ACLs rather than refusing
			// to start.
			w.cfg.Log.Warn("failed to add netsh firewall rule", "rule", r.name, "error", err)
		}
	}

	w.cfg.Log.Info("netsh host firewall rules installed", "rules", len(rules))
	return nil
}

func (w *windowsNetworking) removeNetshFirewallRules() {
	rules, err := hostFirewallRules(DefaultSubnet, defaultGateway, w.cfg.ControlPorts)
	if err != nil {
		w.cfg.Log.Debug("failed to rebuild netsh firewall rule set for removal", "error", err)
		return
	}
	for _, r := range rules {
		if err := netsh(r.deleteArgs()...); err != nil {
			w.cfg.Log.Debug("failed to remove netsh firewall rule", "rule", r.name, "error", err)
		}
	}
}
