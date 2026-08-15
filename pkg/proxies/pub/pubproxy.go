// Package pubproxy implements proxies.CacheProxy for the Dart/Flutter
// ecosystem (pub.dev).
//
// It is a pull-through cache for the two request shapes `dart pub get` and
// `flutter pub get` make:
//
//	GET /api/packages/<name>     the version listing   — MUTABLE
//	GET /api/archives/<file>     a package archive     — IMMUTABLE
//
// # WHAT IS CACHED AND WHY
//
// Archives are the bytes. A published (package, version) archive on pub.dev
// is immutable — the version listing ships an archive_sha256 for each one
// and pub verifies it — so archives are cached permanently and never
// revalidated.
//
// The version listing is not immutable: it grows on every publish and its
// "latest" moves. It is cached with a short TTL (default 5 minutes) and then
// revalidated with a conditional GET, so a package published minutes ago is
// resolvable and an unchanged listing costs one 304.
//
// # WHY THE LISTING IS REWRITTEN
//
// Each entry in a version listing carries an ABSOLUTE archive_url. Left
// alone, pub would take the listing from the cache and the archives from
// pub.dev. archive_url is therefore rewritten to this proxy's artifact
// route; archive_sha256 is untouched and still checked by pub against the
// bytes we serve.
//
// PUBLISHING IS NOT PROXIED. The publish flow starts at
// /api/packages/versions/new and continues with a multipart upload; this
// proxy serves GET and HEAD only, and its routing does not recognise that
// path at all. Publish from CI against the real pub.dev.
package pubproxy

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

const (
	// DefaultUpstream is the public pub.dev registry.
	DefaultUpstream = "https://pub.dev"
	// DefaultListingTTL is how long a cached version listing is served
	// before revalidation.
	DefaultListingTTL = 5 * time.Minute

	packagesRoute = "/api/packages"
	archivesRoute = "/api/archives"
)

// defaultAllowedHosts is the SSRF fence for the artifact route. pub.dev has
// served archive_url from its own domain and from Google Cloud Storage at
// different times, so both are allowed.
var defaultAllowedHosts = pkgcache.HostAllowlist{
	"pub.dev",
	"pub.dartlang.org",
	"storage.googleapis.com",
}

// pubNameRe matches the package names pub.dev permits: a Dart identifier,
// lowercase with underscores.
var pubNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// archiveFileRe matches an archive filename ("<name>-<version>.tar.gz").
var archiveFileRe = regexp.MustCompile(`^[a-z][A-Za-z0-9._~+-]{0,255}\.tar\.gz$`)

// Config for the pub caching proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/pub).
	CacheDir string
	// Upstream is the pub server to pull through to.
	Upstream string
	// ListenAddr is the address to bind and advertise.
	ListenAddr string
	// ListingTTL is the revalidation interval for version listings. Zero
	// takes DefaultListingTTL; negative means "revalidate every request".
	ListingTTL time.Duration
	// MaxBytes is the cache disk budget.
	MaxBytes int64
	// AllowedHosts extends the artifact-fetch allowlist.
	AllowedHosts []string
	// Cleanup wipes the cache on Stop. Default false.
	Cleanup bool
	Log     *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is the pub.dev caching proxy.
type Proxy struct {
	cfg   Config
	srv   *pkgcache.Server
	allow pkgcache.HostAllowlist
}

// New creates a pub caching proxy. Call Start() to begin serving.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.ListingTTL == 0 {
		cfg.ListingTTL = DefaultListingTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	allow := append(pkgcache.HostAllowlist{}, defaultAllowedHosts...)
	allow = append(allow, cfg.AllowedHosts...)
	p := &Proxy{cfg: cfg, allow: allow.WithHostsOf(cfg.Upstream)}

	srv, err := pkgcache.NewServer(pkgcache.ServerConfig{
		Name:       "pub",
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
func (p *Proxy) Name() string { return "pub" }

// Healthy reports whether the proxy answers its own health probe.
func (p *Proxy) Healthy() bool { return p.srv.Healthy() }

// EnvVars returns the environment variables to inject into job containers.
//
// PUB_HOSTED_URL is the documented way to point the pub client at a mirror,
// and both `dart pub` and `flutter pub` honour it. No trailing slash: pub
// concatenates paths onto it directly.
//
// NOT covered, and documented as such: a dependency declared with an
// explicit `hosted:` URL in pubspec.yaml, which wins over the environment;
// git and path dependencies, which never touch a registry; the Flutter SDK's
// own artifact downloads, which follow FLUTTER_STORAGE_BASE_URL rather than
// PUB_HOSTED_URL; and authenticated private pub servers.
func (p *Proxy) EnvVars() []string {
	return []string{
		"PUB_HOSTED_URL=" + p.srv.AdvertiseBase(),
	}
}

// --- routing ---------------------------------------------------------------

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, pkgcache.ArtifactRoute+"/") {
		p.srv.ServeArtifactRoute(w, r, p.allow, "application/octet-stream")
		return
	}

	if name, ok := strings.CutPrefix(path, packagesRoute+"/"); ok && !strings.Contains(strings.Trim(name, "/"), "/") {
		name = strings.Trim(name, "/")
		if !pubNameRe.MatchString(name) {
			p.srv.Log().Warn("pub proxy rejecting package path", "path", path)
			http.NotFound(w, r)
			return
		}
		p.serveListing(w, r, name)
		return
	}

	if file, ok := strings.CutPrefix(path, archivesRoute+"/"); ok {
		if !archiveFileRe.MatchString(file) {
			p.srv.Log().Warn("pub proxy rejecting archive path", "path", path)
			http.NotFound(w, r)
			return
		}
		p.serveArchive(w, r, file)
		return
	}

	// Other read-only API surfaces the client may probe (advisories,
	// package-name validation). Relayed, never cached, so a new endpoint
	// does not become a hard 404 for the job. The publish routes are NOT
	// here: /api/packages/versions/new has two segments after
	// /api/packages/ and so never matches the listing route, and the upload
	// itself is a POST, which the method gate rejects.
	if strings.HasPrefix(path, "/api/") {
		p.srv.Passthrough(w, r, p.cfg.Upstream+path)
		return
	}

	http.NotFound(w, r)
}

// serveListing serves a rewritten version listing.
func (p *Proxy) serveListing(w http.ResponseWriter, r *http.Request, name string) {
	upstream := p.cfg.Upstream + packagesRoute + "/" + name
	body, _, err := p.srv.Fetcher().Document(r.Context(), pkgcache.Request{
		Key:                "listing/" + name,
		URL:                upstream,
		TTL:                p.cfg.ListingTTL,
		Accept:             "application/vnd.pub.v2+json",
		DefaultContentType: "application/json",
	})
	if err != nil {
		if pkgcache.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// Fail open: pub follows the redirect and reads the origin listing,
		// whose archive_url values point at pub.dev.
		p.srv.RedirectUpstream(w, r, upstream)
		return
	}

	rewritten, err := rewriteListing(body, p.srv.AdvertiseBase(), p.allow)
	if err != nil {
		p.srv.Log().Warn("serving pub listing unrewritten", "package", name, "error", err)
		rewritten = body
	}
	pkgcache.WriteDocument(w, r, rewritten, "application/json")
}

// serveArchive serves an immutable package archive at its canonical pub
// path. Most requests arrive on the artifact route (the rewritten
// archive_url); a pubspec.lock that already records the canonical URL, or a
// client that reconstructs it, lands here and is cached identically.
func (p *Proxy) serveArchive(w http.ResponseWriter, r *http.Request, file string) {
	upstream := p.cfg.Upstream + archivesRoute + "/" + file
	err := p.srv.Fetcher().ServeArtifact(w, r, pkgcache.Request{
		Key:                "archive/" + file,
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
	p.srv.RedirectUpstream(w, r, upstream)
}

// rewriteListing redirects every archive_url in a version listing at this
// proxy.
//
// Decoded into a generic map so unknown fields survive verbatim — pub.dev's
// listing carries the full pubspec of every version plus advisory and
// retraction metadata, and dropping any of it would change resolution.
// An archive_url on a host outside the allowlist is left ALONE, so the
// client fetches it directly rather than through a relay we do not trust.
func rewriteListing(raw []byte, base string, allow pkgcache.HostAllowlist) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	if latest, ok := doc["latest"].(map[string]any); ok {
		rewriteArchiveURL(latest, base, allow)
	}
	if versions, ok := doc["versions"].([]any); ok {
		for _, v := range versions {
			if vm, ok := v.(map[string]any); ok {
				rewriteArchiveURL(vm, base, allow)
			}
		}
	}
	return json.Marshal(doc)
}

func rewriteArchiveURL(version map[string]any, base string, allow pkgcache.HostAllowlist) {
	raw, ok := version["archive_url"].(string)
	if !ok || raw == "" {
		return
	}
	if !allow.Allows(raw) {
		return
	}
	version["archive_url"] = pkgcache.ArtifactURL(base, raw)
}
