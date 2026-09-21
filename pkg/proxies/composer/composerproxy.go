// Package composerproxy implements proxies.CacheProxy for PHP Composer.
//
// It is a pull-through cache for the Packagist metadata repository and the
// distribution archives that metadata points at.
//
// # INTERCEPTION
//
// Composer honours COMPOSER_REPO_PACKAGIST, so pointing every job container at
// this proxy is a single env var — no workflow change, and a build off-fleet
// keeps working because the variable is simply absent.
//
// # WHAT IS CACHED
//
// Metadata (packages.json and the per-package p2/<vendor>/<name>.json files)
// is MUTABLE: a package gains a version on every release. It is cached with a
// short TTL and then revalidated with a conditional GET, so a release
// published minutes ago is never invisible for long and an unchanged document
// costs one 304 — the same treatment the npm and pip proxies give their index
// documents.
//
// Distribution archives are IMMUTABLE and are the bytes that actually matter.
// A Packagist dist URL names an exact commit:
//
//	https://api.github.com/repos/<vendor>/<pkg>/zipball/<40-char sha>
//
// A given SHA cannot come back with different contents, so archives are cached
// permanently and never revalidated.
//
// # WHY DIST URLS ARE REWRITTEN
//
// This is the difference between a cache that saves seconds and one that saves
// the download. The previous implementation of this proxy cached metadata only
// and said so plainly: "distribution zips come from mirrors and GitHub, which
// this proxy does not front". That leaves Composer taking a few kilobytes of
// JSON from the LAN and every megabyte of actual code from the WAN, on every
// job.
//
// So each dist URL in a metadata document is rewritten to this proxy's archive
// route, exactly as the pip proxy rewrites the file links on a project page.
// Everything else in the document is left alone: rewriting is done on decoded
// JSON rather than by string replacement, because a blind substitution would
// also corrupt homepage/source/support fields that merely look like URLs.
package composerproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

const (
	// DefaultUpstream is the Packagist metadata repository.
	DefaultUpstream = "https://repo.packagist.org"

	// DefaultMetadataTTL is how long a metadata document is served before it
	// is revalidated. Matches the npm/pip index default.
	DefaultMetadataTTL = 5 * time.Minute

	// DefaultPort continues the sequence npm 8084, pip 8085, pub 8086,
	// ghrel 8087.
	DefaultPort = 8088

	// distPrefix is the route carrying distribution archive bytes. It is
	// deliberately a path Packagist itself never serves, so a rewritten URL
	// can never collide with a real metadata path.
	distPrefix = "/__dist/"
)

// defaultAllowedHosts is where archive bytes may be fetched from. Packagist
// dist URLs point at GitHub's API, which redirects to codeload, and GitLab and
// Bitbucket for packages hosted there.
var defaultAllowedHosts = pkgcache.HostAllowlist{
	"repo.packagist.org",
	"api.github.com",
	"codeload.github.com",
	"github.com",
	"objects.githubusercontent.com",
	"gitlab.com",
	"bitbucket.org",
}

// Config configures the proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/composer).
	CacheDir string
	// Upstream overrides the Packagist metadata repository.
	Upstream string
	// ListenAddr is the address to bind and advertise.
	ListenAddr string
	// MetadataTTL is the revalidation interval for metadata documents.
	// Zero takes DefaultMetadataTTL; negative revalidates every request.
	MetadataTTL time.Duration
	// MaxBytes is the cache disk budget.
	MaxBytes int64
	// AllowedHosts extends the archive-fetch allowlist.
	AllowedHosts []string
	// Cleanup wipes the cache on Stop. Default false.
	Cleanup bool
	Log     *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is the Packagist caching proxy.
type Proxy struct {
	cfg   Config
	srv   *pkgcache.Server
	allow pkgcache.HostAllowlist
}

// New creates a Composer caching proxy. Call Start() to begin serving.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.MetadataTTL == 0 {
		cfg.MetadataTTL = DefaultMetadataTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	allow := append(pkgcache.HostAllowlist{}, defaultAllowedHosts...)
	allow = append(allow, cfg.AllowedHosts...)
	p := &Proxy{cfg: cfg, allow: allow.WithHostsOf(cfg.Upstream)}

	srv, err := pkgcache.NewServer(pkgcache.ServerConfig{
		Name:       "composer",
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

// Name returns the proxy name for logs.
func (p *Proxy) Name() string { return "composer" }

// Healthy reports whether the proxy is serving.
func (p *Proxy) Healthy() bool { return p.srv.Healthy() }

// EnvVars points Composer at this proxy.
func (p *Proxy) EnvVars() []string {
	return []string{"COMPOSER_REPO_PACKAGIST=" + p.advertiseBase()}
}

// advertiseBase is the base URL containers should use to reach this proxy.
//
// Delegated to the shared server rather than formatting cfg.ListenAddr
// directly: a listen address with port 0 has to be resolved to the port
// actually bound, or the proxy advertises (and rewrites dist URLs to) an
// address nothing can connect to.
func (p *Proxy) advertiseBase() string { return p.srv.AdvertiseBase() }

// handle routes a request to archive or metadata handling.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.URL.Path, distPrefix) {
		p.serveDist(w, r)
		return
	}
	p.serveMetadata(w, r)
}

// serveMetadata proxies a Packagist metadata document and rewrites the dist
// URLs inside it so Composer comes back here for the archives.
func (p *Proxy) serveMetadata(w http.ResponseWriter, r *http.Request) {
	upstream := p.cfg.Upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	body, meta, err := p.srv.Fetcher().Document(r.Context(), pkgcache.Request{
		Key:                metaKey(r.URL.Path, r.URL.RawQuery),
		URL:                upstream,
		TTL:                p.cfg.MetadataTTL,
		Accept:             r.Header.Get("Accept"),
		DefaultContentType: "application/json",
	})
	if err != nil {
		p.writeFetchError(w, err, "packagist metadata")
		return
	}

	rewritten, err := rewriteDistURLs(body, p.advertiseBase(), p.allow)
	if err != nil {
		// Serving the document unrewritten would silently send every
		// archive to the WAN while looking like a cache hit. Fail loudly.
		p.cfg.Log.Warn("composer: could not rewrite metadata", "path", r.URL.Path, "error", err)
		http.Error(w, "malformed upstream metadata", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rewritten)
	}
}

// serveDist serves a distribution archive, caching it permanently.
//
// The upstream URL is carried in the "u" query parameter rather than encoded
// into the path: dist URLs span several hosts (GitHub, GitLab, Bitbucket) with
// different path shapes, and reconstructing one from a flattened path would be
// guesswork. The allowlist is re-checked HERE, not just at rewrite time, so a
// job cannot hand this route an arbitrary URL and use ephemerd as an open
// proxy.
func (p *Proxy) serveDist(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" {
		http.Error(w, "missing upstream", http.StatusBadRequest)
		return
	}
	if _, err := url.Parse(raw); err != nil || !p.allow.Allows(raw) {
		http.Error(w, "upstream not permitted", http.StatusForbidden)
		return
	}

	// Immutable: a Packagist dist URL names an exact commit, so the bytes
	// behind it cannot change.
	//
	// Keyed by ArtifactKey (a hash of the URL) rather than by host+path: a
	// host:port — a self-hosted Packagist or a GitLab on a nonstandard port —
	// contains a colon, which SafeSegments rejects, and the failure mode is
	// silent. ServeArtifact logs a warning and streams the archive UNCACHED,
	// so the proxy keeps working while quietly caching nothing.
	if err := p.srv.Fetcher().ServeArtifact(w, r, pkgcache.Request{
		Key:                pkgcache.ArtifactKey(raw),
		URL:                raw,
		Immutable:          true,
		DefaultContentType: "application/zip",
	}); err != nil {
		p.writeFetchError(w, err, "distribution archive")
	}
}

// writeFetchError maps a fetch failure onto a status code, keeping a genuine
// upstream 404 distinguishable from an outage.
func (p *Proxy) writeFetchError(w http.ResponseWriter, err error, what string) {
	if pkgcache.IsNotFound(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p.cfg.Log.Warn("composer: upstream fetch failed", "what", what, "error", err)
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// rewriteDistURLs rewrites every dist.url in a metadata document to this
// proxy's archive route.
//
// Works on decoded JSON, not string substitution: a package document also
// carries homepage, source.url and support links, and rewriting those would
// corrupt metadata Composer shows to users and may act on.
func rewriteDistURLs(body []byte, base string, allow pkgcache.HostAllowlist) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode packagist metadata: %w", err)
	}
	rewriteNode(doc, base, allow)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode packagist metadata: %w", err)
	}
	return out, nil
}

// rewriteNode walks the document rewriting the "url" inside any "dist" object.
func rewriteNode(node any, base string, allow pkgcache.HostAllowlist) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "dist" {
				if dist, ok := val.(map[string]any); ok {
					rewriteDist(dist, base, allow)
					continue
				}
			}
			rewriteNode(val, base, allow)
		}
	case []any:
		for _, item := range v {
			rewriteNode(item, base, allow)
		}
	}
}

// rewriteDist points one dist object's url at the proxy.
func rewriteDist(dist map[string]any, base string, allow pkgcache.HostAllowlist) {
	raw, ok := dist["url"].(string)
	if !ok || raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !allow.Allows(raw) {
		// Leave anything unrecognised pointing upstream. A package served
		// from a host we do not front must still install.
		return
	}
	dist["url"] = strings.TrimRight(base, "/") + distPrefix + "archive?u=" + url.QueryEscape(raw)
}

// metaKey is the cache key for one metadata document. Path AND query are part
// of the identity, so two documents at the same path with different queries
// do not share an entry.
//
// Hashed rather than path-shaped, for the same reason pkgcache.ArtifactKey is:
// a URL is not safe to map onto a filesystem. A query string carries "?" and
// "&", which pkgcache.SafeSegments rejects outright, and Packagist's document
// paths nest (packages.json alongside p2/<vendor>/<name>.json), so a
// path-shaped key can need the same name to be a file and a directory at
// once. Both failures are SILENT — Cache.Write warns, the document is still
// served, and the cache quietly stops working.
func metaKey(path, rawQuery string) string {
	sum := sha256.Sum256([]byte(path + "?" + rawQuery))
	h := hex.EncodeToString(sum[:])
	// Sharded one level to keep the directory from growing very wide.
	return "meta/" + h[0:2] + "/" + h
}
