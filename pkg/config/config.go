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
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	GitHub      GitHubConfig      `toml:"github"`
	Forgejo     ForgejoConfig     `toml:"forgejo"`
	Gitea       GiteaConfig       `toml:"gitea"`
	GitLab      GitLabConfig      `toml:"gitlab"`
	Woodpecker  WoodpeckerConfig  `toml:"woodpecker"`
	Webhook     WebhookConfig     `toml:"webhook"`
	Containerd  ContainerdConfig  `toml:"containerd"`
	Network     NetworkConfig     `toml:"network"`
	VM          VMConfig          `toml:"vm"`
	Dind        DindConfig        `toml:"dind"`
	ModuleProxy ModuleProxyConfig `toml:"module_proxy"`
	Runtime     RuntimeConfig     `toml:"runtime"`
	Runner      RunnerConfig      `toml:"runner"`
	Metrics     MetricsConfig     `toml:"metrics"`
	Log         LogConfig         `toml:"log"`
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
	if c.GitHub.Owner != "" || c.GitHub.Token != "" || c.GitHub.AppID != 0 {
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
// Set tunnel = "localtunnel" or "ngrok" for instant webhook delivery.
type WebhookConfig struct {
	Secret           string `toml:"secret"`             // webhook HMAC secret (auto-generated if empty)
	Port             int    `toml:"port"`               // listen port for health endpoint (default 8080)
	TLSCert          string `toml:"tls_cert"`           // TLS certificate path (direct TLS, no tunnel)
	TLSKey           string `toml:"tls_key"`            // TLS private key path
	Tunnel           string `toml:"tunnel"`             // "none" (default, polling), "localtunnel", or "ngrok"
	TunnelURL        string `toml:"tunnel_url"`         // localtunnel: self-hosted server URL
	NgrokAuthtoken   string `toml:"ngrok_authtoken"`    // ngrok auth token (or use NGROK_AUTHTOKEN env)
	TunnelMaxRetries int    `toml:"tunnel_max_retries"` // max consecutive reconnect failures before falling back to polling (default 5)
}

// NetworkConfig configures container networking.
type NetworkConfig struct {
	Subnet string `toml:"subnet"` // container subnet (auto-selected if empty)
	MTU    int    `toml:"mtu"`    // bridge MTU (auto-detected from host if 0)
}

type ContainerdConfig struct {
	// Reserved for future containerd-specific settings (e.g. snapshotter overrides)
}

// RuntimeConfig configures behavior of the per-job container runtime —
// things that apply to the OCI spec rather than to a specific subsystem
// like dind or networking.
type RuntimeConfig struct {
	Rlimits RuntimeRlimits `toml:"rlimits"`
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

	// CacheMaxAge is the eviction threshold for cached image records:
	// any record whose ephemerd.io/last-accessed label (or UpdatedAt as
	// fallback) is older than this gets removed on the next prune pass.
	// Containerd's content GC then reclaims the unreferenced blobs.
	// Set to 0 to disable eviction (only empty-namespace cleanup runs).
	// Default 168h (7 days).
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
// containers are allowed, applying the platform-aware default when the
// operator hasn't set the key explicitly.
//
// Default policy:
//   - Windows / macOS host → true. The dind containerd backing store
//     runs inside a VM that ephemerd manages (WSL2/Hyper-V on Windows,
//     Vz on macOS), so the worst-case escape stays inside that VM.
//   - Linux host → false. ephemerd runs directly on the host with no
//     VM fence, so a privileged escape is bare-metal-host compromise.
//     Operators that trust their workloads (e.g. internal CI for KIND
//     tests) can opt in via `allow_privileged = true`.
func (d *DindConfig) ResolvedAllowPrivileged() bool {
	if d.AllowPrivileged != nil {
		return *d.AllowPrivileged
	}
	return goruntime.GOOS != "linux"
}

// DindCachePruneInterval returns the prune interval with the default
// applied when unset (or set to 0).
func (d *DindConfig) DindCachePruneInterval() time.Duration {
	if d.CachePruneInterval == 0 {
		return 24 * time.Hour
	}
	return d.CachePruneInterval
}

// DindCacheMaxAge returns the eviction threshold with the default applied
// when unset (or set to 0).
func (d *DindConfig) DindCacheMaxAge() time.Duration {
	if d.CacheMaxAge == 0 {
		return 7 * 24 * time.Hour
	}
	return d.CacheMaxAge
}

// ModuleProxyConfig configures the Go module caching proxy.
// When enabled, ephemerd runs a local GOPROXY on the bridge gateway that
// caches module downloads. Containers receive GOPROXY env var automatically.
type ModuleProxyConfig struct {
	Enabled  bool   `toml:"enabled"`  // enable Go module caching proxy
	Port     int    `toml:"port"`     // listen port on bridge gateway (default 8082)
	Upstream string `toml:"upstream"` // upstream proxy URL (default "https://proxy.golang.org")
	Cleanup  bool   `toml:"cleanup"`  // wipe cache on shutdown (default true)
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
	Enabled    bool   `toml:"enabled"`      // enable Linux VM for cross-OS Linux jobs
	CPUs       uint   `toml:"cpus"`         // virtual CPUs (default: 2)
	MemoryMB   uint64 `toml:"memory_mb"`    // memory in MB (default: 2048)
	DiskSizeGB uint64 `toml:"disk_size_gb"` // sparse disk size in GB (default: 50)
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
	MacOS           MacOSRunnerConfig `toml:"macos"`
}

// MacOSRunnerConfig controls macOS job routing. It lives under [runner]
// (not [vm.macos]) because native jobs don't involve VMs.
//
// TOML shape:
//
//	[runner.macos]
//	mode = "vm"         # default mode: "vm" or "native"
//	max_native = 4      # max concurrent native jobs
//	# user = "ciuser"   # optional: existing user for native runners.
//	#                   # Default (unset): an ephemeral hidden user is
//	#                   # created per job and deleted on cleanup.
//
//	[runner.macos.repos]
//	"ephpm/*"           = "native"  # all repos in org
//	"ephpm/secret-repo" = "vm"     # except this one (exact wins over wildcard)
//	"someuser/ephemerd" = "vm"     # fork stays on VM
type MacOSRunnerConfig struct {
	Mode      string            `toml:"mode"`       // "vm" (default) or "native"
	MaxNative int               `toml:"max_native"` // max concurrent native jobs (default 4)
	User      string            `toml:"user"`       // existing user for native runners (empty = ephemeral per-job user, recommended)
	Repos     map[string]string `toml:"repos"`      // "org/repo" -> "vm" or "native"
}

// ModeForRepo returns "native" or "vm" for the given repo. Resolution order:
//
//  1. Exact match on "org/repo"
//  2. Wildcard match on "org/*"
//  3. Short-name fallback: if repo has no "/", match any "org/<repo>" key
//  4. Top-level mode
//  5. Default: "vm"
//
// The short-name fallback exists because some providers (GitHub polling)
// currently emit event.Repo as just the repo name without the org prefix.
// Config keys should always use "org/repo" format for disambiguation.
func (m *MacOSRunnerConfig) ModeForRepo(repo string) string {
	if m != nil && len(m.Repos) > 0 {
		// 1. Exact match
		if mode, ok := m.Repos[repo]; ok && isValidMode(mode) {
			return mode
		}

		// 2. Wildcard: "org/*" matches any repo under that org
		if slash := strings.IndexByte(repo, '/'); slash > 0 {
			wildcard := repo[:slash] + "/*"
			if mode, ok := m.Repos[wildcard]; ok && isValidMode(mode) {
				return mode
			}
		}

		// 3. Short-name fallback: repo="ephemerd" matches key "ephpm/ephemerd"
		if !strings.Contains(repo, "/") {
			suffix := "/" + repo
			for key, mode := range m.Repos {
				if strings.HasSuffix(key, suffix) && !strings.HasSuffix(key, "/*") && isValidMode(mode) {
					return mode
				}
			}
		}
	}
	if m != nil && isValidMode(m.Mode) {
		return m.Mode
	}
	return "vm"
}

func isValidMode(mode string) bool {
	return mode == "native" || mode == "vm"
}

// ResolvedMaxNative returns the max concurrent native macOS jobs,
// defaulting to 4 if unset or non-positive.
func (m *MacOSRunnerConfig) ResolvedMaxNative() int {
	if m == nil || m.MaxNative <= 0 {
		return 4
	}
	return m.MaxNative
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

	// Generate a random webhook secret if not explicitly set and tunnel is active
	if c.Webhook.Secret == "" && c.Webhook.Tunnel != "none" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating webhook secret: %w", err)
		}
		c.Webhook.Secret = hex.EncodeToString(b)
	}

	return nil
}

func (c *Config) validateGitHub() error {
	// Fall back to GITHUB_TOKEN env var if no token is configured
	if c.GitHub.Token == "" {
		c.GitHub.Token = os.Getenv("GITHUB_TOKEN")
	}
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 {
		return fmt.Errorf("github.token or github.app_id is required (or set GITHUB_TOKEN env var)")
	}
	if c.GitHub.AppID != 0 {
		if c.GitHub.InstallationID == 0 {
			return fmt.Errorf("github.installation_id is required when using github.app_id")
		}
		if c.GitHub.PrivateKeyPath == "" {
			return fmt.Errorf("github.private_key_path is required when using github.app_id")
		}
	}
	if c.GitHub.Owner == "" {
		return fmt.Errorf("github.owner is required")
	}
	// repos is optional — if empty, ephemerd registers org-level runners
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
