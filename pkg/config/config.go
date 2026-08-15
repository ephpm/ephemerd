package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub      GitHubConfig      `toml:"github"`
	GitHubExtra []GitHubConfig    `toml:"github_extra"` // additional GitHub owners, each with its own auth ([[github_extra]])
	Forgejo     ForgejoConfig     `toml:"forgejo"`
	Gitea       GiteaConfig       `toml:"gitea"`
	GitLab      GitLabConfig      `toml:"gitlab"`
	Woodpecker  WoodpeckerConfig  `toml:"woodpecker"`
	Webhook     WebhookConfig     `toml:"webhook"`
	Containerd  ContainerdConfig  `toml:"containerd"`
	Network     NetworkConfig     `toml:"network"`
	VM          VMConfig          `toml:"vm"`
	Dind        DindConfig        `toml:"dind"`
	BuildKit    BuildKitConfig    `toml:"buildkit"`
	ImageGC     ImageGCConfig     `toml:"image_gc"`
	ModuleProxy ModuleProxyConfig `toml:"module_proxy"`
	CargoProxy  CargoProxyConfig  `toml:"cargo_proxy"`
	Runtime     RuntimeConfig     `toml:"runtime"`
	Runner      RunnerConfig      `toml:"runner"`
	Metrics     MetricsConfig     `toml:"metrics"`
	Dispatch    DispatchConfig    `toml:"dispatch"`
	Log         LogConfig         `toml:"log"`
}

// DispatchConfig configures the host<->VM dispatch gRPC channel. On Windows
// (Hyper-V) and macOS (Vz) hosts, Linux jobs run inside a long-lived Linux VM
// and the host dispatches Create/Wait/Destroy over gRPC. That surface can
// spawn and kill jobs with a caller-supplied image, so it is authenticated
// with a shared bearer token.
type DispatchConfig struct {
	// Token is the shared bearer token the host client presents and the in-VM
	// dispatch server requires on every RPC (constant-time compared). It is
	// delivered to the VM inside config.toml through the existing config-share
	// channel, so both sides converge on the same value with no extra plumbing.
	//
	// Leave empty for the daemon to mint a 256-bit token on first run and
	// persist it back into config.toml (see EnsureDispatchToken) — operators
	// normally never set this by hand. Set it explicitly only to pin a known
	// value (e.g. to rotate, or in a config baked read-only).
	Token string `toml:"token"`
}

// GitHubTargets returns every configured GitHub target: the primary [github]
// (when set) followed by any [[github_extra]] entries. Each target carries its
// own owner + auth, so ephemerd can serve multiple owners at once (e.g. an org
// via a GitHub App and a personal account via a PAT).
func (c *Config) GitHubTargets() []GitHubConfig {
	var targets []GitHubConfig
	if c.GitHub.Owner != "" || c.GitHub.Token != "" || c.GitHub.AppID != 0 {
		targets = append(targets, c.GitHub)
	}
	targets = append(targets, c.GitHubExtra...)
	return targets
}

// Provider returns the name of the first configured forge provider.
// Returns "github" by default for backward compatibility.
func (c *Config) Provider() string {
	ps := c.Providers()
	if len(ps) > 0 {
		return ps[0]
	}
	return "github"
}

// Providers returns all configured provider names.
// A provider is "configured" if its credential/URL fields are set.
func (c *Config) Providers() []string {
	var ps []string
	if len(c.GitHubTargets()) > 0 {
		ps = append(ps, "github")
	}
	if c.Forgejo.InstanceURL != "" {
		ps = append(ps, "forgejo")
	}
	if c.Gitea.InstanceURL != "" {
		ps = append(ps, "gitea")
	}
	if c.GitLab.InstanceURL != "" {
		ps = append(ps, "gitlab")
	}
	if c.Woodpecker.ServerURL != "" {
		ps = append(ps, "woodpecker")
	}
	return ps
}

// MetricsConfig configures the Prometheus metrics endpoint.
// Disabled by default. Set enabled = true to expose /metrics.
type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`  // enable /metrics endpoint (default false)
	Port    int    `toml:"port"`     // listen port (default 9090)
	Path    string `toml:"path"`     // metrics path (default "/metrics")
	TLSCert string `toml:"tls_cert"` // TLS certificate path (optional)
	TLSKey  string `toml:"tls_key"`  // TLS private key path (optional)
	// BindAddr is the interface the metrics listener binds to. The endpoint is
	// unauthenticated, so it defaults to "127.0.0.1" (loopback only). Set to
	// "0.0.0.0" to scrape from another host, and firewall the port and/or set
	// tls_cert/tls_key when you do.
	BindAddr string `toml:"bind_addr"`
	// ContainerStatsInterval is how often per-container resource samples are
	// taken (CPU, memory). Used both by the host's local sampler ticker and as
	// the cadence the host requests from the in-VM Dispatch StreamContainerStats
	// stream. Default 10s.
	ContainerStatsInterval string `toml:"container_stats_interval"`
}

// ParsedContainerStatsInterval returns the configured per-container sampling
// interval, applying the default (10s) when unset. Falls back to the default
// on parse error rather than failing the daemon — the metric series simply
// won't update if this is misconfigured, which is not worth aborting startup.
func (m MetricsConfig) ParsedContainerStatsInterval() time.Duration {
	const defaultInterval = 10 * time.Second
	if m.ContainerStatsInterval == "" {
		return defaultInterval
	}
	d, err := time.ParseDuration(m.ContainerStatsInterval)
	if err != nil || d <= 0 {
		return defaultInterval
	}
	return d
}

// WebhookConfig configures webhook delivery and tunnel providers.
// By default, ephemerd uses polling (tunnel = "none").
// Set tunnel = "localtunnel", "ngrok", or "cloudflared" for ephemerd to create
// and manage a tunnel and auto-register the GitHub webhook. Set tunnel =
// "external" when a tunnel is provided by something else: ephemerd then serves
// the webhook receiver and disables polling, but does not create a tunnel or
// register the webhook — that is owned externally, so a matching secret is
// required.
type WebhookConfig struct {
	Secret  string `toml:"secret"`   // webhook HMAC secret (auto-generated for managed tunnels; required for "external")
	Port    int    `toml:"port"`     // listen port for health endpoint (default 8080)
	TLSCert string `toml:"tls_cert"` // TLS certificate path (direct TLS, no tunnel)
	TLSKey  string `toml:"tls_key"`  // TLS private key path
	Tunnel  string `toml:"tunnel"`   // "none" (default, polling), "external" (unmanaged ingress), "localtunnel", "ngrok", or "cloudflared"
	// ExternalURL is the public base URL of the externally-managed tunnel
	// (e.g. https://mac.tricorder.cc). When set with tunnel="external",
	// ephemerd registers each tracked repo's webhook to
	// <external_url>/webhook/<provider> using the secret. Ignored for managed
	// tunnels (they use the tunnel's own URL) and for polling.
	ExternalURL      string `toml:"external_url"`
	TunnelURL        string `toml:"tunnel_url"`         // localtunnel: self-hosted server URL
	NgrokAuthtoken   string `toml:"ngrok_authtoken"`    // ngrok auth token (or use NGROK_AUTHTOKEN env)
	TunnelMaxRetries int    `toml:"tunnel_max_retries"` // max consecutive reconnect failures before falling back to polling (default 5)

	// cloudflared: tunnel = "cloudflared". Ephemerd runs cloudflared as a
	// managed subprocess bound to its own lifetime (child gets SIGTERM on
	// parent exit). The tunnel and its DNS record must be provisioned in
	// Cloudflare beforehand — ephemerd only runs the client. The token
	// authenticates and identifies which tunnel to connect.
	CloudflaredToken    string `toml:"cloudflared_token"`    // tunnel run token (or use CLOUDFLARE_TUNNEL_TOKEN env)
	CloudflaredHostname string `toml:"cloudflared_hostname"` // public FQDN of the tunnel (e.g. "runner.example.com"); required for GitHub webhook registration
	CloudflaredVersion  string `toml:"cloudflared_version"`  // pinned cloudflared release (e.g. "2026.6.1"); defaults to a known-good version if empty

	// Pool marks this instance as one member of a pool of ephemerd nodes
	// sharing a single public webhook URL (e.g. cloudflared tunnel replicas
	// behind one hostname). In pool mode webhook registration is
	// adopt-or-create (an existing hook with the same URL is converged, not
	// duplicated), the hook is never deregistered on shutdown (pool-mates
	// still need it), and the startup stale-hook sweep is skipped (it cannot
	// tell a pool-mate's live hook from a stale one). Requires an explicit
	// shared webhook.secret — every pool member must present the same one.
	Pool bool `toml:"pool"`

	// ReconcileInterval controls the webhook-mode reconcile sweep: how often
	// ephemerd re-runs the catch-up poll to pick up jobs that got stranded.
	//
	// This is a LAST-RESORT backstop only for genuinely DROPPED webhook
	// deliveries (network/tunnel loss where GitHub's own redelivery also
	// missed us). The common stranding case — a fungibly-reassigned runner
	// leaving its dispatched job queued — is now healed instantly and
	// event-drivenly by the scheduler on runner exit, without polling. So this
	// runs at a low frequency: empty = default 30m; a zero/negative duration
	// disables it entirely (relying purely on the event-driven path + GitHub's
	// delivery retries).
	ReconcileInterval string `toml:"reconcile_interval"`
}

// ResolvedReconcileInterval returns the webhook-mode reconcile sweep interval:
// 30m by default (empty or unparseable), the parsed value otherwise, and 0
// (disabled) only when explicitly set to a zero/negative duration.
func (w *WebhookConfig) ResolvedReconcileInterval() time.Duration {
	if w == nil || w.ReconcileInterval == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(w.ReconcileInterval)
	if err != nil {
		return 30 * time.Minute
	}
	if d < 0 {
		return 0
	}
	return d
}

// NetworkConfig configures container networking.
type NetworkConfig struct {
	// Subnet is the container subnet.
	//
	// Linux (CNI bridge): auto-selected when empty, avoiding ranges already in
	// use on the host.
	//
	// Windows L2Bridge (l2bridge_egress = true): the CIDR of the LAN the bridge
	// is attached to — containers are peers on it, not behind NAT — declared as
	// the HNS network's Ipam subnet. Auto-derived from the address configured on
	// host_nic when empty, which is the expected setting. Pin it only when the
	// adapter carries a prefix that differs from the LAN you want declared.
	//
	// Windows NAT (the default): not consulted; the HNS NAT network always uses
	// the built-in 10.88.0.0/16.
	Subnet string `toml:"subnet"`

	MTU int `toml:"mtu"` // bridge MTU (auto-detected from host if 0)

	// L2BridgeEgress opts a Windows pool into L2Bridge container networking
	// with VFP-enforced egress filtering, instead of the default HNS NAT.
	// NAT cannot software-filter Windows container egress (VFP does not engage
	// on a NAT network); L2Bridge puts the container on a VFP-managed vSwitch
	// port so per-endpoint ACLs actually enforce. Windows only; ignored on
	// Linux/macOS. Default false — NAT stays the default and this flag flips
	// nothing until an operator opts a pool in.
	L2BridgeEgress bool `toml:"l2bridge_egress"`

	// HostNIC is the host network adapter name the L2Bridge binds onto
	// (e.g. "Ethernet"). REQUIRED when L2BridgeEgress is true — the bridge
	// has no uplink without it. There is no default: the correct NIC name is
	// host-specific (do not assume "Ethernet 2", which was a spike's
	// hot-added test NIC). Ignored when L2BridgeEgress is false.
	HostNIC string `toml:"host_nic"`

	// IPPool is the range of LAN addresses ephemerd may assign to job
	// containers on the L2Bridge path. REQUIRED when L2BridgeEgress is true,
	// with no default, and validated at load time.
	//
	// Why it cannot be inferred: an L2Bridge network must declare a subnet (HNS
	// rejects a subnet-less one outright), and once it has one HNS will assign
	// endpoint addresses from anywhere inside it — on a real LAN, straight into
	// the site DHCP server's scope. ephemerd therefore allocates addresses
	// itself, and only the operator knows which slice of their LAN the DHCP
	// server is configured never to lease.
	//
	// Accepts a CIDR ("192.0.2.192/27" — network and broadcast excluded) or an
	// inclusive range ("192.0.2.200-192.0.2.230"). Must lie inside Subnet and
	// must not contain the host's own address or the LAN gateway. Size it for
	// at least runner.max_concurrent addresses. Ignored when L2BridgeEgress is
	// false.
	IPPool string `toml:"ip_pool"`

	// Gateway is the LAN router the L2Bridge default route points at (the HNS
	// Ipam route next hop). Auto-derived from the default route on HostNIC when
	// empty, which is the expected setting; pin it only when the adapter has no
	// default route of its own or carries more than one.
	//
	// Containers route THROUGH this address while the egress ACLs stop them
	// ADDRESSING it — the gateway gets no allow rule. Windows L2Bridge only.
	Gateway string `toml:"gateway"`

	// PublicDNS is the DNS resolver list handed to L2Bridge containers. Public
	// resolvers keep container DNS off the LAN router (which the egress ACLs
	// block along with the rest of RFC1918). Empty falls back to a built-in
	// public default (1.1.1.1, 8.8.8.8). Only consulted on the L2Bridge path.
	PublicDNS []string `toml:"public_dns"`

	// ExtraAllowedDestinations are additional CIDRs permitted through the
	// L2Bridge egress ACLs at a precedence ABOVE the RFC1918 block (so a
	// listed destination wins over the block). Reserved for future use —
	// default empty, which reproduces the strict Linux end-state (no RFC1918
	// carve-outs at all). Only consulted on the L2Bridge path.
	ExtraAllowedDestinations []string `toml:"extra_allowed_destinations"`
}

// validate rejects an L2Bridge configuration that cannot be completed safely at
// runtime. It runs at config load, on every platform, so a node whose config was
// rendered wrong dies at startup with a specific message instead of coming up
// and quietly mis-addressing containers.
//
// Only the two settings that cannot be inferred are required. subnet, gateway,
// and public_dns are all derived from the host's real primary adapter when
// unset, so nothing here assumes any particular network. ip_pool deliberately
// has NO default: on L2Bridge, containers take addresses on the operator's own
// LAN, and a built-in guess would hand out addresses the site's DHCP server is
// also leasing. Semantic checks that need the host (does the pool fit inside the
// adapter's subnet, does it swallow the gateway) happen in pkg/networking once
// the adapter has been read.
func (n *NetworkConfig) validate() error {
	if !n.L2BridgeEgress {
		return nil
	}
	if strings.TrimSpace(n.HostNIC) == "" {
		return fmt.Errorf(`network.host_nic is required when network.l2bridge_egress = true: ` +
			`set it to the host adapter the bridge attaches to — the name shown by ` + "`Get-NetAdapter`" + `, ` +
			`e.g. host_nic = "Ethernet". There is no default; the correct adapter is host-specific`)
	}
	if strings.TrimSpace(n.IPPool) == "" {
		return fmt.Errorf(`network.ip_pool is required when network.l2bridge_egress = true: ` +
			`on L2Bridge, job containers are addressed on this host's own LAN rather than behind NAT, ` +
			`so ephemerd must be told which addresses it may hand out. Set it to a range your DHCP server ` +
			`is configured never to lease — either a CIDR (ip_pool = "192.0.2.192/27") or an inclusive ` +
			`range (ip_pool = "192.0.2.200-192.0.2.230") — sized for at least runner.max_concurrent ` +
			`containers. There is no default: any built-in guess would collide with live DHCP leases`)
	}
	return nil
}

type ContainerdConfig struct {
	// Reserved for future containerd-specific settings (e.g. snapshotter overrides)
}

// RuntimeConfig configures behavior of the per-job container runtime —
// things that apply to the OCI spec rather than to a specific subsystem
// like dind or networking.
type RuntimeConfig struct {
	Rlimits RuntimeRlimits `toml:"rlimits"`

	// AllowNewPrivileges controls whether a process in the runner
	// container may gain privileges through execve — i.e. whether setuid
	// binaries and file capabilities take effect. When true the OCI spec
	// carries NoNewPrivileges=false, which is what makes `sudo` work.
	//
	// Jobs generally need this: `sudo apt-get install` is routine in CI,
	// and the stock actions-runner image expects it. That is why the
	// default is true.
	//
	// SECURITY: the runner container already runs as root with a trimmed
	// capability set (see runtime.containerCapabilities), so with
	// NoNewPrivileges=false the seccomp and AppArmor profiles carry
	// nearly all of the containment on their own. A pool that builds
	// untrusted code — fork PRs especially — should set this to false and
	// accept that `sudo` and `apt-get install` stop working there.
	//
	// This is NOT the same knob as dind's allow_privileged: that one
	// gates what a *sibling* container launched through the fake Docker
	// API may request, while this one applies to the runner container
	// itself. See DindConfig.AllowPrivileged.
	//
	// Use the pointer form so a missing TOML key is distinguishable from
	// an explicit `allow_new_privileges = false`. See
	// ResolvedAllowNewPrivileges for the default policy.
	AllowNewPrivileges *bool `toml:"allow_new_privileges"`
}

// ResolvedAllowNewPrivileges returns whether the runner container may
// gain privileges through execve, applying the default when the operator
// hasn't set the key explicitly.
//
// Default policy: true on all platforms — this preserves the behavior
// ephemerd has always had. Unlike dind's allow_privileged, a secure-by
// -default of false would break `sudo apt-get install` in every existing
// workflow, so tightening it is an explicit operator decision per pool.
func (r RuntimeConfig) ResolvedAllowNewPrivileges() bool {
	if r.AllowNewPrivileges != nil {
		return *r.AllowNewPrivileges
	}
	return true
}

// RuntimeRlimits sets POSIX resource limits (RLIMIT_*) on each runner
// container's OCI spec. Defaults match containerd's built-in OCI spec
// (nofile=1024, nproc=1024) so an empty config is a no-behavior-change.
//
// Set higher when CI workloads need more file descriptors or processes
// than containerd's defaults allow. Common case: a build tool calling
// `ulimit -n 2048` to raise its open-file ceiling. That fails with
// "Operation not permitted" if the container's hard limit is 1024 —
// raising the hard limit needs CAP_SYS_RESOURCE, which we deliberately
// don't grant. Setting nofile higher here lets the same `ulimit` call
// succeed without granting the capability, because lowering is always
// allowed and the OCI hard limit is now generous.
type RuntimeRlimits struct {
	// Nofile is RLIMIT_NOFILE (max open file descriptors). Both soft
	// and hard get set to this value. Default 1024 (containerd default).
	Nofile int64 `toml:"nofile"`
	// Nproc is RLIMIT_NPROC (max processes/threads for the container's
	// user). Both soft and hard get set to this value. Default 1024.
	Nproc int64 `toml:"nproc"`
}

// Resolved returns the rlimits with defaults filled in for any unset
// (zero or negative) field. Always returns positive values so callers
// can blindly emit OCI rlimit entries.
func (r RuntimeRlimits) Resolved() RuntimeRlimits {
	if r.Nofile <= 0 {
		r.Nofile = 1024
	}
	if r.Nproc <= 0 {
		r.Nproc = 1024
	}
	return r
}

// DindConfig configures the fake Docker daemon mounted into job containers.
type DindConfig struct {
	Enabled bool `toml:"enabled"` // mount /var/run/docker.sock with a fake Docker API

	// CachePruneInterval is how often the per-repo image cache pruner runs.
	// Accepts standard Go duration strings ("24h", "30m"). Set to 0 to
	// disable pruning entirely. Default 24h.
	CachePruneInterval time.Duration `toml:"cache_prune_interval"`

	// CacheMaxAge is an OPTIONAL age backstop for cached image records:
	// any record whose ephemerd.io/last-accessed label (or UpdatedAt as
	// fallback) is older than this gets removed on the next prune pass.
	// Containerd's content GC then reclaims the unreferenced blobs.
	//
	// BEHAVIOR CHANGE: this used to default to 168h (7 days) and was the
	// only image eviction mechanism ephemerd had. It now defaults to 0
	// (disabled), because disk pressure — not age — is the correct
	// trigger: evicting a warm cache while the disk is half empty just
	// forces re-downloads. See [image_gc], which supersedes this for
	// both the dind cache namespaces and the main runtime namespace.
	//
	// An explicit value is still honored, and still applies only to the
	// ephemerd-dind-cache-* namespaces. Empty cache namespaces are
	// reaped on every prune pass regardless of this setting.
	CacheMaxAge time.Duration `toml:"cache_max_age"`

	// AllowPrivileged controls whether `docker run --privileged` (or
	// HostConfig.Privileged=true / HostConfig.CapAdd) from inside a job
	// is honored. When true, a sibling container can request the full
	// elevation stack (all caps, all devices, seccomp/apparmor off,
	// writable sysfs/cgroupfs) — needed for KIND clusters, nested
	// containerd, /dev/fuse-style mounts, etc. When false, such requests
	// are rejected with HTTP 403.
	//
	// SECURITY: a privileged sibling container is effectively root on
	// whatever host runs the containerd that backs dind. On Windows and
	// macOS hosts that backing containerd lives inside a managed Linux
	// VM (WSL2 / Hyper-V / Vz), so an escape only reaches the VM. On a
	// Linux host with no VM fence, an escape reaches the bare-metal host
	// — set this to false unless every workload is trusted.
	//
	// Use the pointer form so an empty/missing TOML key is
	// distinguishable from an explicit `allow_privileged = false`. See
	// ResolvedAllowPrivileged for the default policy.
	AllowPrivileged *bool `toml:"allow_privileged"`
}

// ResolvedAllowPrivileged returns whether privileged dind sibling
// containers are allowed, applying the secure default when the operator
// hasn't set the key explicitly.
//
// Default policy: false on ALL platforms. Privileged is opt-in everywhere.
// A privileged sibling container is effectively root on whatever host runs
// the backing containerd, so shipping it on-by-default is unsafe even where
// a VM fence limits the blast radius (Windows/macOS) — an operator that
// needs it (KIND clusters, nested containerd, /dev/fuse) opts in explicitly
// with `allow_privileged = true`.
func (d *DindConfig) ResolvedAllowPrivileged() bool {
	if d.AllowPrivileged != nil {
		return *d.AllowPrivileged
	}
	return false
}

// DindCachePruneInterval returns the prune interval with the default
// applied when unset (or set to 0).
func (d *DindConfig) DindCachePruneInterval() time.Duration {
	if d.CachePruneInterval == 0 {
		return 24 * time.Hour
	}
	return d.CachePruneInterval
}

// DindCacheMaxAge returns the optional age backstop for dind cache
// namespaces. Zero means disabled, which is now the default — see
// DindConfig.CacheMaxAge for why. A negative value is also treated as
// disabled so a typo cannot evict everything.
func (d *DindConfig) DindCacheMaxAge() time.Duration {
	if d.CacheMaxAge < 0 {
		return 0
	}
	return d.CacheMaxAge
}

// ImageGCConfig configures disk-pressure-triggered container image garbage
// collection.
//
// Model (kubelet's): disk pressure is the TRIGGER, least-recently-used is
// the ORDER. Collection starts when usage crosses a high watermark and
// evicts LRU-first until a distinctly lower low watermark is reached, then
// stops. Two watermarks rather than one line is what prevents thrashing at
// the boundary.
//
// Two independent trigger arms exist and the more conservative one wins:
// a percentage (high_watermark_percent) and an absolute floor
// (min_free_gb). Neither is safe alone — 15% free of a 1 TB node is 150 GB
// and evicting there is pointless, while 15% free of a 100 GB node is
// 15 GB, which three concurrent jobs writing ~5 GB of container layers each
// can eat between ticks. Size min_free_gb relative to
// runner.max_concurrent times the expected per-job writable layer.
//
// Scope is the main "ephemerd" runtime namespace AND the per-repo
// "ephemerd-dind-cache-*" namespaces. Images referenced by an existing
// container, and the node's configured runner images, are never evicted.
type ImageGCConfig struct {
	// Enabled toggles collection. Nil = default true; operators disable
	// by explicitly setting enabled = false.
	Enabled *bool `toml:"enabled"`

	// CheckInterval is how often disk usage is sampled. The sample is one
	// statfs-class syscall (microseconds), so this can be short. Default
	// 60s. Set to 0 to disable the periodic sweep — the pre-pull headroom
	// check still runs.
	CheckInterval time.Duration `toml:"check_interval"`

	// HighWatermarkPercent is the disk used-percentage at which a
	// collection pass triggers. Default 85. Set to 0 to disable the
	// percentage arm and rely on min_free_gb alone.
	HighWatermarkPercent float64 `toml:"high_watermark_percent"`

	// LowWatermarkPercent is the used-percentage a triggered pass evicts
	// down to. Default 70. Must be below HighWatermarkPercent; a value at
	// or above it degrades to single-threshold behavior.
	LowWatermarkPercent float64 `toml:"low_watermark_percent"`

	// MinFreeGB is the absolute free-space floor, in GiB, below which a
	// pass triggers regardless of percentage. Default 20. Set to 0 to
	// disable the absolute arm.
	MinFreeGB uint64 `toml:"min_free_gb"`

	// TargetFreeGB is the free space, in GiB, a pass triggered by
	// MinFreeGB evicts back up to. Defaults to twice MinFreeGB, mirroring
	// the default 85%/70% percentage gap. Values below MinFreeGB are
	// clamped up to it.
	TargetFreeGB uint64 `toml:"target_free_gb"`

	// MaxAge is an OPTIONAL age backstop applied to every collected
	// namespace: records idle longer than this are evicted whether or not
	// the disk is under pressure. Default 0 (disabled) — age is
	// deliberately NOT the primary mechanism, because evicting a warm
	// cache while the disk is half empty just forces re-downloads.
	MaxAge time.Duration `toml:"max_age"`
}

// ImageGCEnabled reports whether image garbage collection runs. Default true.
func (i *ImageGCConfig) ImageGCEnabled() bool {
	if i.Enabled != nil {
		return *i.Enabled
	}
	return true
}

// ImageGCCheckInterval returns the sampling interval, defaulting to 60s.
// A negative value is treated as 0 (periodic sweep off).
func (i *ImageGCConfig) ImageGCCheckInterval() time.Duration {
	if i.CheckInterval == 0 {
		return 60 * time.Second
	}
	if i.CheckInterval < 0 {
		return 0
	}
	return i.CheckInterval
}

// ImageGCHighWatermarkPercent returns the trigger percentage, defaulting to
// 85. Out-of-range values fall back to the default rather than failing
// startup — a misconfigured watermark should not stop the node collecting.
func (i *ImageGCConfig) ImageGCHighWatermarkPercent() float64 {
	if i.HighWatermarkPercent < 0 || i.HighWatermarkPercent > 100 {
		return 85
	}
	if i.HighWatermarkPercent == 0 {
		// An explicit 0 is indistinguishable from "unset" in TOML for a
		// float, so treat it as unset. Operators who want the
		// percentage arm off should raise min_free_gb instead.
		return 85
	}
	return i.HighWatermarkPercent
}

// ImageGCLowWatermarkPercent returns the stop percentage, defaulting to 70.
// A value at or above the high watermark is clamped to it by the collector.
func (i *ImageGCConfig) ImageGCLowWatermarkPercent() float64 {
	if i.LowWatermarkPercent <= 0 || i.LowWatermarkPercent > 100 {
		return 70
	}
	return i.LowWatermarkPercent
}

// ImageGCMinFreeBytes returns the absolute floor in bytes, defaulting to
// 20 GiB.
func (i *ImageGCConfig) ImageGCMinFreeBytes() uint64 {
	gb := i.MinFreeGB
	if gb == 0 {
		gb = 20
	}
	return gb * 1024 * 1024 * 1024
}

// ImageGCTargetFreeBytes returns the absolute target in bytes, defaulting to
// twice the floor.
func (i *ImageGCConfig) ImageGCTargetFreeBytes() uint64 {
	if i.TargetFreeGB > 0 {
		return i.TargetFreeGB * 1024 * 1024 * 1024
	}
	return 2 * i.ImageGCMinFreeBytes()
}

// ImageGCMaxAge returns the optional age backstop. Zero (the default) and
// any negative value mean disabled.
func (i *ImageGCConfig) ImageGCMaxAge() time.Duration {
	if i.MaxAge < 0 {
		return 0
	}
	return i.MaxAge
}

// PinnedRunnerImages returns every image ref this node is configured to run
// runners from: the [runner] default, every per-repo [runner.images.<repo>]
// entry, and each provider's per-OS defaults.
//
// The image GC treats these as never-evictable. Dropping one guarantees a
// re-pull on the very next job of that shape, which is exactly the network
// thrash pressure-triggered GC exists to avoid; they are also the images
// most likely to look "stale" to an LRU sweep on a node that has been busy
// with third-party container: images.
//
// Refs are returned in a stable order with duplicates removed.
func (c *Config) PinnedRunnerImages() []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(refs ...string) {
		for _, r := range refs {
			if r == "" {
				continue
			}
			if _, ok := seen[r]; ok {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}

	add(c.Runner.DefaultImage)
	for _, byOS := range c.Runner.Images {
		for _, ref := range byOS {
			add(ref)
		}
	}
	for _, g := range c.GitHubTargets() {
		add(g.DefaultImageFor("linux"), g.DefaultImageFor("windows"))
	}
	add(c.Forgejo.DefaultImageFor("linux"), c.Forgejo.DefaultImageFor("windows"), c.Forgejo.JobImage)
	add(c.Gitea.DefaultImageFor("linux"), c.Gitea.DefaultImageFor("windows"), c.Gitea.JobImage)
	add(c.GitLab.DefaultImageFor("linux"), c.GitLab.DefaultImageFor("windows"))

	// A per-repo images map iterates in random order; sort so the log
	// line and any test assertion are stable.
	sort.Strings(out)
	return out
}

// ModuleProxyConfig configures the Go module caching proxy.
// When enabled, ephemerd runs a local GOPROXY on the bridge gateway that
// caches module downloads. Containers receive GOPROXY env var automatically.
type ModuleProxyConfig struct {
	Enabled  bool   `toml:"enabled"`  // enable Go module caching proxy
	Port     int    `toml:"port"`     // listen port on bridge gateway (default 8082)
	Upstream string `toml:"upstream"` // upstream proxy URL (default "https://proxy.golang.org")
	Cleanup  *bool  `toml:"cleanup"`  // wipe cache on shutdown (default true)
}

// CleanupEnabled reports whether the Go module cache is wiped on shutdown.
// Defaults to true, preserving the historical behavior.
//
// Pointer-typed so "unset" is distinguishable from an explicit false. The
// previous plain bool could not express that: the call site coerced any
// false value back to true, which silently ignored `cleanup = false`.
func (m *ModuleProxyConfig) CleanupEnabled() bool {
	if m.Cleanup == nil {
		return true
	}
	return *m.Cleanup
}

// CargoProxyConfig configures the Cargo/crates caching proxy.
//
// When enabled, ephemerd runs a pull-through cache for the crates.io sparse
// index, .crate tarballs, and rustup toolchain artifacts on the bridge
// gateway. Job containers are pointed at it automatically: rustup via
// RUSTUP_DIST_SERVER, and Cargo via a generated .cargo/config.toml that is
// bind-mounted read-only at the container's filesystem root (Cargo ignores
// CARGO_SOURCE_* environment variables, so a file is the only mechanism).
type CargoProxyConfig struct {
	// Enabled turns the proxy on. Default false.
	Enabled bool `toml:"enabled"`
	// Port is the listen port on the bridge gateway. Default 8083.
	Port int `toml:"port"`
	// Upstream is the sparse registry index base URL.
	// Default "https://index.crates.io".
	Upstream string `toml:"upstream"`
	// RustupUpstream is the toolchain distribution server.
	// Default "https://static.rust-lang.org".
	RustupUpstream string `toml:"rustup_upstream"`
	// IndexTTL is how long a cached sparse-index entry is served before a
	// conditional revalidation. Default 10m. Crate tarballs ignore this —
	// they are immutable and cached permanently.
	IndexTTL time.Duration `toml:"index_ttl"`
	// Cleanup wipes the cache on shutdown. Default FALSE, unlike the Go
	// module proxy: the whole point of a pull-through cache is to survive
	// restarts, and wiping it on every shutdown is what made the module
	// proxy's cache worthless.
	Cleanup *bool `toml:"cleanup"`
}

// CleanupEnabled reports whether the Cargo cache is wiped on shutdown.
// Defaults to false — see the field comment.
func (c *CargoProxyConfig) CleanupEnabled() bool {
	if c.Cleanup == nil {
		return false
	}
	return *c.Cleanup
}

// VMConfig configures virtual machines for cross-OS job execution.
type VMConfig struct {
	// CrossPlatform enables macOS and Windows VM support. Default true.
	// Set to false for platforms like Gitea/Forgejo that only support
	// Linux runners — this skips macOS image pulls and Windows VM setup.
	CrossPlatform *bool       `toml:"cross_platform"`
	Linux         LinuxVMToml `toml:"linux"`
	MacOS         MacOSVMToml `toml:"macos"`
}

// CrossPlatformEnabled returns whether macOS/Windows VM support is enabled.
// Defaults to true when not set.
func (v *VMConfig) CrossPlatformEnabled() bool {
	if v.CrossPlatform == nil {
		return true
	}
	return *v.CrossPlatform
}

// LinuxVMToml configures the long-running Linux VM for Linux jobs
// on Windows (Hyper-V) and macOS (Virtualization.framework) hosts.
type LinuxVMToml struct {
	// Enabled controls whether this host serves Linux jobs through the VM.
	// Pointer so "not set" is distinguishable from an explicit choice: the
	// platform default is preserved when omitted — ON for darwin (macOS has
	// always booted the VM unconditionally), OFF for windows (the WSL VM has
	// always been opt-in). Set `enabled = false` on a darwin host to stop it
	// claiming Linux jobs — e.g. when a dedicated Linux box now serves the
	// same labels and dual-claiming would orphan a runner per job. (The VM
	// itself still boots on darwin for now; skipping the boot entirely is a
	// separate change.)
	Enabled    *bool  `toml:"enabled"`
	CPUs       uint   `toml:"cpus"`         // virtual CPUs (default: 2)
	MemoryMB   uint64 `toml:"memory_mb"`    // memory in MB (default: 2048)
	DiskSizeGB uint64 `toml:"disk_size_gb"` // sparse disk size in GB (default: 50)
}

// ResolvedEnabled returns whether the host should serve Linux jobs via the
// Linux VM, defaulting per platform when the key is not set: darwin true
// (historical always-on), everything else false (windows is opt-in; on a
// linux host the field is meaningless — jobs run natively).
func (l *LinuxVMToml) ResolvedEnabled() bool {
	if l.Enabled == nil {
		return goruntime.GOOS == "darwin"
	}
	return *l.Enabled
}

// MacOSVMToml configures per-job macOS VMs. macOS jobs always run in a
// per-job VM on darwin hosts — there's no other way on Apple Silicon —
// so there's no enable/disable toggle. On non-darwin hosts this block
// is ignored.
type MacOSVMToml struct {
	// DiskImage is an optional path to a pre-installed macOS VM disk
	// (produced by `ephemerd vm setup-macos` or an operator-supplied
	// restore of an Apple IPSW). If empty, ephemerd downloads the latest
	// Apple-signed IPSW on first boot and installs stock macOS into
	// <data_dir>/vm/macos/base.img. Distinct from the OCI base image
	// overlaid per job — that's fetched from the job's image label.
	DiskImage     string `toml:"disk_image"`
	CPUs          uint   `toml:"cpus"`           // CPUs per VM (default: 4)
	MemoryMB      uint64 `toml:"memory_mb"`      // memory per VM in MB (default: 8192)
	MaxConcurrent int    `toml:"max_concurrent"` // max simultaneous macOS VMs (default: auto-detected from host CPUs)
}

type GitHubConfig struct {
	// Authentication: either a PAT or GitHub App
	Token          string `toml:"token"`
	AppID          int64  `toml:"app_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`

	// Which org/user and repos to register runners for
	Owner string   `toml:"owner"`
	Repos []string `toml:"repos"`

	// DispatchPolicy is an OPTIONAL defense-in-depth allowlist that gates which
	// incoming workflow_job webhook events are allowed to dispatch a runner.
	// It is IN ADDITION to (not a replacement for) GitHub's own "require
	// approval for outside collaborators" setting, which remains the primary
	// control against fork-PR abuse. Empty (the default) preserves today's
	// behavior: any tracked repo with a self-hosted label dispatches.
	DispatchPolicy DispatchPolicyConfig `toml:"dispatch_policy"`

	// Job discovery: polling interval (default "30s")
	PollInterval string `toml:"poll_interval"`

	// DefaultImage is the legacy single-image override (Linux only).
	// Kept for backward compatibility — prefer DefaultImageLinux /
	// DefaultImageWindows for new configs. When DefaultImageLinux is empty
	// and this is set, it's treated as the Linux default.
	// Linux fallback (when nothing is set): "ghcr.io/actions/actions-runner:latest".
	DefaultImage string `toml:"default_image"`

	// DefaultImageLinux is the provider-level default image for Linux jobs.
	// Per-repo entries in [runner.images.<repo>].linux win over this.
	DefaultImageLinux string `toml:"default_image_linux"`

	// DefaultImageWindows is the provider-level default image for Windows
	// jobs. Per-repo entries in [runner.images.<repo>].windows win over
	// this. Falls through to the runtime's host-matched servercore default
	// (pkg/runtime/image_windows.go) when unset.
	DefaultImageWindows string `toml:"default_image_windows"`
}

// DispatchPolicyConfig is an opt-in allowlist restricting which webhook jobs
// may dispatch a runner. All fields default to "no restriction"; setting any
// field narrows dispatch. It is a safety net layered under GitHub's outside-
// collaborator approval gate, not a substitute for it.
type DispatchPolicyConfig struct {
	// AllowedRepos, when non-empty, restricts dispatch to jobs whose repository
	// name is in this list. Empty means "all tracked repos" (current behavior).
	// Repo names are matched exactly against the webhook's repository name (the
	// same value used by [github].repos).
	AllowedRepos []string `toml:"allowed_repos"`

	// RequiredLabels, when non-empty, requires an incoming job to carry at
	// least one of these labels (beyond the mandatory "self-hosted" gate)
	// before it dispatches. Empty means "no extra label requirement". Use this
	// to pin dispatch to jobs that opt in with a distinctive label an outside
	// fork is unlikely to request (e.g. "ephemerd").
	RequiredLabels []string `toml:"required_labels"`
}

// IsZero reports whether the policy imposes no restrictions (the default).
func (d DispatchPolicyConfig) IsZero() bool {
	return len(d.AllowedRepos) == 0 && len(d.RequiredLabels) == 0
}

// DefaultImageFor returns the provider-level default image for the given OS.
// Resolution: per-OS field → legacy DefaultImage (Linux only) → empty.
// Empty means "no provider default — let the runtime pick its OS-native
// fallback (e.g. mcr.microsoft.com/windows/servercore:ltsc20XX on Windows)".
func (g *GitHubConfig) DefaultImageFor(os string) string {
	switch os {
	case "linux":
		if g.DefaultImageLinux != "" {
			return g.DefaultImageLinux
		}
		return g.DefaultImage
	case "windows":
		return g.DefaultImageWindows
	}
	return ""
}

// ForgejoConfig configures the Forgejo Actions provider.
// Set instance_url and token to enable Forgejo instead of GitHub.
// Uses forgejo-runner binary with one-job --handle mode.
//
// Forgejo's runner daemon (DefaultImage) is Linux-only; setting
// DefaultImageWindows is allowed for completeness but no upstream Windows
// build of forgejo-runner exists today.
type ForgejoConfig struct {
	InstanceURL         string   `toml:"instance_url"`          // Forgejo instance URL (e.g., "https://codeberg.org")
	Token               string   `toml:"token"`                 // runner registration token from Forgejo admin
	Owner               string   `toml:"owner"`                 // org or user (empty = instance-level runner)
	Repos               []string `toml:"repos"`                 // limit to specific repos (empty = all)
	Labels              []string `toml:"labels"`                // runner labels (default: ["ubuntu-latest:docker://<job_image>"])
	DefaultImage        string   `toml:"default_image"`         // runner daemon image (default: "data.forgejo.org/forgejo/runner:12")
	DefaultImageLinux   string   `toml:"default_image_linux"`   // per-OS Linux runner image (wins over default_image when set)
	DefaultImageWindows string   `toml:"default_image_windows"` // per-OS Windows runner image
	JobImage            string   `toml:"job_image"`             // job execution image (default: "gitea/runner-images:ubuntu-24.04")
}

// DefaultImageFor returns the provider-level default for the given OS.
func (f *ForgejoConfig) DefaultImageFor(os string) string {
	switch os {
	case "linux":
		if f.DefaultImageLinux != "" {
			return f.DefaultImageLinux
		}
		return f.DefaultImage
	case "windows":
		return f.DefaultImageWindows
	}
	return ""
}

// GiteaConfig configures the Gitea Actions provider.
// Set instance_url and token to enable Gitea instead of GitHub.
// Uses act_runner binary with --ephemeral mode.
type GiteaConfig struct {
	InstanceURL         string   `toml:"instance_url"`          // Gitea instance URL (e.g., "https://gitea.example.com")
	Token               string   `toml:"token"`                 // runner registration token from Gitea admin
	Owner               string   `toml:"owner"`                 // org or user (empty = instance-level runner)
	Repos               []string `toml:"repos"`                 // limit to specific repos (empty = all)
	Labels              []string `toml:"labels"`                // runner labels (default: ["ubuntu-latest:docker://<job_image>"])
	DefaultImage        string   `toml:"default_image"`         // runner daemon image (default: "docker.io/gitea/act_runner:latest")
	DefaultImageLinux   string   `toml:"default_image_linux"`   // per-OS Linux runner image (wins over default_image when set)
	DefaultImageWindows string   `toml:"default_image_windows"` // per-OS Windows runner image
	JobImage            string   `toml:"job_image"`             // job execution image (default: "gitea/runner-images:ubuntu-24.04")
}

// DefaultImageFor returns the provider-level default for the given OS.
func (g *GiteaConfig) DefaultImageFor(os string) string {
	switch os {
	case "linux":
		if g.DefaultImageLinux != "" {
			return g.DefaultImageLinux
		}
		return g.DefaultImage
	case "windows":
		return g.DefaultImageWindows
	}
	return ""
}

// GitLabConfig configures the GitLab CI provider.
// Set instance_url and token to enable GitLab instead of GitHub.
type GitLabConfig struct {
	InstanceURL         string   `toml:"instance_url"`          // GitLab instance URL (e.g., "https://gitlab.com")
	Token               string   `toml:"token"`                 // runner authentication token (glrt-xxx for GitLab 16+)
	Tags                []string `toml:"tags"`                  // runner tags for job matching
	DefaultImage        string   `toml:"default_image"`         // runner image (default: "ghcr.io/ephpm/runner-gitlab:latest")
	DefaultImageLinux   string   `toml:"default_image_linux"`   // per-OS Linux runner image (wins over default_image when set)
	DefaultImageWindows string   `toml:"default_image_windows"` // per-OS Windows runner image
}

// DefaultImageFor returns the provider-level default for the given OS.
func (g *GitLabConfig) DefaultImageFor(os string) string {
	switch os {
	case "linux":
		if g.DefaultImageLinux != "" {
			return g.DefaultImageLinux
		}
		return g.DefaultImage
	case "windows":
		return g.DefaultImageWindows
	}
	return ""
}

// WoodpeckerConfig configures the Woodpecker CI provider.
// Set server_url and agent_secret to enable Woodpecker instead of GitHub.
// Woodpecker requires a forge backend (Gitea/Forgejo) for repo management;
// ephemerd manages the agent lifecycle, not the server.
type WoodpeckerConfig struct {
	ServerURL   string `toml:"server_url"`   // Woodpecker server gRPC URL (e.g., "woodpecker.example.com:9000")
	AgentSecret string `toml:"agent_secret"` // shared secret for agent authentication
}

type RunnerConfig struct {
	MaxConcurrent int      `toml:"max_concurrent"`
	ExtraLabels   []string `toml:"extra_labels"`
	DefaultImage  string   `toml:"default_image"`

	// Images maps repo → OS → image. TOML shape:
	//
	//   [runner.images.ephemerd]
	//   linux   = "ephpm/ephemerd:runner-ci-linux-amd64"
	//   windows = "ephpm/ephemerd:runner-ci-windows"
	//
	// A repo can specify just one OS — the others fall through to the
	// provider per-OS default and then the runtime fallback.
	Images map[string]map[string]string `toml:"images"`

	JobTimeout      string            `toml:"job_timeout"`
	ShutdownTimeout string            `toml:"shutdown_timeout"`
	Windows         WindowsRunnerToml `toml:"windows"`

	// ClaimRetry controls the in-memory retry queue for jobs whose
	// initial claim / provision attempt fails with a transient error
	// (rate limit, 5xx, network). GitHub does not re-deliver
	// workflow_job webhooks, so without this queue any queued job that
	// hit an API blip at claim time would be lost until human
	// intervention.
	ClaimRetry ClaimRetryToml `toml:"claim_retry"`

	// OrphanSweep controls teardown of runners that were dispatched for
	// a job but never observed picking one up. GitHub assigns JIT
	// runners to ANY queued job with matching labels, so runner
	// lifecycle is keyed to the observed assignment; a runner whose
	// intended job went elsewhere and that never got a job of its own is
	// destroyed after the grace window.
	OrphanSweep OrphanSweepToml `toml:"orphan_sweep"`
}

// OrphanSweepToml configures the scheduler's orphaned-runner sweep.
//
// Enabled defaults to true when the [runner.orphan_sweep] table is
// omitted. The sweep only acts in webhook mode (in polling mode there
// are no in_progress events, so ephemerd cannot tell an orphaned runner
// from a busy one) and only for providers that report runner
// assignments (GitHub).
type OrphanSweepToml struct {
	// Enabled toggles the sweep. Nil = default true; operators disable
	// by explicitly setting enabled = false.
	Enabled *bool `toml:"enabled"`

	// Grace is how long a dispatched runner may sit without being
	// assigned a job before it is destroyed and deregistered. Accepts
	// Go duration strings ("10m", "1h"). Default 10m.
	Grace string `toml:"grace"`
}

// ClaimRetryToml configures the scheduler's claim-retry queue.
//
// Enabled defaults to true when the [runner.claim_retry] table is
// omitted (see RunnerConfig.ClaimRetryEnabled). Losing queued jobs to
// transient errors is almost never what an operator wants; set
// enabled = false to restore the pre-existing "log and drop" behavior.
type ClaimRetryToml struct {
	// Enabled toggles the retry queue. Nil = default true; operators
	// disable by explicitly setting enabled = false.
	Enabled *bool `toml:"enabled"`

	// MaxAge is the total time budget from first failure to giving up.
	// Accepts Go duration strings ("90m", "1h30m"). Default 90m.
	MaxAge string `toml:"max_age"`

	// Schedule is the ordered backoff ladder. Each entry is the base
	// delay for one attempt; jitter is applied on top. Default is
	// {30s, 1m, 2m, 5m, 10m}. Use a shorter schedule for shorter
	// MaxAge, or a longer one to let jobs marinate through extended
	// outages.
	Schedule []string `toml:"schedule"`

	// Jitter is the +/- fraction applied to each delay (0.0-1.0).
	// Default 0.2 (+/-20%). Set 0 to disable jitter (useful in tests,
	// rarely useful in production).
	Jitter *float64 `toml:"jitter"`
}

// WindowsRunnerToml configures resource limits for Hyper-V isolated Windows
// runner containers. Without limits Hyper-V containers default to ~1 GB RAM,
// which is too small for MSVC + parallel cl.exe builds.
type WindowsRunnerToml struct {
	MemoryMB uint64 `toml:"memory_mb"` // memory in MB (default: 4096)
	CPUs     uint64 `toml:"cpus"`      // virtual CPUs (default: 2)
}

// MemoryBytes returns the memory limit in bytes, applying the default if unset.
func (w WindowsRunnerToml) MemoryBytes() uint64 {
	mb := w.MemoryMB
	if mb == 0 {
		mb = 4096
	}
	return mb * 1024 * 1024
}

// CPUCount returns the CPU count, applying the default if unset.
func (w WindowsRunnerToml) CPUCount() uint64 {
	if w.CPUs == 0 {
		return 2
	}
	return w.CPUs
}

// ImageForRepoOS returns the per-repo, per-OS image override, or empty if
// no override is configured for that combination. Caller falls back to the
// provider default and then the runtime default.
func (r *RunnerConfig) ImageForRepoOS(repo, os string) string {
	if r == nil {
		return ""
	}
	if perOS, ok := r.Images[repo]; ok {
		if img, ok := perOS[os]; ok {
			return img
		}
	}
	return ""
}

// ImageForRepo is the legacy helper kept for callers that haven't migrated.
// Prefer ImageForRepoOS. Returns the Linux image override (if set), then
// DefaultImage. Empty when neither is set.
func (r *RunnerConfig) ImageForRepo(repo string) string {
	if img := r.ImageForRepoOS(repo, "linux"); img != "" {
		return img
	}
	return r.DefaultImage
}

// ClaimRetryEnabled reports whether the claim retry queue should run.
// Defaults to true (retries on) when the table is omitted or Enabled is
// nil; operators opt out with `enabled = false`.
func (r *RunnerConfig) ClaimRetryEnabled() bool {
	if r == nil || r.ClaimRetry.Enabled == nil {
		return true
	}
	return *r.ClaimRetry.Enabled
}

// ClaimRetryMaxAge returns the retry queue give-up threshold. Defaults
// to 90 minutes when unset or unparseable.
func (r *RunnerConfig) ClaimRetryMaxAge() time.Duration {
	if r == nil || r.ClaimRetry.MaxAge == "" {
		return 90 * time.Minute
	}
	if d, err := time.ParseDuration(r.ClaimRetry.MaxAge); err == nil && d > 0 {
		return d
	}
	return 90 * time.Minute
}

// ClaimRetrySchedule returns the retry backoff ladder. Defaults to
// {30s, 1m, 2m, 5m, 10m} when unset. Unparseable entries are dropped;
// if all entries fail to parse, the default is returned.
func (r *RunnerConfig) ClaimRetrySchedule() []time.Duration {
	def := []time.Duration{
		30 * time.Second,
		1 * time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
	}
	if r == nil || len(r.ClaimRetry.Schedule) == 0 {
		return def
	}
	out := make([]time.Duration, 0, len(r.ClaimRetry.Schedule))
	for _, s := range r.ClaimRetry.Schedule {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// OrphanSweepEnabled reports whether the orphaned-runner sweep should
// run. Defaults to true when the table is omitted or Enabled is nil;
// operators opt out with `enabled = false`.
func (r *RunnerConfig) OrphanSweepEnabled() bool {
	if r == nil || r.OrphanSweep.Enabled == nil {
		return true
	}
	return *r.OrphanSweep.Enabled
}

// OrphanSweepGrace returns how long a dispatched runner may remain
// unassigned before the sweep destroys it. Defaults to 10 minutes when
// unset or unparseable.
func (r *RunnerConfig) OrphanSweepGrace() time.Duration {
	if r == nil || r.OrphanSweep.Grace == "" {
		return 10 * time.Minute
	}
	if d, err := time.ParseDuration(r.OrphanSweep.Grace); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// ClaimRetryJitter returns the retry backoff jitter fraction. Defaults
// to 0.2 (+/-20%). Values outside [0,1] are clamped to the default.
func (r *RunnerConfig) ClaimRetryJitter() float64 {
	if r == nil || r.ClaimRetry.Jitter == nil {
		return 0.2
	}
	j := *r.ClaimRetry.Jitter
	if j < 0 || j > 1 {
		return 0.2
	}
	return j
}

// ParsedPollInterval returns the poll interval as a time.Duration.
func (g *GitHubConfig) ParsedPollInterval() time.Duration {
	if g.PollInterval == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(g.PollInterval)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// ParsedJobTimeout returns the job timeout as a time.Duration.
func (r *RunnerConfig) ParsedJobTimeout() time.Duration {
	d, err := time.ParseDuration(r.JobTimeout)
	if err != nil {
		return 2 * time.Hour
	}
	return d
}

// ParsedShutdownTimeout returns the shutdown timeout as a time.Duration.
func (r *RunnerConfig) ParsedShutdownTimeout() time.Duration {
	d, err := time.ParseDuration(r.ShutdownTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

type LogConfig struct {
	Level        string `toml:"level"`
	Format       string `toml:"format"`        // "text" or "json"
	LogRetention string `toml:"log_retention"` // max age for job log files (e.g. "7d", "24h"); default "7d"

	// Writer overrides the log output destination. When nil, logs go to
	// stderr. Set by the Windows Service handler to route logs to the
	// Windows Event Log.
	Writer io.Writer `toml:"-"`
}

// LogRetentionDuration returns the parsed log retention duration.
// Supports Go duration strings (e.g. "168h") and a "d" suffix for days (e.g. "7d").
// Returns 7 days if the value is empty or invalid.
func (lc LogConfig) LogRetentionDuration() time.Duration {
	s := lc.LogRetention
	if s == "" {
		return 7 * 24 * time.Hour
	}
	// Support "Nd" shorthand for days.
	if len(s) > 1 && s[len(s)-1] == 'd' {
		if d, err := time.ParseDuration(s[:len(s)-1] + "h"); err == nil {
			return d * 24
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return 7 * 24 * time.Hour
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		Runner: RunnerConfig{
			MaxConcurrent:   4,
			JobTimeout:      "2h",
			ShutdownTimeout: "5m",
		},
		Webhook: WebhookConfig{
			Port:   8080,
			Tunnel: "none",
		},
		Metrics: MetricsConfig{
			Port: 9090,
			Path: "/metrics",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				slog.Warn("config file not found, using defaults", "path", path)
				return cfg, nil
			}
			return nil, fmt.Errorf("reading config: %w", err)
		}
		if err := toml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// EnsureDispatchToken guarantees c.Dispatch.Token is set, minting a fresh token
// and persisting it into the config file at path when it is currently empty.
// The generated token is appended as a "[dispatch]" block so the operator's
// existing content (and comments) are preserved; the persisted file then rides
// into the Linux VM through the normal config-delivery channel, giving the host
// client and the in-VM dispatch server the same shared secret.
//
// It is a no-op when a token is already set (whether from config or a prior
// mint). Only the host that boots the VM needs to call this; the in-VM daemon
// simply reads the token that was delivered inside config.toml.
//
// gen supplies the token bytes (crypto/rand-backed in production); injecting it
// keeps this package free of a scheduler import and makes the persistence path
// testable.
func (c *Config) EnsureDispatchToken(path string, gen func() (string, error)) error {
	if c.Dispatch.Token != "" {
		return nil
	}
	tok, err := gen()
	if err != nil {
		return fmt.Errorf("generating dispatch token: %w", err)
	}
	if tok == "" {
		return fmt.Errorf("dispatch token generator returned empty token")
	}
	c.Dispatch.Token = tok

	// Persist so the value survives restarts and reaches the VM. A missing
	// path (config loaded from defaults only) means there is nowhere durable to
	// write; keep the in-memory token so the current process still works, but
	// surface the situation to the caller's logs via the returned nil (no
	// error — an ephemeral token is still better than none).
	if path == "" {
		return nil
	}

	block := fmt.Sprintf("\n[dispatch]\n# Auto-generated shared token for the host<->VM dispatch gRPC channel.\n# Delete this block to have ephemerd mint a new one on next start.\ntoken = %q\n", tok)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening config to persist dispatch token: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing config after persisting dispatch token: %w", cerr)
		}
	}()
	if _, err = f.WriteString(block); err != nil {
		return fmt.Errorf("writing dispatch token to config: %w", err)
	}
	return nil
}

func (c *Config) validate() error {
	// Apply GITHUB_TOKEN env fallback before checking which providers are configured.
	// This ensures Providers() picks up the env-based token.
	if c.GitHub.Token == "" && os.Getenv("GITHUB_TOKEN") != "" {
		c.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}

	// Validate all configured providers (not just the first one).
	for _, p := range c.Providers() {
		switch p {
		case "github":
			if err := c.validateGitHub(); err != nil {
				return err
			}
		case "forgejo":
			if c.Forgejo.Token == "" {
				return fmt.Errorf("forgejo.token is required")
			}
		case "gitea":
			if c.Gitea.Token == "" {
				return fmt.Errorf("gitea.token is required")
			}
		case "gitlab":
			if c.GitLab.Token == "" {
				return fmt.Errorf("gitlab.token is required")
			}
		case "woodpecker":
			if c.Woodpecker.AgentSecret == "" {
				return fmt.Errorf("woodpecker.agent_secret is required")
			}
		}
	}

	if err := c.Network.validate(); err != nil {
		return err
	}

	// Webhook secret handling depends on who owns the tunnel:
	//   - "external": ingress and the GitHub webhook are configured elsewhere,
	//     so the secret must match that external config — we cannot invent one.
	//   - managed tunnels ("localtunnel"/"ngrok"): ephemerd registers the
	//     webhook itself, so a random secret is fine when none is provided.
	//   - "none": polling (or external ingress with an explicit secret); nothing
	//     to generate.
	// Pool mode shares one webhook (and its HMAC secret) across many nodes.
	// An auto-generated secret would differ per node and break signature
	// verification on every member except whichever registered the hook.
	if c.Webhook.Pool && c.Webhook.Secret == "" {
		return fmt.Errorf(`webhook.secret is required when webhook.pool = true (every pool member must share the same secret)`)
	}

	switch c.Webhook.Tunnel {
	case "external":
		if c.Webhook.Secret == "" {
			return fmt.Errorf(`webhook.secret is required when webhook.tunnel = "external" (it must match the secret configured on the external GitHub webhook)`)
		}
		// When external_url is set, ephemerd auto-registers each tracked repo's
		// webhook. That requires a secret (already validated above). Trim a
		// trailing slash so <external_url>/webhook/<provider> is well-formed.
		if c.Webhook.ExternalURL != "" {
			c.Webhook.ExternalURL = strings.TrimRight(c.Webhook.ExternalURL, "/")
		}
	case "none":
		// external_url only makes sense with tunnel="external": for managed
		// tunnels ephemerd derives the URL from the tunnel provider, and for
		// polling there is no receiver to point a webhook at.
		if c.Webhook.ExternalURL != "" {
			return fmt.Errorf(`webhook.external_url is only valid when webhook.tunnel = "external"`)
		}
		// Nothing to generate; secret, if set, enables an externally-fronted
		// webhook receiver, otherwise ephemerd polls.
	case "cloudflared":
		if c.Webhook.CloudflaredToken == "" {
			c.Webhook.CloudflaredToken = os.Getenv("CLOUDFLARE_TUNNEL_TOKEN")
		}
		if c.Webhook.CloudflaredToken == "" {
			return fmt.Errorf(`webhook.cloudflared_token is required when webhook.tunnel = "cloudflared" (or set CLOUDFLARE_TUNNEL_TOKEN env)`)
		}
		if c.Webhook.CloudflaredHostname == "" {
			return fmt.Errorf(`webhook.cloudflared_hostname is required when webhook.tunnel = "cloudflared" (the public FQDN cloudflared exposes)`)
		}
		if c.Webhook.Secret == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return fmt.Errorf("generating webhook secret: %w", err)
			}
			c.Webhook.Secret = hex.EncodeToString(b)
		}
	default:
		// Managed tunnels ("localtunnel"/"ngrok") derive the webhook URL from
		// the tunnel provider's own public URL; external_url has no meaning here.
		if c.Webhook.ExternalURL != "" {
			return fmt.Errorf(`webhook.external_url is only valid when webhook.tunnel = "external"`)
		}
		if c.Webhook.Secret == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return fmt.Errorf("generating webhook secret: %w", err)
			}
			c.Webhook.Secret = hex.EncodeToString(b)
		}
	}

	// Any webhook mode (managed tunnel, or external/none with a secret) needs a
	// listen port; default to 8080 when unset.
	if c.Webhook.Port == 0 && (c.Webhook.Tunnel != "none" || c.Webhook.Secret != "") {
		c.Webhook.Port = 8080
	}

	return nil
}

func (c *Config) validateGitHub() error {
	// Fall back to GITHUB_TOKEN env var for the primary [github] target.
	if c.GitHub.Token == "" {
		c.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}
	primary := c.GitHub.Owner != "" || c.GitHub.Token != "" || c.GitHub.AppID != 0
	if primary {
		if err := validateGitHubTarget(&c.GitHub, "github"); err != nil {
			return err
		}
	}
	// Each [[github_extra]] target is self-contained (its own owner + auth).
	for i := range c.GitHubExtra {
		if err := validateGitHubTarget(&c.GitHubExtra[i], fmt.Sprintf("github_extra[%d]", i)); err != nil {
			return err
		}
	}
	if !primary && len(c.GitHubExtra) == 0 {
		return fmt.Errorf("github.token or github.app_id is required (or set GITHUB_TOKEN env var)")
	}
	// repos is optional — if empty, ephemerd registers org-level runners
	return nil
}

func validateGitHubTarget(g *GitHubConfig, label string) error {
	if g.Token == "" && g.AppID == 0 {
		return fmt.Errorf("%s.token or %s.app_id is required", label, label)
	}
	if g.AppID != 0 {
		if g.InstallationID == 0 {
			return fmt.Errorf("%s.installation_id is required when using app_id", label)
		}
		if g.PrivateKeyPath == "" {
			return fmt.Errorf("%s.private_key_path is required when using app_id", label)
		}
	}
	if g.Owner == "" {
		return fmt.Errorf("%s.owner is required", label)
	}
	return nil
}

func (c *Config) Logger() *slog.Logger {
	var level slog.Level
	switch c.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	// Use the configured writer, or fall back to stderr.
	// On Windows terminals, wrap stderr to fix \n → \r\n line endings.
	// When Writer is set (e.g. Event Log), use it directly — it handles
	// its own formatting.
	var w io.Writer
	if c.Log.Writer != nil {
		w = c.Log.Writer
	} else if goruntime.GOOS == "windows" {
		w = &crlfWriter{w: os.Stderr}
	} else {
		w = os.Stderr
	}

	var handler slog.Handler
	if c.Log.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	return slog.New(handler)
}

// crlfWriter wraps a writer to replace bare \n with \r\n for Windows terminals.
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	replaced := bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	_, err := c.w.Write(replaced)
	return len(p), err
}
