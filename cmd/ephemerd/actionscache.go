package main

import (
	"fmt"
	"log/slog"
	runtime_ "runtime"

	"github.com/ephpm/ephemerd/pkg/config"
	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/ephpm/ephemerd/pkg/providers"
	"github.com/ephpm/ephemerd/pkg/proxies/actionscache"
	"github.com/ephpm/ephemerd/pkg/scheduler"
)

// actionsCachePorts returns the gateway port [actions_cache] binds, so the
// container firewall allows it. Linux only, like the cache itself.
func actionsCachePorts(cfg *config.Config) []int {
	if !cfg.ActionsCache.Enabled || runtime_.GOOS != "linux" {
		return nil
	}
	return []int{cfg.ActionsCache.ResolvedPort()}
}

// startActionsCache starts the LAN Actions cache when [actions_cache] is
// enabled, returning the scheduler's handle on it (nil when off) and a
// teardown func.
//
// FAIL-OPEN: a cache that cannot start is logged and left out — jobs keep
// GitHub's cache. One that starts with its registry down is kept: it turns
// itself on for new jobs as soon as the registry answers (the scheduler asks
// Healthy() per job).
func startActionsCache(cfg *config.Config, net *networking.Manager, log *slog.Logger) (scheduler.ActionsCache, func()) {
	noop := func() {}
	ac := cfg.ActionsCache
	if !ac.Enabled {
		return nil, noop
	}
	if runtime_.GOOS != "linux" {
		log.Warn("actions_cache is Linux-only; leaving it off on this host", "goos", runtime_.GOOS)
		return nil, noop
	}
	addr := fmt.Sprintf("%s:%d", net.GatewayIP(), ac.ResolvedPort())
	p, err := actionscache.New(actionscache.Config{
		ListenAddr:         addr,
		AdvertiseBase:      "http://" + addr,
		Registry:           ac.Registry,
		RegistryUser:       ac.Username,
		RegistryPassword:   ac.ResolvedPassword(),
		MaxBytesPerRepo:    ac.MaxBytesPerRepo(),
		MaxEntryBytes:      ac.MaxEntryBytes(),
		UpstreamResultsURL: ac.UpstreamResultsURL,
		JobForIP:           net.JobForIP,
		Log:                log,
	})
	if err != nil {
		log.Warn("actions cache misconfigured; jobs use GitHub's cache", "error", err)
		return nil, noop
	}
	if err := p.Start(); err != nil {
		log.Warn("failed to start actions cache; jobs use GitHub's cache", "error", err)
		return nil, noop
	}
	return actionsCacheAdapter{p}, func() {
		if err := p.Stop(); err != nil {
			log.Warn("error stopping actions cache", "error", err)
		}
	}
}

// actionsCacheAdapter carries the provider's scope type into the proxy's.
type actionsCacheAdapter struct{ *actionscache.Proxy }

func (a actionsCacheAdapter) Bind(runner string, s providers.CacheScope) {
	a.Proxy.Bind(runner, actionscache.Scope{Repo: s.Repo, Write: s.Write, Reads: s.Reads})
}
