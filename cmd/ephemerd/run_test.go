package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ephpm/ephemerd/pkg/workflow"
)

// configDirGuard saves and restores the package-level configDir global so
// each test case can point at its own tempdir without leaking state.
func configDirGuard(t *testing.T) func() {
	t.Helper()
	prev := configDir
	return func() { configDir = prev }
}

// writeConfig drops a minimal valid config.toml into dir and returns the
// path. Tests pass it via the configDir global, the same way the live
// CLI command does.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing config.toml: %v", err)
	}
}

func TestResolveRunImage_FlagWins(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	if got := resolveRunImage("ghcr.io/explicit:v1", workflow.PlatformLinux, loadRunConfig()); got != "ghcr.io/explicit:v1" {
		t.Errorf("flag-wins: got %q, want explicit override", got)
	}
}

func TestResolveRunImage_ConfigWins_Linux(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	got := resolveRunImage("", workflow.PlatformLinux, loadRunConfig())
	if got != "ghcr.io/from-config:linux" {
		t.Errorf("config-wins linux: got %q, want %q", got, "ghcr.io/from-config:linux")
	}
}

func TestResolveRunImage_ConfigWins_Windows(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	got := resolveRunImage("", workflow.PlatformWindows, loadRunConfig())
	if got != "ghcr.io/from-config:windows" {
		t.Errorf("config-wins windows: got %q, want %q", got, "ghcr.io/from-config:windows")
	}
}

func TestResolveRunImage_NoConfigFile(t *testing.T) {
	// Empty data dir → config.Load returns ENOENT → resolver returns "" so
	// the downstream RunJob can apply the built-in default.
	defer configDirGuard(t)()
	configDir = t.TempDir()

	if got := resolveRunImage("", workflow.PlatformLinux, loadRunConfig()); got != "" {
		t.Errorf("no-config: got %q, want empty (caller defaults)", got)
	}
}

func TestResolveRunImage_ConfigParseError(t *testing.T) {
	// Malformed TOML should not panic and must fall through to "" so the
	// caller can default. Today this happens via the swallowed error in
	// the config.Load call site — guard against a refactor that surfaces
	// the error and crashes the resolver.
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, "this is not valid TOML [\n")
	configDir = dir

	if got := resolveRunImage("", workflow.PlatformLinux, loadRunConfig()); got != "" {
		t.Errorf("config-parse-error: got %q, want empty fallback", got)
	}
}

func TestResolveRunImage_ConfigWithoutImageOverride(t *testing.T) {
	// A config.toml that exists but doesn't set a default image for this
	// platform must fall through to "" so the built-in default applies.
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	// Windows has no built-in default image (the runtime picks one from
	// the host build number), so DefaultImageFor("windows") returns "" —
	// resolver must propagate the empty string.
	if got := resolveRunImage("", workflow.PlatformWindows, loadRunConfig()); got != "" {
		t.Errorf("no-windows-override: got %q, want empty (caller defaults)", got)
	}
}

// A local run must inherit the host's L2Bridge settings. Before this, `ephemerd
// run` built its own default network: on Windows an HNS NAT network plus the
// NAT-era netsh rules that block RFC1918 host-wide — which severs the host's own
// DNS when its resolver is a LAN address — and put the job on an UNFILTERED
// network on a host deliberately configured to filter.
func TestRunNetworkOptions_CarriesL2BridgeConfig(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"

[network]
l2bridge_egress = true
host_nic = "Ethernet"
ip_pool = "192.0.2.192/27"
public_dns = ["9.9.9.9"]
extra_allowed_destinations = ["198.51.100.0/24"]
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	opts := runNetworkOptions(loadRunConfig(), quietLog())

	if !opts.L2BridgeEgress {
		t.Error("L2BridgeEgress not carried into the run; the job would land on the unfiltered NAT network")
	}
	if opts.HostNIC != "Ethernet" {
		t.Errorf("HostNIC: got %q, want %q", opts.HostNIC, "Ethernet")
	}
	if opts.IPPool != "192.0.2.192/27" {
		t.Errorf("IPPool: got %q, want %q", opts.IPPool, "192.0.2.192/27")
	}
	if len(opts.PublicDNS) != 1 || opts.PublicDNS[0] != "9.9.9.9" {
		t.Errorf("PublicDNS: got %v, want [9.9.9.9]", opts.PublicDNS)
	}
	if len(opts.ExtraAllowedCIDRs) != 1 || opts.ExtraAllowedCIDRs[0] != "198.51.100.0/24" {
		t.Errorf("ExtraAllowedCIDRs: got %v, want [198.51.100.0/24]", opts.ExtraAllowedCIDRs)
	}
}

// AllowHostAccess must follow the same rule serve uses (needsHostAccess): the
// host /32 allow exists only because dind and the module proxy serve job
// containers over the network. With neither enabled the strict posture applies.
func TestRunNetworkOptions_AllowHostAccessFollowsDind(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"

[network]
l2bridge_egress = true
host_nic = "Ethernet"
ip_pool = "192.0.2.192/27"

[dind]
enabled = true
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	if opts := runNetworkOptions(loadRunConfig(), quietLog()); !opts.AllowHostAccess {
		t.Error("AllowHostAccess must be set when dind is enabled, or dind cannot reach the host and jobs fail to provision")
	}
}

func TestRunNetworkOptions_StrictWhenNothingServesContainers(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"

[network]
l2bridge_egress = true
host_nic = "Ethernet"
ip_pool = "192.0.2.192/27"

[dind]
enabled = false
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	if opts := runNetworkOptions(loadRunConfig(), quietLog()); opts.AllowHostAccess {
		t.Error("AllowHostAccess must stay false when nothing serves containers — that is the strictest posture")
	}
}

// No config at all is not fatal: the run falls back to the built-in default
// network (and warns on Windows that egress is unfiltered).
func TestRunNetworkOptions_NoConfigFallsBack(t *testing.T) {
	defer configDirGuard(t)()
	configDir = t.TempDir()

	opts := runNetworkOptions(loadRunConfig(), quietLog())
	if opts.L2BridgeEgress || opts.HostNIC != "" || opts.IPPool != "" {
		t.Errorf("no-config: expected zero-value options, got %+v", opts)
	}
}
