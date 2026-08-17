// Package registrymirror routes container image pulls through a
// pull-through registry cache on the LAN.
//
// # Why
//
// ephemerd nodes re-pull the same base image constantly. A production
// linux-amd64 node measured 294 GB inbound over 4.1 days across ~330 jobs —
// roughly 890 MB per job — and the single biggest contributor was one
// 1.1 GB CI image pulled 163 times in seven days, because dind pulls into a
// per-job containerd namespace and the per-repo cache was not retaining
// content. Layers that a LAN cache would serve at wire speed were crossing
// the WAN dozens of times a day per node. Pointing every pull at a
// pull-through cache makes the first pull the only WAN pull, and takes the
// node out of Docker Hub's anonymous rate limit besides.
//
// # How
//
// containerd's docker resolver picks a registry endpoint through a
// docker.RegistryHosts function: given the host part of a reference
// ("docker.io"), it returns an ordered list of endpoints to try. The
// resolver walks that list and moves to the next entry whenever one fails
// to answer, 404s, or returns any 4xx/5xx. So "mirror first, origin
// second" is expressed directly as a two-element host list, and
// fail-open falls out of the resolver's own retry loop — no health
// checking, no circuit breaker, no state.
//
// The mirror entry is advertised with pull+resolve capabilities only.
// Pushes never touch it: a pull-through cache is not somewhere to publish,
// and the push path (pkg/dind/registry.go) is deliberately left on the
// origin registry.
//
// # No-mirror path
//
// Every entry point is nil-safe and returns no options when no mirror is
// configured, so an unconfigured node builds exactly the same pull call it
// built before this package existed.
package registrymirror

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/ephpm/ephemerd/pkg/config"
)

// Mirror is a resolved, immutable mirror policy shared by every pull path.
// A nil *Mirror means "no mirror configured" and every method on it is a
// no-op, so callers never need a branch.
type Mirror struct {
	// hosts maps a normalized upstream registry host ("docker.io") to the
	// parsed base URL of the cache that serves it.
	hosts map[string]*url.URL

	// fallback keeps the origin registry in the host list behind the
	// mirror. See config.RegistryMirrorConfig.FallbackToOrigin.
	fallback bool

	// forwardCreds allows the caller's credentials to reach the mirror.
	// Off by default so a mirror cannot harvest a registry PAT with a
	// Basic challenge. See config.RegistryMirrorConfig.ForwardCredentials.
	forwardCreds bool

	log *slog.Logger
}

// New builds a Mirror from configuration. It returns nil when mirroring is
// disabled or nothing is mapped — the caller keeps the nil and every method
// stays a no-op.
//
// cfg must have passed config validation; malformed endpoints are dropped
// here with a warning rather than failing, because by this point the daemon
// is already running and degrading to origin pulls beats refusing to start.
func New(cfg config.RegistryMirrorConfig, log *slog.Logger) *Mirror {
	resolved := cfg.ResolvedMirrors()
	if len(resolved) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "registry-mirror")

	m := &Mirror{
		hosts:        make(map[string]*url.URL, len(resolved)),
		fallback:     cfg.ResolvedFallbackToOrigin(),
		forwardCreds: cfg.ForwardCredentials,
		log:          log,
	}
	for registry, endpoint := range resolved {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			// Validation should have caught this at load; if something
			// slipped through, skip the entry instead of pulling from a
			// nonsense URL.
			log.Warn("ignoring unusable registry mirror endpoint",
				"registry", registry, "endpoint", endpoint, "error", err)
			continue
		}
		m.hosts[registry] = u
	}
	if len(m.hosts) == 0 {
		return nil
	}

	log.Info("registry mirror active",
		"mirrors", m.describe(),
		"fallback_to_origin", m.fallback,
		"forward_credentials", m.forwardCreds)
	return m
}

// Enabled reports whether this Mirror routes anything. Nil-safe.
func (m *Mirror) Enabled() bool {
	return m != nil && len(m.hosts) > 0
}

// describe renders the host table for a log line, sorted so the output is
// stable across restarts (Go map iteration is not).
func (m *Mirror) describe() string {
	if !m.Enabled() {
		return ""
	}
	parts := make([]string, 0, len(m.hosts))
	for registry, u := range m.hosts {
		parts = append(parts, registry+"="+u.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Endpoint returns the mirror base URL serving an upstream registry host,
// and whether one is configured. The argument is normalized, so both
// "docker.io" and "index.docker.io" find a docker.io mapping. Nil-safe.
func (m *Mirror) Endpoint(registry string) (*url.URL, bool) {
	if !m.Enabled() {
		return nil, false
	}
	u, ok := m.hosts[config.NormalizeRegistryHost(registry)]
	return u, ok
}

// Creds supplies a username/secret pair for a registry host. It matches the
// shape docker.WithAuthCreds wants. A nil Creds means anonymous.
type Creds func(host string) (string, string, error)

// PullOpts returns the containerd RemoteOpts that route a pull through the
// mirror, authenticating with creds where credentials are needed.
//
// It returns nil — meaning "add nothing to the pull" — when no mirror is
// configured AND no credentials were supplied. That is the important
// property for the no-mirror path: an unconfigured node's client.Pull call
// is byte-identical to what it was before this package existed, right down
// to containerd constructing its own default resolver internally.
//
// Pass nil creds for an anonymous pull.
func (m *Mirror) PullOpts(creds Creds) []client.RemoteOpt {
	if !m.Enabled() && creds == nil {
		return nil
	}
	return []client.RemoteOpt{client.WithResolver(m.Resolver(creds))}
}

// Resolver builds a containerd resolver honoring this mirror. Exported for
// the pull paths that need the resolver itself rather than a RemoteOpt, and
// for tests.
func (m *Mirror) Resolver(creds Creds) remotes.Resolver {
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: m.RegistryHosts(creds),
	})
}

// RegistryHosts returns the docker.RegistryHosts function containerd's
// resolver consults for each reference. For a mirrored registry the list is
// [mirror, origin] (or [mirror] with fallback disabled); for every other
// registry it is exactly the containerd default, so a node with a docker.io
// mirror still reaches ghcr.io the ordinary way.
//
// Nil-safe: a nil Mirror yields the containerd default for every host, which
// is what makes this usable as the single code path for "pull with
// credentials, no mirror" too.
func (m *Mirror) RegistryHosts(creds Creds) docker.RegistryHosts {
	// The origin authorizer drives the standard Bearer challenge → token
	// GET → retry flow. It is required even for anonymous pulls: Docker Hub
	// answers an unauthenticated manifest request with a 401 challenge and
	// expects the client to fetch an anonymous token from auth.docker.io.
	originAuth := newAuthorizer(creds)

	// The mirror gets its own authorizer, with credentials only when the
	// operator opted in. Sharing the origin authorizer would let a mirror
	// answer with a Basic challenge and collect the upstream PAT — over
	// plaintext, when the endpoint is http://.
	mirrorAuth := newAuthorizer(nil)
	if m.Enabled() && m.forwardCreds {
		mirrorAuth = originAuth
	}

	// containerd's stock endpoint for any registry: https, /v2, plain HTTP
	// only for localhost. This is what an unmirrored host resolves through,
	// unchanged.
	origins := docker.ConfigureDefaultRegistries(
		docker.WithAuthorizer(originAuth),
		docker.WithPlainHTTP(docker.MatchLocalhost),
	)
	return m.registryHosts(origins, mirrorAuth)
}

// registryHosts is the testable core of RegistryHosts: it takes the origin
// endpoint table as a parameter so a test can point "the origin registry" at
// a local server and observe the fallback actually happening, rather than
// asserting on a struct and hoping containerd behaves.
func (m *Mirror) registryHosts(origins docker.RegistryHosts, mirrorAuth docker.Authorizer) docker.RegistryHosts {
	return func(registry string) ([]docker.RegistryHost, error) {
		origin, err := origins(registry)
		if err != nil {
			return nil, err
		}
		endpoint, ok := m.Endpoint(registry)
		if !ok {
			// Not a mirrored registry — hand back containerd's own answer
			// untouched, so a node with a docker.io mirror still reaches
			// ghcr.io exactly the way it always did.
			return origin, nil
		}

		mirrorHost := docker.RegistryHost{
			Client:     &http.Client{Transport: docker.DefaultHTTPTransport(nil)},
			Authorizer: mirrorAuth,
			Host:       endpoint.Host,
			Scheme:     endpoint.Scheme,
			Path:       joinV2(endpoint.Path),
			// Pull + resolve only. A pull-through cache must never be
			// offered as a push target, and asking one for the OCI
			// referrers API just produces a 404 per pull.
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
		}

		if !m.fallback {
			// Mirror is authoritative: nothing behind it, so an image the
			// cache cannot serve fails rather than reaching the WAN.
			return []docker.RegistryHost{mirrorHost}, nil
		}
		// Fail open. containerd tries hosts in order and moves on when one
		// errors or answers 4xx/5xx, so a cache that is down, wedged, or
		// simply does not have the image costs one failed request and the
		// pull completes against the origin — the node degrades to the WAN
		// speed it had before the mirror existed, and the job still runs.
		return append([]docker.RegistryHost{mirrorHost}, origin...), nil
	}
}

// LogPull emits one line per pull naming the endpoint actually tried first,
// so an operator can confirm from the daemon log that traffic is going to
// the cache. No-op when no mirror covers the reference.
func (m *Mirror) LogPull(ref string) {
	if !m.Enabled() {
		return
	}
	endpoint, ok := m.Endpoint(RegistryHostFromRef(ref))
	if !ok {
		return
	}
	m.log.Info("pulling through registry mirror",
		"ref", ref,
		"mirror", endpoint.String(),
		"fallback_to_origin", m.fallback)
}

// newAuthorizer builds a docker authorizer, matching what containerd's own
// default resolver constructs. creds may be nil for anonymous access.
func newAuthorizer(creds Creds) docker.Authorizer {
	if creds == nil {
		return docker.NewDockerAuthorizer()
	}
	return docker.NewDockerAuthorizer(docker.WithAuthCreds(creds))
}

// joinV2 puts the registry API root under any path prefix the mirror URL
// carries, so "https://harbor.lan/dockerhub-proxy" becomes
// "/dockerhub-proxy/v2" and a bare "http://registry.lan:5000" becomes "/v2".
func joinV2(prefix string) string {
	if prefix == "" || prefix == "/" {
		return "/v2"
	}
	return path.Join("/", prefix, "v2")
}

// RegistryHostFromRef peels the registry hostname off an image reference,
// applying Docker's rule that a first segment without a dot, colon, or the
// literal name "localhost" is not a host at all but the start of a Docker
// Hub repository path.
//
// It exists here so the mirror lookup agrees with the host containerd's own
// reference parser will hand to RegistryHosts.
func RegistryHostFromRef(ref string) string {
	// Strip a digest first: the "@sha256:" colon must not be mistaken for a
	// port separator.
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	var first string
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	} else {
		// No slash at all: a single-segment name, possibly tagged
		// ("alpine:3.20"). Always Docker Hub.
		return "docker.io"
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return config.NormalizeRegistryHost(first)
	}
	return "docker.io"
}

// String renders the policy for diagnostics.
func (m *Mirror) String() string {
	if !m.Enabled() {
		return "registrymirror(disabled)"
	}
	return fmt.Sprintf("registrymirror(%s fallback=%t forward_creds=%t)",
		m.describe(), m.fallback, m.forwardCreds)
}
