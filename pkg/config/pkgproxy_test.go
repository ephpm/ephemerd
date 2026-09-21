package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoad_PkgProxyConfig covers the language package caches
// ([npm_proxy], [pip_proxy], [pub_proxy]) decoding from TOML, including the
// duration and slice fields.
func TestLoad_PkgProxyConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"

[npm_proxy]
enabled = true
port = 9100
upstream = "https://npm.mirror.test"
index_ttl = "90s"
max_size_gb = 12
allowed_hosts = ["cdn.mirror.test"]
cleanup = true

[pip_proxy]
enabled = true
port = 9101
upstream = "https://pypi.mirror.test"

[pub_proxy]
enabled = false
max_size_gb = -1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	npm := cfg.NpmProxy
	if !npm.Enabled {
		t.Error("NpmProxy.Enabled = false, want true")
	}
	if npm.ProxyPort(8084) != 9100 {
		t.Errorf("NpmProxy port = %d, want 9100", npm.ProxyPort(8084))
	}
	if npm.Upstream != "https://npm.mirror.test" {
		t.Errorf("NpmProxy.Upstream = %q", npm.Upstream)
	}
	if npm.ProxyIndexTTL() != 90*time.Second {
		t.Errorf("NpmProxy index TTL = %v, want 90s", npm.ProxyIndexTTL())
	}
	if npm.ProxyMaxBytes() != 12<<30 {
		t.Errorf("NpmProxy max bytes = %d, want %d", npm.ProxyMaxBytes(), int64(12)<<30)
	}
	if len(npm.AllowedHosts) != 1 || npm.AllowedHosts[0] != "cdn.mirror.test" {
		t.Errorf("NpmProxy.AllowedHosts = %v", npm.AllowedHosts)
	}
	if !npm.Cleanup {
		t.Error("NpmProxy.Cleanup = false, want true")
	}

	pip := cfg.PipProxy
	if !pip.Enabled || pip.ProxyPort(8085) != 9101 {
		t.Errorf("PipProxy = %+v", pip)
	}
	// Unset fields must fall back to the documented defaults.
	if pip.ProxyIndexTTL() != 5*time.Minute {
		t.Errorf("PipProxy index TTL = %v, want the 5m default", pip.ProxyIndexTTL())
	}
	if pip.ProxyMaxBytes() != 5<<30 {
		t.Errorf("PipProxy max bytes = %d, want the 5 GiB default", pip.ProxyMaxBytes())
	}
	if pip.Cleanup {
		t.Error("PipProxy.Cleanup defaults to true; a pull-through cache emptied on restart saves nothing")
	}

	if cfg.PubProxy.Enabled {
		t.Error("PubProxy.Enabled = true, want false")
	}
	if cfg.PubProxy.ProxyMaxBytes() != -1 {
		t.Errorf("PubProxy max bytes = %d, want -1 (explicitly unbounded)", cfg.PubProxy.ProxyMaxBytes())
	}
}

// TestPkgProxyDefaultsAreOffAndBounded pins the two postures that matter:
// the caches are opt-in, and an operator who enables one without tuning it
// still gets a bounded cache rather than an unbounded disk-filler.
func TestPkgProxyDefaultsAreOffAndBounded(t *testing.T) {
	var cfg Config
	for name, p := range map[string]PkgProxyConfig{
		"npm_proxy":      cfg.NpmProxy,
		"pip_proxy":      cfg.PipProxy,
		"pub_proxy":      cfg.PubProxy,
		"composer_proxy": cfg.ComposerProxy,
		"ghrel_proxy":    cfg.GhrelProxy.PkgProxyConfig,
	} {
		if p.Enabled {
			t.Errorf("[%s] is enabled by default, want opt-in like [module_proxy]", name)
		}
		if got := p.ProxyMaxBytes(); got <= 0 {
			t.Errorf("[%s] default max bytes = %d, want a positive bound", name, got)
		}
		if p.Cleanup {
			t.Errorf("[%s] cleanup defaults to true", name)
		}
	}
}

// TestLoad_GhrelAndComposerProxyConfig covers the two newest caches.
//
// [ghrel_proxy] is the interesting one: it embeds PkgProxyConfig, so the
// shared keys have to decode into the embedded struct from the SAME TOML
// table as its own `immutable_assets`. If that ever stops working the
// failure is silent — the proxy just runs on defaults.
func TestLoad_GhrelAndComposerProxyConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[github]
token = "ghp_test"
owner = "org"

[ghrel_proxy]
enabled = true
port = 9102
upstream = "https://ghe.acme.test/api/v3"
index_ttl = "30s"
max_size_gb = 20
allowed_hosts = ["assets.ghe.acme.test"]
immutable_assets = true
cleanup = true

[composer_proxy]
enabled = true
upstream = "https://packagist.mirror.test"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	gh := cfg.GhrelProxy
	if !gh.Enabled {
		t.Error("GhrelProxy.Enabled = false, want true")
	}
	if !gh.ImmutableAssets {
		t.Error("GhrelProxy.ImmutableAssets = false, want true")
	}
	// Every one of these lives on the EMBEDDED struct.
	if got := gh.ProxyPort(8087); got != 9102 {
		t.Errorf("GhrelProxy port = %d, want 9102", got)
	}
	if gh.Upstream != "https://ghe.acme.test/api/v3" {
		t.Errorf("GhrelProxy.Upstream = %q", gh.Upstream)
	}
	if got := gh.ProxyIndexTTL(); got != 30*time.Second {
		t.Errorf("GhrelProxy index TTL = %v, want 30s", got)
	}
	if got := gh.ProxyMaxBytes(); got != 20<<30 {
		t.Errorf("GhrelProxy max bytes = %d, want %d", got, int64(20)<<30)
	}
	if len(gh.AllowedHosts) != 1 || gh.AllowedHosts[0] != "assets.ghe.acme.test" {
		t.Errorf("GhrelProxy.AllowedHosts = %v", gh.AllowedHosts)
	}
	if !gh.Cleanup {
		t.Error("GhrelProxy.Cleanup = false, want true")
	}

	comp := cfg.ComposerProxy
	if !comp.Enabled {
		t.Error("ComposerProxy.Enabled = false, want true")
	}
	if comp.Upstream != "https://packagist.mirror.test" {
		t.Errorf("ComposerProxy.Upstream = %q", comp.Upstream)
	}
	if got := comp.ProxyPort(8088); got != 8088 {
		t.Errorf("ComposerProxy port = %d, want the 8088 default", got)
	}
	if got := comp.ProxyIndexTTL(); got != 5*time.Minute {
		t.Errorf("ComposerProxy index TTL = %v, want the 5m default", got)
	}
	if got := comp.ProxyMaxBytes(); got != 5<<30 {
		t.Errorf("ComposerProxy max bytes = %d, want the 5 GiB default", got)
	}
}

// TestGhrelImmutableAssetsDefaultsOff: GitHub is the one ecosystem here that
// lets an asset be replaced in place, so "cache forever, never revalidate"
// must be something an operator opts into, never the default.
func TestGhrelImmutableAssetsDefaultsOff(t *testing.T) {
	var cfg Config
	if cfg.GhrelProxy.ImmutableAssets {
		t.Error("[ghrel_proxy].immutable_assets defaults to true; it is a promise the proxy cannot verify")
	}
}

func TestPkgProxyPortFallback(t *testing.T) {
	var p PkgProxyConfig
	if got := p.ProxyPort(8084); got != 8084 {
		t.Errorf("ProxyPort(8084) with no port set = %d, want 8084", got)
	}
	p.Port = -5
	if got := p.ProxyPort(8084); got != 8084 {
		t.Errorf("a negative port must fall back to the default, got %d", got)
	}
	p.Port = 1234
	if got := p.ProxyPort(8084); got != 1234 {
		t.Errorf("ProxyPort = %d, want 1234", got)
	}
}

func TestPkgProxyNegativeTTLMeansAlwaysRevalidate(t *testing.T) {
	p := PkgProxyConfig{IndexTTL: -time.Second}
	if got := p.ProxyIndexTTL(); got >= 0 {
		t.Errorf("ProxyIndexTTL = %v, want the negative value preserved", got)
	}
}
