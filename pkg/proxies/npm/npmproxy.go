// Package npmproxy implements proxies.CacheProxy for the npm ecosystem.
//
// It is a pull-through cache for the two request shapes an `npm install`
// makes, which are cached very differently:
//
//	GET /<pkg>                     packument (metadata)  — MUTABLE
//	GET /<pkg>/-/<pkg>-<ver>.tgz   package tarball       — IMMUTABLE
//
// # WHAT IS CACHED AND WHY
//
// Tarballs are the bytes: a cold `npm ci` on a mid-sized project pulls
// hundreds of megabytes of them, and a published (name, version) tarball is
// never re-published with different content — npm's registry forbids it and
// every lockfile in the world pins its integrity hash. Those are cached
// permanently and never revalidated.
//
// Packuments are NOT immutable. A packument gains a version every time
// anyone publishes, and `dist-tags.latest` moves. Caching one forever would
// pin every job on the node to whatever versions existed when the daemon
// started — a job asking for a version published an hour ago would get
// ETARGET. So packuments are cached with a SHORT TTL (default 5 minutes)
// and then revalidated with a conditional GET. npm's registry serves strong
// ETags, so the steady-state cost of a stale-but-unchanged packument is one
// 304, not a re-download.
//
// # WHY THE PACKUMENT IS REWRITTEN
//
// Every packument carries ABSOLUTE tarball URLs (dist.tarball) pointing at
// the upstream registry. Left alone, npm would fetch metadata from the cache
// and then every byte straight from registry.npmjs.org — the cache would
// hold only the cheap part. dist.tarball is therefore rewritten to point at
// this proxy's artifact route. dist.integrity/dist.shasum are untouched and
// still verified by npm against the bytes we serve, so the rewrite cannot
// smuggle in a different tarball.
package npmproxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

// Defaults. Kept here rather than in the config package so the proxy is
// usable (and testable) standalone.
const (
	// DefaultUpstream is the public npm registry.
	DefaultUpstream = "https://registry.npmjs.org"
	// DefaultPackumentTTL is how long a cached packument is served before
	// revalidation. Short by design: a dependency published minutes ago
	// must not be invisible to the node for an hour.
	DefaultPackumentTTL = 5 * time.Minute
)

// abbreviatedAccept is the media type npm sends when it wants the small
// "install" packument rather than the full document. The two differ, so
// they are cached as separate variants — serving one where the other was
// asked for breaks `npm view` and wastes bandwidth respectively.
const abbreviatedAccept = "application/vnd.npm.install-v1+json"

// defaultAllowedHosts is the SSRF fence for the artifact route: the hosts
// this proxy will fetch tarballs from unless an operator widens it.
var defaultAllowedHosts = pkgcache.HostAllowlist{
	"registry.npmjs.org",
	"npmjs.org",
	"npmjs.com",
}

// npmNameRe matches the package names the registry permits, scope included.
// Anything else is rejected rather than sanitised — a name we do not
// recognise must never reach the filesystem.
var npmNameRe = regexp.MustCompile(`^(@[A-Za-z0-9][A-Za-z0-9._~-]{0,127}/)?[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// tarballFileRe matches a tarball filename. Deliberately narrow.
var tarballFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~+-]{0,255}\.tgz$`)

// Config for the npm caching proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/npm).
	CacheDir string
	// Upstream is the registry to pull through to.
	Upstream string
	// ListenAddr is the address to bind and advertise (e.g. "10.88.0.1:8084").
	ListenAddr string
	// PackumentTTL is the revalidation interval for packuments. Zero takes
	// DefaultPackumentTTL; negative means "revalidate every request".
	PackumentTTL time.Duration
	// MaxBytes is the cache disk budget. Zero takes pkgcache.DefaultMaxBytes.
	MaxBytes int64
	// AllowedHosts extends the artifact-fetch allowlist. The upstream's own
	// host is always allowed.
	AllowedHosts []string
	// Cleanup wipes the cache on Stop. Default false.
	Cleanup bool
	Log     *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is the npm registry caching proxy.
type Proxy struct {
	cfg   Config
	srv   *pkgcache.Server
	allow pkgcache.HostAllowlist
}

// New creates an npm caching proxy. Call Start() to begin serving.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.PackumentTTL == 0 {
		cfg.PackumentTTL = DefaultPackumentTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	allow := append(pkgcache.HostAllowlist{}, defaultAllowedHosts...)
	allow = append(allow, cfg.AllowedHosts...)
	p := &Proxy{cfg: cfg, allow: allow.WithHostsOf(cfg.Upstream)}
	srv, err := pkgcache.NewServer(pkgcache.ServerConfig{
		Name:       "npm",
		ListenAddr: cfg.ListenAddr,
		CacheDir:   cfg.CacheDir,
		MaxBytes:   cfg.MaxBytes,
		Cleanup:    cfg.Cleanup,
		Log:        cfg.Log,
	}, func(_ *pkgcache.Cache, _ *pkgcache.Fetcher) http.Handler {
		return http.HandlerFunc(p.handle)
	})
	if err != nil {
		return nil, err
	}
	p.srv = srv
	return p, nil
}

// Start begins serving. Returns after the listener is bound.
func (p *Proxy) Start() error { return p.srv.Start() }

// Stop shuts the proxy down.
func (p *Proxy) Stop() error { return p.srv.Stop() }

// Addr returns the bound address.
func (p *Proxy) Addr() string { return p.srv.Addr() }

// Name returns the proxy name for logging.
func (p *Proxy) Name() string { return "npm" }

// Healthy reports whether the proxy answers its own health probe.
func (p *Proxy) Healthy() bool { return p.srv.Healthy() }

// EnvVars returns the environment variables to inject into job containers.
//
//   - npm_config_registry is npm's own config channel and needs no .npmrc.
//     pnpm reads it too, and Yarn Classic (v1) picks it up through its npm
//     config compatibility layer.
//   - YARN_NPM_REGISTRY_SERVER is what Yarn Berry (v2+) reads instead; it
//     ignores npm_config_* entirely.
//
// NOT covered, and documented as such: a repo-committed .npmrc or
// .yarnrc.yml with an explicit `registry`, which wins over the environment;
// private/scoped registries with auth (this proxy forwards no credentials);
// and Yarn Berry's offline mirror / zero-installs, which bypasses the
// network altogether — all of those simply keep working, uncached.
func (p *Proxy) EnvVars() []string {
	base := p.srv.AdvertiseBase() + "/"
	return []string{
		"npm_config_registry=" + base,
		"YARN_NPM_REGISTRY_SERVER=" + base,
	}
}

// --- routing ---------------------------------------------------------------

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, pkgcache.ArtifactRoute+"/") {
		p.srv.ServeArtifactRoute(w, r, p.allow, "application/octet-stream")
		return
	}

	// Registry endpoints that are neither metadata nor artifacts: search
	// (/-/v1/search), ping (/-/ping), whoami. Relayed, never cached.
	if path == "/-" || strings.HasPrefix(path, "/-/") {
		p.srv.Passthrough(w, r, p.cfg.Upstream+path)
		return
	}

	// The registry root is a service document; some tools probe it.
	if path == "" || path == "/" {
		p.srv.Passthrough(w, r, p.cfg.Upstream+"/")
		return
	}

	pkg, file, ok := parsePath(path)
	if !ok {
		p.srv.Log().Warn("npm proxy rejecting path", "path", path)
		http.NotFound(w, r)
		return
	}
	if file != "" {
		p.serveTarball(w, r, pkg, file)
		return
	}
	p.servePackument(w, r, pkg)
}

// parsePath splits an npm registry path into a package name and, for a
// tarball request, its filename.
//
// The two shapes are "/<pkg>" (or "/@scope/<pkg>") and
// "/<pkg>/-/<file>.tgz". The "-" separator segment is what distinguishes
// them; npm package names cannot be "-", so the split is unambiguous.
//
// Every component is validated against the registry's own naming rules, so
// a hostile "package name" like "../../etc/passwd" is rejected outright
// rather than sanitised — it never reaches a path join at all.
func parsePath(urlPath string) (pkg, file string, ok bool) {
	segs := strings.Split(strings.Trim(urlPath, "/"), "/")
	for _, s := range segs {
		if s == "" {
			return "", "", false
		}
	}

	// Tarball: <pkg…>/-/<file>
	for i, s := range segs {
		if s != "-" {
			continue
		}
		if i == 0 || i != len(segs)-2 {
			return "", "", false
		}
		pkg = strings.Join(segs[:i], "/")
		file = segs[i+1]
		if !npmNameRe.MatchString(pkg) || !tarballFileRe.MatchString(file) {
			return "", "", false
		}
		return pkg, file, true
	}

	// Packument: <pkg> or @scope/<pkg>. A third segment would be a
	// single-version manifest ("/<pkg>/<version>"), which npm only uses for
	// `npm view`; it is not part of the install path and is not cached.
	if len(segs) > 2 {
		return "", "", false
	}
	pkg = strings.Join(segs, "/")
	if !npmNameRe.MatchString(pkg) {
		return "", "", false
	}
	if len(segs) == 2 && !strings.HasPrefix(segs[0], "@") {
		return "", "", false
	}
	if len(segs) == 1 && strings.HasPrefix(segs[0], "@") {
		return "", "", false // a bare scope is not a package
	}
	return pkg, "", true
}

// serveTarball serves an immutable package tarball at its canonical
// registry path. Most requests arrive on the artifact route (the rewritten
// dist.tarball), but a lockfile with a pre-rewritten `resolved` URL, or a
// tool that reconstructs the conventional path, lands here — so it is
// cached identically rather than being a hole in the cache.
func (p *Proxy) serveTarball(w http.ResponseWriter, r *http.Request, pkg, file string) {
	upstream := p.cfg.Upstream + "/" + pkg + "/-/" + file
	err := p.srv.Fetcher().ServeArtifact(w, r, pkgcache.Request{
		Key:                "tarball/" + pkg + "/" + file,
		URL:                upstream,
		Immutable:          true,
		DefaultContentType: "application/octet-stream",
	})
	if err == nil {
		return
	}
	if pkgcache.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	// Fail open: npm follows redirects, so a dead cache costs a direct
	// (uncached) download rather than a failed install.
	p.srv.RedirectUpstream(w, r, upstream)
}

// servePackument serves a rewritten packument.
func (p *Proxy) servePackument(w http.ResponseWriter, r *http.Request, pkg string) {
	accept := r.Header.Get("Accept")
	variant := "full"
	if strings.Contains(accept, abbreviatedAccept) {
		variant = "abbrev"
		accept = abbreviatedAccept
	} else {
		accept = "application/json"
	}

	upstream := p.cfg.Upstream + "/" + escapePackage(pkg)
	body, _, err := p.srv.Fetcher().Document(r.Context(), pkgcache.Request{
		Key:                "packument/" + variant + "/" + pkg,
		URL:                upstream,
		TTL:                p.cfg.PackumentTTL,
		Accept:             accept,
		DefaultContentType: "application/json",
	})
	if err != nil {
		if pkgcache.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// Fail open: the origin packument still resolves, its tarball URLs
		// just point at the CDN instead of at us.
		p.srv.RedirectUpstream(w, r, upstream)
		return
	}

	rewritten, err := rewritePackument(body, p.srv.AdvertiseBase(), p.allow)
	if err != nil {
		// Unparseable metadata: serve it through unchanged rather than
		// failing. npm then downloads tarballs from the origin.
		p.srv.Log().Warn("serving npm packument unrewritten", "package", pkg, "error", err)
		rewritten = body
	}
	pkgcache.WriteDocument(w, r, rewritten, "application/json")
}

// escapePackage renders a package name for an upstream URL. A scoped name
// travels as "@scope%2fname" on the wire; net/url would otherwise re-encode
// the "@" and leave the slash as a path separator, which registries accept
// but mirrors do not always.
func escapePackage(pkg string) string {
	return strings.Replace(pkg, "/", "%2f", 1)
}

// rewritePackument redirects every dist.tarball URL in a packument at this
// proxy.
//
// Decoded into a generic map so that unknown fields survive verbatim: npm
// packuments carry registry-specific keys, and dropping one silently would
// be far worse than not caching at all. Tarball URLs on hosts outside the
// allowlist are deliberately left ALONE — the client fetches those directly
// (uncached), which is the right failure mode for a private or third-party
// registry we are not permitted to relay.
func rewritePackument(raw []byte, base string, allow pkgcache.HostAllowlist) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	rewriteDist(doc, base, allow)
	if versions, ok := doc["versions"].(map[string]any); ok {
		for _, v := range versions {
			if vm, ok := v.(map[string]any); ok {
				rewriteDist(vm, base, allow)
			}
		}
	}
	return json.Marshal(doc)
}

// rewriteDist rewrites the "dist.tarball" of one version object in place.
func rewriteDist(version map[string]any, base string, allow pkgcache.HostAllowlist) {
	dist, ok := version["dist"].(map[string]any)
	if !ok {
		return
	}
	tarball, ok := dist["tarball"].(string)
	if !ok || tarball == "" {
		return
	}
	if !allow.Allows(tarball) {
		return
	}
	dist["tarball"] = pkgcache.ArtifactURL(base, tarball)
}
