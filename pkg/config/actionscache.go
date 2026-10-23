package config

import (
	"errors"
	"os"
	"strings"
)

// ActionsCacheConfig configures [actions_cache]: GitHub Actions' cache
// service (what actions/cache and Swatinem/rust-cache talk to) served from
// the LAN instead of GitHub, with entries stored in a writable OCI registry
// (registry:2 in normal, non-proxy mode — the pull-through mirror is
// read-only and cannot hold them).
//
// Linux hosts only. Off by default; with it off nothing about a job changes.
// With it on, a job only uses the LAN cache when the registry answers its
// health probe at job start AND the job's runner could be patched; otherwise
// it uses GitHub's cache exactly as before.
//
// Which entries a job may read and write is decided by ephemerd from
// GitHub's record of the job's run — branch pushes save under their branch,
// pull requests under their PR, and other events are read-only. See
// pkg/proxies/actionscache.
type ActionsCacheConfig struct {
	// Enabled turns the cache on. Default false.
	Enabled bool `toml:"enabled"`
	// Port is the listen port on the bridge gateway. Default 8089.
	Port int `toml:"port"`

	// Registry is the writable registry's base URL, e.g.
	// "http://192.168.16.224:5001". Required when enabled.
	Registry string `toml:"registry"`
	// Username / Password authenticate to the registry (htpasswd). Jobs
	// never see them: only ephemerd talks to the registry.
	Username string `toml:"username"`
	Password string `toml:"password"`
	// PasswordEnv names an environment variable to read the password from
	// instead of writing it in this file. Wins over Password when set.
	PasswordEnv string `toml:"password_env"`

	// MaxSizeGBPerRepo caps each repository's total cache; the least
	// recently used entries are deleted past it. Default 100. 0 keeps the
	// default; a negative value disables eviction.
	MaxSizeGBPerRepo int `toml:"max_size_gb_per_repo"`
	// MaxEntryGB refuses a single entry larger than this. Default 10.
	MaxEntryGB int `toml:"max_entry_gb"`

	// UpstreamResultsURL is where requests that are not the cache (artifact
	// uploads and downloads) are relayed. Default
	// "https://results-receiver.actions.githubusercontent.com".
	UpstreamResultsURL string `toml:"upstream_results_url"`
}

// DefaultActionsCachePort is the gateway port [actions_cache] binds when
// port is unset — the next free one after the package caches (8082-8088).
const DefaultActionsCachePort = 8089

// ResolvedPort returns Port or the default.
func (c ActionsCacheConfig) ResolvedPort() int {
	if c.Port > 0 {
		return c.Port
	}
	return DefaultActionsCachePort
}

// ResolvedPassword returns the registry password, from PasswordEnv when set.
func (c ActionsCacheConfig) ResolvedPassword() string {
	if c.PasswordEnv != "" {
		return os.Getenv(c.PasswordEnv)
	}
	return c.Password
}

// MaxBytesPerRepo returns the eviction cap in bytes (0 = no eviction).
func (c ActionsCacheConfig) MaxBytesPerRepo() int64 {
	switch {
	case c.MaxSizeGBPerRepo < 0:
		return 0
	case c.MaxSizeGBPerRepo == 0:
		return 100 << 30
	default:
		return int64(c.MaxSizeGBPerRepo) << 30
	}
}

// MaxEntryBytes returns the per-entry limit in bytes.
func (c ActionsCacheConfig) MaxEntryBytes() int64 {
	if c.MaxEntryGB <= 0 {
		return 10 << 30
	}
	return int64(c.MaxEntryGB) << 30
}

func (c ActionsCacheConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Registry) == "" {
		return errors.New("actions_cache.registry is required when actions_cache is enabled: the writable registry entries are stored in")
	}
	if err := validateMirrorURL("actions_cache.registry", c.Registry); err != nil {
		return err
	}
	if c.UpstreamResultsURL != "" {
		if err := validateMirrorURL("actions_cache.upstream_results_url", c.UpstreamResultsURL); err != nil {
			return err
		}
	}
	if c.PasswordEnv != "" && os.Getenv(c.PasswordEnv) == "" {
		return errors.New("actions_cache.password_env names " + c.PasswordEnv + ", which is not set in ephemerd's environment")
	}
	return nil
}
