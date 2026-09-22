// Package ghrelproxy implements proxies.CacheProxy for GitHub release
// artifacts: the release metadata a build tool reads, and the asset bytes
// that metadata points at.
//
// # WHY THIS EXISTS
//
// The other proxies in this package cache a package registry. This one caches
// a distribution channel that has no registry: a great deal of CI input
// arrives as "a file attached to a GitHub release" — prebuilt toolchains,
// vendored library tarballs, SDK archives — and every job on a node fetches
// the same bytes over the WAN, every time.
//
// Measured on this fleet (php-sdk, x86_64 gnu, 2026-09-20): 40 source
// artifacts took 87 s of a 591 s build, a large share of them GitHub release
// downloads. Nothing about those bytes changes between jobs.
//
// # THE INTERCEPTION PROBLEM
//
// Go has GOPROXY, Cargo has source replacement, pip has PIP_INDEX_URL. A
// GitHub release download has no such redirect — it is a plain
// https://github.com/... URL baked into whatever tool is fetching it. There
// are only two ways to intercept that: terminate TLS for job containers
// (a CA in every image, and a real change to what untrusted job code is
// exposed to), or have the tool ASK for a proxy.
//
// This proxy takes the second route, in the shape Go already established:
// ephemerd advertises GHREL_PROXY, and a tool that understands it uses it.
// A tool that does not is unaffected, and an operator who leaves the proxy
// off gets upstream — the env var is simply absent, so the usual
//
//	base := getenv("GHREL_PROXY") ?: "https://api.github.com"
//
// falls back on its own. Nothing in a workflow has to change, and a build
// stays correct off-fleet, which a workflow rewritten to hardcode a proxy
// address would not.
//
// # WHAT IS CACHED, AND THE MUTABILITY PROBLEM
//
// Release METADATA is mutable — a release gains assets, a tag can be moved —
// so it is cached with a short TTL and then revalidated with a conditional
// GET, exactly as the npm/pip proxies treat their index documents.
//
// Release ASSETS are where this ecosystem differs from every other proxy
// here, and it is worth being explicit because getting it wrong is silent.
// PyPI REFUSES to let a file be re-uploaded even after deletion; npm and pub
// are similar. That is why those proxies mark artifacts Immutable and never
// look at them again. GitHub enforces nothing of the kind: an asset can be
// deleted and replaced under the same release, keeping its name and URL.
//
// So assets are NOT immutable by default here. They are cached with a TTL
// and revalidated, which costs one conditional request per asset per TTL and
// returns 304 for unchanged bytes — cheap next to re-downloading tens of
// megabytes, and correct even if someone republishes.
//
// ImmutableAssets turns that off for operators whose publishing discipline
// guarantees it (assets are never overwritten; a new build gets a new tag).
// It is faster and it is a promise the proxy cannot verify, which is why it
// is opt-in rather than the default.
package ghrelproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

const (
	// DefaultAPIUpstream is GitHub's REST API origin.
	DefaultAPIUpstream = "https://api.github.com"

	// DefaultDownloadUpstream is where release assets actually live.
	// Separate from the API origin because they are different hosts and an
	// operator pointing at GitHub Enterprise must be able to move both.
	DefaultDownloadUpstream = "https://github.com"

	// DefaultMetadataTTL is how long release metadata is served before a
	// conditional GET. Matches the npm/pip index default: long enough that
	// a burst of jobs costs one upstream request, short enough that an
	// asset published minutes ago is not invisible.
	DefaultMetadataTTL = 5 * time.Minute

	// DefaultAssetTTL is how long an asset is served before revalidation.
	// Much longer than metadata: the bytes behind a published asset almost
	// never change, so this is a backstop against silent replacement rather
	// than an expectation of churn.
	DefaultAssetTTL = 24 * time.Hour

	// DefaultPort continues the sequence npm 8084, pip 8085, pub 8086.
	DefaultPort = 8087

	// apiPrefix is the route carrying GitHub REST API requests.
	apiPrefix = "/repos/"

	// downloadPrefix is the route carrying asset bytes. Deliberately not
	// under /repos/ so the two cannot be confused when rewriting.
	downloadPrefix = "/download/"
)

// defaultAllowedHosts is where asset bytes may be fetched from. GitHub serves
// release assets from its own host and redirects to an S3-backed CDN, so both
// must be permitted or every download fails on the redirect.
var defaultAllowedHosts = pkgcache.HostAllowlist{
	"github.com",
	"api.github.com",
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",
}

// Config configures the proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/ghrel).
	CacheDir string
	// APIUpstream overrides the REST API origin (GitHub Enterprise).
	APIUpstream string
	// DownloadUpstream overrides the asset origin.
	DownloadUpstream string
	// ListenAddr is the address to bind and advertise.
	ListenAddr string
	// MetadataTTL is the revalidation interval for release metadata.
	// Zero takes DefaultMetadataTTL; negative revalidates every request.
	MetadataTTL time.Duration
	// AssetTTL is the revalidation interval for asset bytes. Ignored when
	// ImmutableAssets is set. Zero takes DefaultAssetTTL.
	AssetTTL time.Duration
	// ImmutableAssets caches asset bytes permanently and never revalidates
	// them. Only correct when assets are never overwritten in place; see
	// the package comment.
	ImmutableAssets bool
	// MaxBytes is the cache disk budget.
	MaxBytes int64
	// AllowedHosts extends the asset-fetch allowlist.
	AllowedHosts []string
	// Cleanup wipes the cache on Stop. Default false.
	Cleanup bool
	Log     *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is the GitHub release caching proxy.
type Proxy struct {
	cfg   Config
	srv   *pkgcache.Server
	allow pkgcache.HostAllowlist
}

// New creates a GitHub release caching proxy. Call Start() to begin serving.
func New(cfg Config) (*Proxy, error) {
	if cfg.APIUpstream == "" {
		cfg.APIUpstream = DefaultAPIUpstream
	}
	if cfg.DownloadUpstream == "" {
		cfg.DownloadUpstream = DefaultDownloadUpstream
	}
	cfg.APIUpstream = strings.TrimRight(cfg.APIUpstream, "/")
	cfg.DownloadUpstream = strings.TrimRight(cfg.DownloadUpstream, "/")
	if cfg.MetadataTTL == 0 {
		cfg.MetadataTTL = DefaultMetadataTTL
	}
	if cfg.AssetTTL == 0 {
		cfg.AssetTTL = DefaultAssetTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	allow := append(pkgcache.HostAllowlist{}, defaultAllowedHosts...)
	allow = append(allow, cfg.AllowedHosts...)
	p := &Proxy{
		cfg:   cfg,
		allow: allow.WithHostsOf(cfg.APIUpstream).WithHostsOf(cfg.DownloadUpstream),
	}

	srv, err := pkgcache.NewServer(pkgcache.ServerConfig{
		Name:       "ghrel",
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
func (p *Proxy) Name() string { return "ghrel" }

// Healthy reports whether the proxy is serving.
func (p *Proxy) Healthy() bool { return p.srv.Healthy() }

// EnvVars advertises the proxy to job containers.
//
// GHREL_PROXY is a base URL, not a full endpoint, because a consumer needs
// both routes off it: the API for metadata and /download/ for bytes. A tool
// that does not recognise the variable ignores it and goes upstream, which is
// the entire point of advertising rather than intercepting.
func (p *Proxy) EnvVars() []string {
	return []string{"GHREL_PROXY=" + p.advertiseBase()}
}

// advertiseBase is the base URL containers should use to reach this proxy.
//
// Delegated to the shared server rather than formatting cfg.ListenAddr
// directly: a listen address with port 0 has to be resolved to the port
// actually bound, or the proxy advertises (and rewrites asset URLs to) a
// address nothing can connect to.
func (p *Proxy) advertiseBase() string { return p.srv.AdvertiseBase() }

// handle routes a request to metadata or asset handling.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	// MUST precede the apiPrefix case. The asset-download endpoint lives
	// under /repos/ too but returns BYTES, not JSON -- see isAPIAssetPath.
	case isAPIAssetPath(r.URL.Path):
		p.serveAPIAsset(w, r)
	case strings.HasPrefix(r.URL.Path, apiPrefix):
		p.serveMetadata(w, r)
	case strings.HasPrefix(r.URL.Path, downloadPrefix):
		p.serveAsset(w, r)
	default:
		http.NotFound(w, r)
	}
}

// apiAssetRe matches GitHub's asset-DOWNLOAD endpoint:
//
//	/repos/{owner}/{repo}/releases/assets/{id}
//
// which is the one path under /repos/ that does not return JSON. Asked with
// Accept: application/octet-stream it streams the asset bytes, and that is
// exactly how a client fetches an asset from a private repository, so it
// cannot simply be excluded.
var apiAssetRe = regexp.MustCompile(`^/repos/[^/]+/[^/]+/releases/assets/[^/]+/?$`)

// isAPIAssetPath reports whether a path is the asset-download endpoint.
//
// Getting this wrong is not subtle but IS silent-looking: serveMetadata runs
// every /repos/ response through a JSON rewriter, so a binary body fails to
// parse and the handler answers 502. The client sees a failed download with no
// indication the proxy invented the failure. That took out every php-sdk Linux
// build the day this proxy was first enabled (run 35611830865, "Download
// artifact 'zlib' failed", curl exit 22 after 8 retries) -- zlib is a
// `type: ghrel` artifact, and spc fetches its bytes from this endpoint rather
// than from browser_download_url.
func isAPIAssetPath(path string) bool {
	return apiAssetRe.MatchString(path)
}

// serveAPIAsset streams asset bytes from the API asset endpoint, caching them.
//
// Same treatment as serveAsset -- the only difference is which upstream the
// request goes to, because this path is on the API origin rather than the
// download origin.
func (p *Proxy) serveAPIAsset(w http.ResponseWriter, r *http.Request) {
	upstream := p.cfg.APIUpstream + r.URL.Path
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	// Accept is NOT optional here. GitHub's asset endpoint serves the asset's
	// METADATA as JSON by default and only returns the bytes when asked for
	// application/octet-stream. Omitting it does not fail — it quietly returns
	// a ~1.4 KB JSON document with a 200, which then gets cached under the
	// tarball's key and served to every build. On 2026-09-21 that fed spc a
	// JSON blob in place of zlib-1.3.2.tar.gz; only spc's own sha256 check
	// turned it into a visible failure instead of a corrupt PHP binary.
	//
	// The client's own Accept is deliberately not forwarded. Whatever spc or
	// curl happens to send, this path exists to return bytes, and the cache
	// key does not vary on Accept — so honoring a caller that asked for JSON
	// would poison the same key for everyone else.
	req := pkgcache.Request{
		Key:                "apiasset/" + strings.TrimPrefix(r.URL.Path, "/"),
		URL:                upstream,
		Accept:             "application/octet-stream",
		DefaultContentType: "application/octet-stream",
		// Belt and braces: if upstream answers with JSON anyway (a rate-limit
		// body, an error document, a future API change), refuse it rather than
		// storing it. A wrong-typed response on this path is never the
		// artifact, and caching one is indistinguishable from corruption.
		RejectContentTypes: []string{"application/json"},
	}
	if p.cfg.ImmutableAssets {
		req.Immutable = true
	} else {
		req.TTL = p.cfg.AssetTTL
	}

	if err := p.srv.Fetcher().ServeArtifact(w, r, req); err != nil {
		p.writeFetchError(w, err, "release asset (api endpoint)")
	}
}

// serveMetadata proxies a REST API request and rewrites the asset URLs in the
// response so the caller comes back here for the bytes.
//
// Without the rewrite the caller would take metadata from the cache and every
// byte from GitHub's CDN — the same trap the pip proxy documents for project
// pages, and the reason this proxy is worth having at all.
func (p *Proxy) serveMetadata(w http.ResponseWriter, r *http.Request) {
	upstream := p.cfg.APIUpstream + r.URL.Path
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
		p.writeFetchError(w, err, "release metadata")
		return
	}
	ct := meta.ContentType

	rewritten, err := rewriteAssetURLs(body, p.advertiseBase())
	if err != nil {
		// Serving unrewritten metadata would silently send every byte to
		// the CDN, which looks like success and caches nothing. Fail
		// loudly instead.
		p.cfg.Log.Warn("ghrel: could not rewrite release metadata", "path", r.URL.Path, "error", err)
		http.Error(w, "malformed upstream release metadata", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rewritten)
	}
}

// serveAsset serves release asset bytes, caching them.
//
// The route is /download/<owner>/<repo>/releases/download/<tag>/<file>, which
// is GitHub's own asset path with the host swapped, so rewriting is a prefix
// substitution and the shape stays recognisable in logs.
func (p *Proxy) serveAsset(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, downloadPrefix)
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	upstream := p.cfg.DownloadUpstream + "/" + rest

	req := pkgcache.Request{
		Key:                "asset/" + rest,
		URL:                upstream,
		DefaultContentType: "application/octet-stream",
	}
	if p.cfg.ImmutableAssets {
		req.Immutable = true
	} else {
		req.TTL = p.cfg.AssetTTL
	}

	// ServeArtifact STREAMS to the response rather than returning bytes.
	// That matters here more than for the other proxies: release assets are
	// routinely tens or hundreds of megabytes (prebuilt toolchains, SDK
	// archives), and buffering one per concurrent job would put that much
	// per job into ephemerd's heap on a node already running builds.
	if err := p.srv.Fetcher().ServeArtifact(w, r, req); err != nil {
		p.writeFetchError(w, err, "release asset")
	}
}

// writeFetchError maps a fetch failure onto a status code, keeping a real
// upstream 404 distinguishable from an outage — a nonexistent release is an
// answer the job should see, an unreachable GitHub is what fail-open exists
// for.
func (p *Proxy) writeFetchError(w http.ResponseWriter, err error, what string) {
	if pkgcache.IsNotFound(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p.cfg.Log.Warn("ghrel: upstream fetch failed", "what", what, "error", err)
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// rewriteAssetURLs rewrites every browser_download_url in a release document
// so it points at this proxy.
//
// Operates on the decoded JSON rather than by string substitution: the API
// returns either a single release object or an array of them depending on the
// endpoint, and a blind replace would also rewrite unrelated fields (an
// asset's name, a release body quoting a URL) that must be left alone.
func rewriteAssetURLs(body []byte, base string) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode release metadata: %w", err)
	}
	rewriteNode(doc, base)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode release metadata: %w", err)
	}
	return out, nil
}

// rewriteNode walks the decoded document rewriting browser_download_url.
func rewriteNode(node any, base string) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "browser_download_url" {
				if s, ok := val.(string); ok {
					if nu, ok := proxifyAssetURL(s, base); ok {
						v[k] = nu
					}
				}
				continue
			}
			rewriteNode(val, base)
		}
	case []any:
		for _, item := range v {
			rewriteNode(item, base)
		}
	}
}

// proxifyAssetURL turns a GitHub asset URL into this proxy's /download/ route.
// Anything that is not a recognisable asset URL is left untouched rather than
// guessed at.
func proxifyAssetURL(raw, base string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return "", false
	}
	return strings.TrimRight(base, "/") + downloadPrefix + path, true
}

// metaKey is the cache key for one release-metadata document. Path AND query
// are part of the identity: /repos/o/r/releases?page=1 and ?page=2 are
// different documents and must not share an entry.
//
// Hashed rather than path-shaped, for the same reason pkgcache.ArtifactKey is
// — a URL is not safe to map onto a filesystem — and here for two concrete
// reasons, both of which fail SILENTLY (Cache.Write warns, the document is
// still served, and the proxy simply caches nothing from then on):
//
//   - A query string carries "?" and "&", which pkgcache.SafeSegments rejects
//     outright. Paginated release listings are exactly the request a busy
//     node repeats.
//   - The REST API's document paths NEST: /repos/o/r/releases is a document
//     and so is /repos/o/r/releases/latest. A path-shaped key needs
//     "releases" to be a file and a directory at once, and whichever is
//     requested second loses.
func metaKey(path, rawQuery string) string {
	sum := sha256.Sum256([]byte(path + "?" + rawQuery))
	h := hex.EncodeToString(sum[:])
	// Sharded one level so a node that talks to thousands of repos does not
	// end up with one very wide directory.
	return "meta/" + h[0:2] + "/" + h
}
