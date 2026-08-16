package main

import (
	"fmt"
	"log/slog"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/proxies"
	npmproxy "github.com/ephpm/ephemerd/pkg/proxies/npm"
	pipproxy "github.com/ephpm/ephemerd/pkg/proxies/pip"
	pubproxy "github.com/ephpm/ephemerd/pkg/proxies/pub"
)

// Default listen ports for the language package caches, on the bridge
// gateway alongside the Go module proxy (8082).
const (
	defaultNpmProxyPort = 8084
	defaultPipProxyPort = 8085
	defaultPubProxyPort = 8086
)

// healthGated is the interface a cache proxy implements when its env var
// cannot fail over on its own.
//
// GOPROXY has "|direct" and so degrades by itself. npm_config_registry,
// PIP_INDEX_URL and PUB_HOSTED_URL have no such escape hatch: whatever they
// name IS the registry. The next best thing is to not name a proxy that is
// not answering, so these are probed after Start and only injected if the
// probe passes.
type healthGated interface {
	Healthy() bool
}

// pkgProxyPorts returns the gateway ports the enabled language package
// caches will bind, so the container firewall allows them.
func pkgProxyPorts(cfg *config.Config) []int {
	var ports []int
	if cfg.NpmProxy.Enabled {
		ports = append(ports, cfg.NpmProxy.ProxyPort(defaultNpmProxyPort))
	}
	if cfg.PipProxy.Enabled {
		ports = append(ports, cfg.PipProxy.ProxyPort(defaultPipProxyPort))
	}
	if cfg.PubProxy.Enabled {
		ports = append(ports, cfg.PubProxy.ProxyPort(defaultPubProxyPort))
	}
	return ports
}

// startPkgProxies starts whichever of the npm, pip and pub caches are
// enabled and returns those that came up healthy, plus a teardown func.
//
// FAIL-OPEN AT STARTUP: a proxy that fails to start, or that starts but does
// not answer its own health probe, is simply left out of the returned slice.
// Its env vars are then never injected into any job container, and jobs go
// straight to the public registry — normal speed, not a failed build.
func startPkgProxies(cfg *config.Config, dataDir string, net *networking.Manager, log *slog.Logger) ([]proxies.CacheProxy, func()) {
	type candidate struct {
		enabled bool
		port    int
		build   func(addr string) (proxies.CacheProxy, error)
	}

	candidates := []candidate{
		{
			enabled: cfg.NpmProxy.Enabled,
			port:    cfg.NpmProxy.ProxyPort(defaultNpmProxyPort),
			build: func(addr string) (proxies.CacheProxy, error) {
				return npmproxy.New(npmproxy.Config{
					CacheDir:     joinPath(dataDir, "cache", "npm"),
					Upstream:     cfg.NpmProxy.Upstream,
					ListenAddr:   addr,
					PackumentTTL: cfg.NpmProxy.ProxyIndexTTL(),
					MaxBytes:     cfg.NpmProxy.ProxyMaxBytes(),
					AllowedHosts: cfg.NpmProxy.AllowedHosts,
					Cleanup:      cfg.NpmProxy.Cleanup,
					Log:          log.With("component", "npm-proxy"),
				})
			},
		},
		{
			enabled: cfg.PipProxy.Enabled,
			port:    cfg.PipProxy.ProxyPort(defaultPipProxyPort),
			build: func(addr string) (proxies.CacheProxy, error) {
				return pipproxy.New(pipproxy.Config{
					CacheDir:     joinPath(dataDir, "cache", "pip"),
					Upstream:     cfg.PipProxy.Upstream,
					ListenAddr:   addr,
					IndexTTL:     cfg.PipProxy.ProxyIndexTTL(),
					MaxBytes:     cfg.PipProxy.ProxyMaxBytes(),
					AllowedHosts: cfg.PipProxy.AllowedHosts,
					Cleanup:      cfg.PipProxy.Cleanup,
					Log:          log.With("component", "pip-proxy"),
				})
			},
		},
		{
			enabled: cfg.PubProxy.Enabled,
			port:    cfg.PubProxy.ProxyPort(defaultPubProxyPort),
			build: func(addr string) (proxies.CacheProxy, error) {
				return pubproxy.New(pubproxy.Config{
					CacheDir:     joinPath(dataDir, "cache", "pub"),
					Upstream:     cfg.PubProxy.Upstream,
					ListenAddr:   addr,
					ListingTTL:   cfg.PubProxy.ProxyIndexTTL(),
					MaxBytes:     cfg.PubProxy.ProxyMaxBytes(),
					AllowedHosts: cfg.PubProxy.AllowedHosts,
					Cleanup:      cfg.PubProxy.Cleanup,
					Log:          log.With("component", "pub-proxy"),
				})
			},
		},
	}

	var (
		started []proxies.CacheProxy
		stops   []func()
	)
	for _, c := range candidates {
		if !c.enabled {
			continue
		}
		addr := fmt.Sprintf("%s:%d", net.GatewayIP(), c.port)
		p, err := c.build(addr)
		if err != nil {
			log.Warn("failed to create package cache proxy, continuing without it", "port", c.port, "error", err)
			continue
		}
		if err := p.Start(); err != nil {
			log.Warn("failed to start package cache proxy, continuing without it", "proxy", p.Name(), "error", err)
			continue
		}

		// Reachability from job containers is a pool-wide concern, so it is
		// handled once at daemon startup: pkgProxyPorts() feeds these ports
		// into networking.Config.GatewayPorts (see main.go). Manager's
		// OpenHostPort is deliberately NOT used here — it is the per-job dind
		// path and fails closed without a specific container address, because a
		// pool-wide allow there would expose one job's Docker API to every
		// other job. A shared read-only cache has no such per-job identity.
		//
		// Caveat: GatewayPorts is enforced on Linux only. On a Windows
		// L2Bridge node the host's inbound default-deny still drops these,
		// exactly as it does for [module_proxy] and [cargo_proxy]. The health
		// probe below runs over loopback and so cannot detect it.

		// Health gate. Only a proxy that answers is advertised to jobs.
		if hg, ok := p.(healthGated); ok && !hg.Healthy() {
			log.Warn("package cache proxy did not answer its health probe; not injecting its env vars", "proxy", p.Name())
			if err := p.Stop(); err != nil {
				log.Warn("error stopping unhealthy package cache proxy", "proxy", p.Name(), "error", err)
			}
			continue
		}

		log.Info("package cache proxy ready", "proxy", p.Name(), "addr", p.Addr(), "env", p.EnvVars())
		started = append(started, p)
		stopped := p
		stops = append(stops, func() {
			if err := stopped.Stop(); err != nil {
				log.Warn("error stopping package cache proxy", "proxy", stopped.Name(), "error", err)
			}
		})
	}

	return started, func() {
		for _, stop := range stops {
			stop()
		}
	}
}
