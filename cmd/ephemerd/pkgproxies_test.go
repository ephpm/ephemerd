package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/ephpm/ephemerd/pkg/config"
)

// TestPkgProxyPorts is the firewall half of the wiring. These ports are fed
// into networking.Config.GatewayPorts; a proxy that is enabled but missing
// here binds fine, passes its health probe over loopback, advertises its env
// var — and is then unreachable from every job container, which looks like a
// hung download rather than a misconfiguration.
func TestPkgProxyPorts(t *testing.T) {
	t.Parallel()

	if got := pkgProxyPorts(&config.Config{}); len(got) != 0 {
		t.Errorf("pkgProxyPorts with nothing enabled = %v, want none opened", got)
	}

	var cfg config.Config
	cfg.NpmProxy.Enabled = true
	cfg.PipProxy.Enabled = true
	cfg.PubProxy.Enabled = true
	cfg.GhrelProxy.Enabled = true
	cfg.ComposerProxy.Enabled = true

	got := pkgProxyPorts(&cfg)
	for _, want := range []int{
		defaultNpmProxyPort,
		defaultPipProxyPort,
		defaultPubProxyPort,
		defaultGhrelProxyPort,
		defaultComposerProxyPort,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("port %d is not opened to job containers; got %v", want, got)
		}
	}

	// The defaults must stay distinct, or two proxies fight over a listener
	// and the second one silently fails to start.
	seen := map[int]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("port %d is claimed by two proxies: %v", p, got)
		}
		seen[p] = true
	}

	// An explicit port overrides the default.
	cfg.GhrelProxy.Port = 9999
	if got := pkgProxyPorts(&cfg); !slices.Contains(got, 9999) || slices.Contains(got, defaultGhrelProxyPort) {
		t.Errorf("pkgProxyPorts = %v, want 9999 instead of %d", got, defaultGhrelProxyPort)
	}
}

// TestNewCachesAreClearable: every cache these proxies write to must be
// known to `ephemerd cache`, or it grows until it fills the node's disk with
// nothing to clear it. The directory names must match what startPkgProxies
// actually passes as CacheDir.
func TestNewCachesAreClearable(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ghrel", "composer"} {
		c, ok := cacheByName(name)
		if !ok {
			t.Errorf("cache %q is not managed by `ephemerd cache`", name)
			continue
		}
		if want := filepath.Join("cache", name); c.Rel != want {
			t.Errorf("cache %q lives at %q, want %q", name, c.Rel, want)
		}
		if !c.LiveSafe {
			t.Errorf("cache %q is not live-safe; every entry is a pull-through copy that a miss refetches", name)
		}
	}
}
