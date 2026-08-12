// Package proxies defines the CacheProxy interface for language-specific
// caching reverse proxies. Each proxy sits on the bridge gateway so job
// containers can reach it, caches downloads on disk, and injects the
// appropriate env var into containers.
//
// Implementations live in sub-packages:
//
//	pkg/proxies/go/    — Go module proxy (GOPROXY)
//	pkg/proxies/cargo/ — Cargo sparse registry + crate/rustup proxy
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

// Mount is a host→container bind mount a cache proxy needs in every job
// container. Env vars are enough for toolchains that take their proxy from
// the environment (Go, rustup); Cargo is not one of them — its source
// replacement is only read from a config file, never from CARGO_* env vars
// (verified empirically; see pkg/proxies/cargo). Such proxies generate the
// file on the host and declare it here.
type Mount struct {
	// Source is an absolute host path (a directory).
	Source string
	// Destination is the absolute path inside the container.
	Destination string
	// ReadOnly mounts the source read-only. Config material should always
	// be read-only: a job must never be able to rewrite what the next job
	// on this host will read.
	ReadOnly bool
}

// MountProvider is an OPTIONAL interface a CacheProxy may implement when
// env vars alone cannot point a toolchain at the proxy. Callers type-assert
// for it; a proxy that does not implement it needs no mounts.
type MountProvider interface {
	// Mounts returns the bind mounts to add to every job container spec.
	Mounts() []Mount
}
