// Package proxies defines the CacheProxy interface for language-specific
// caching reverse proxies. Each proxy sits on the bridge gateway so job
// containers can reach it, caches downloads on disk, and injects the
// appropriate env var into containers.
//
// Implementations live in sub-packages:
//
//	pkg/proxies/go/    — Go module proxy (GOPROXY)
//	pkg/proxies/npm/   — (future) npm registry proxy
//	pkg/proxies/pip/   — (future) pip index proxy
package proxies

// CacheProxy is a language-specific caching proxy that sits between
// job containers and an upstream package registry. Implementations
// handle protocol-specific caching (e.g., Go module proxy, npm registry).
type CacheProxy interface {
	// Start begins serving the proxy. Returns after the listener is bound.
	Start() error

	// Stop shuts down the proxy and optionally cleans up the cache.
	Stop() error

	// Addr returns the address the proxy is listening on (host:port).
	Addr() string

	// EnvVars returns environment variables to inject into job containers
	// so they use this proxy (e.g., GOPROXY=http://10.88.0.1:8082,direct).
	EnvVars() []string

	// Name returns a human-readable name for logging (e.g., "go", "npm").
	Name() string
}
