// Package pipproxy implements proxies.CacheProxy for the Python ecosystem.
//
// It is a pull-through cache for the PEP 503 "simple" index and the
// distribution files that index points at:
//
//	GET /simple/            the root project list        — MUTABLE
//	GET /simple/<project>/  a project's file list        — MUTABLE
//	GET /_ephemerd/dl/…     wheels and sdists            — IMMUTABLE
//
// # WHY THE SIMPLE INDEX RATHER THAN THE JSON API
//
// The simple index is the only surface pip is guaranteed to use, and it is
// the one PIP_INDEX_URL redirects. It also covers uv, Poetry and pip-tools,
// all of which speak PEP 503/691 to whatever index URL they are given.
//
// # WHAT IS CACHED AND WHY
//
// Wheels and sdists are immutable in the strongest sense in any ecosystem:
// PyPI refuses to let a file be re-uploaded even after deletion, the
// filename encodes name + version + ABI tags, and the index publishes a
// sha256 for each one. They are cached permanently, never revalidated, and
// are essentially all of the bytes — a single `torch` wheel is larger than
// most projects' entire metadata history.
//
// Index pages are mutable: a project page gains a row on every release. They
// are cached with a short TTL (default 5 minutes) and then revalidated with
// a conditional GET, so a release published minutes ago is never invisible
// for long and an unchanged page costs one 304.
//
// # WHY PROJECT PAGES ARE REWRITTEN
//
// A project page's links point at files.pythonhosted.org. Left alone, pip
// would take metadata from the cache and every byte from the CDN. Each file
// link on a project page is therefore rewritten to this proxy's artifact
// route, with the "#sha256=…" fragment preserved so pip still verifies what
// it downloads. The ROOT index is deliberately NOT rewritten: its links are
// project pages, not files, and they must keep resolving against this proxy.
package pipproxy

import (
	"encoding/json"
	"html"
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
	// DefaultUpstream is PyPI.
	DefaultUpstream = "https://pypi.org"
	// DefaultIndexTTL is how long a cached index page is served before
	// revalidation.
	DefaultIndexTTL = 5 * time.Minute
	// simpleRoute is the PEP 503 index prefix this proxy serves.
	simpleRoute = "/simple"
	// jsonAccept is the PEP 691 simple-API media type. pip and uv send it
	// when they can take JSON; the JSON and HTML renderings of a page are
	// different documents and are cached as separate variants.
	jsonAccept = "application/vnd.pypi.simple.v1+json"
	// htmlAccept asks upstream for the classic PEP 503 rendering.
	htmlAccept = "text/html, application/vnd.pypi.simple.v1+html;q=0.9"
)

// defaultAllowedHosts is the SSRF fence for the artifact route.
var defaultAllowedHosts = pkgcache.HostAllowlist{
	"pypi.org",
	"files.pythonhosted.org",
	"pythonhosted.org",
}

// projectNameRe matches a PEP 503 project name loosely enough to accept
// un-normalised requests (pip normalises, other tools do not) and strictly
// enough that the value can never reach the filesystem as anything but a
// single plain segment.
var projectNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// hrefRe matches an href attribute in a PEP 503 page. These pages are
// machine-generated and deliberately minimal ("a simple HTML page with
// anchors"), so an attribute-level rewrite is both sufficient and far less
// fragile than reserialising someone else's HTML through a DOM.
var hrefRe = regexp.MustCompile(`(?i)(href\s*=\s*)("[^"]*"|'[^']*')`)

// Config for the pip caching proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/pip).
	CacheDir string
	// Upstream is the index origin. The simple index is expected at
	// <upstream>/simple/.
	Upstream string
	// ListenAddr is the address to bind and advertise.
	ListenAddr string
	// IndexTTL is the revalidation interval for index pages. Zero takes
	// DefaultIndexTTL; negative means "revalidate every request".
	IndexTTL time.Duration
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

// Proxy is the PyPI caching proxy.
type Proxy struct {
	cfg   Config
	srv   *pkgcache.Server
	allow pkgcache.HostAllowlist
}

// New creates a pip caching proxy. Call Start() to begin serving.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.IndexTTL == 0 {
		cfg.IndexTTL = DefaultIndexTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	allow := append(pkgcache.HostAllowlist{}, defaultAllowedHosts...)
	allow = append(allow, cfg.AllowedHosts...)
	p := &Proxy{cfg: cfg, allow: allow.WithHostsOf(cfg.Upstream)}

	srv, err := pkgcache.NewServer(pkgcache.ServerConfig{
		Name:       "pip",
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
func (p *Proxy) Name() string { return "pip" }

// Healthy reports whether the proxy answers its own health probe.
func (p *Proxy) Healthy() bool { return p.srv.Healthy() }

// EnvVars returns the environment variables to inject into job containers.
//
// PIP_INDEX_URL is pip's own config channel — no pip.conf, no CLI flag.
// PIP_TRUSTED_HOST is required because the proxy speaks plain HTTP on a
// private address: without it pip refuses the index as insecure. Both the
// bare host and the host:port form are listed because pip matches
// trusted-host entries against either, depending on version; pip splits
// append-style env values on whitespace, so one variable carries both.
//
// NOT covered, and documented as such: a repo's pip.conf / requirements.txt
// "--index-url" line, which wins over the environment; Poetry's
// pyproject-declared sources; uv, which reads UV_INDEX_URL rather than
// PIP_INDEX_URL; and any authenticated index (this proxy forwards no
// credentials).
func (p *Proxy) EnvVars() []string {
	return []string{
		"PIP_INDEX_URL=" + p.srv.AdvertiseBase() + simpleRoute + "/",
		"PIP_TRUSTED_HOST=" + p.srv.AdvertiseHost() + " " + p.srv.AdvertiseHostPort(),
	}
}

// --- routing ---------------------------------------------------------------

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, pkgcache.ArtifactRoute+"/") {
		p.srv.ServeArtifactRoute(w, r, p.allow, "application/octet-stream")
		return
	}

	if path == simpleRoute || path == simpleRoute+"/" {
		p.serveIndex(w, r, "", "/simple/")
		return
	}

	rest, ok := strings.CutPrefix(path, simpleRoute+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	project := strings.Trim(rest, "/")
	if strings.Contains(project, "/") || !projectNameRe.MatchString(project) {
		p.srv.Log().Warn("pip proxy rejecting project path", "path", path)
		http.NotFound(w, r)
		return
	}
	p.serveIndex(w, r, project, "/simple/"+project+"/")
}

// serveIndex serves the root index (project == "") or a project page.
func (p *Proxy) serveIndex(w http.ResponseWriter, r *http.Request, project, upstreamPath string) {
	wantJSON := strings.Contains(r.Header.Get("Accept"), jsonAccept)
	variant, accept := "html", htmlAccept
	if wantJSON {
		variant, accept = "json", jsonAccept
	}

	key := "simple/" + variant + "/" + project
	if project == "" {
		key = "simple/" + variant + "/_index"
	}
	upstream := p.cfg.Upstream + upstreamPath

	body, meta, err := p.srv.Fetcher().Document(r.Context(), pkgcache.Request{
		Key:                key,
		URL:                upstream,
		TTL:                p.cfg.IndexTTL,
		Accept:             accept,
		DefaultContentType: "text/html; charset=utf-8",
	})
	if err != nil {
		if pkgcache.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// Fail open: pip follows the redirect and reads the origin index,
		// whose file links point at the CDN. Slower, uncached, not broken.
		p.srv.RedirectUpstream(w, r, upstream)
		return
	}

	contentType := meta.ContentType
	if contentType == "" {
		contentType = "text/html; charset=utf-8"
	}
	// The root index lists PROJECTS, not files. Its links must keep
	// resolving against this proxy, so it is served through untouched.
	if project == "" {
		pkgcache.WriteDocument(w, r, body, contentType)
		return
	}

	rewritten, err := rewriteIndex(body, contentType, upstream, p.srv.AdvertiseBase(), p.allow)
	if err != nil {
		p.srv.Log().Warn("serving pip index page unrewritten", "project", project, "error", err)
		rewritten = body
	}
	pkgcache.WriteDocument(w, r, rewritten, contentType)
}

// rewriteIndex points every distribution-file link on a project page at this
// proxy, in whichever of the two PEP renderings upstream returned.
func rewriteIndex(body []byte, contentType, pageURL, base string, allow pkgcache.HostAllowlist) ([]byte, error) {
	page, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	if strings.Contains(contentType, "json") {
		return rewriteSimpleJSON(body, page, base, allow)
	}
	return rewriteSimpleHTML(body, page, base, allow), nil
}

// rewriteSimpleJSON rewrites the PEP 691 rendering.
//
// Decoded into a generic map so unknown keys survive: PEP 691 is versioned
// and gains fields (yanked, core-metadata, provenance), and silently
// dropping one would be worse than not caching.
func rewriteSimpleJSON(body []byte, page *url.URL, base string, allow pkgcache.HostAllowlist) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	files, ok := doc["files"].([]any)
	if !ok {
		return body, nil
	}
	for _, f := range files {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := fm["url"].(string)
		if !ok || raw == "" {
			continue
		}
		if rewritten, ok := rewriteFileURL(raw, page, base, allow); ok {
			fm["url"] = rewritten
		}
	}
	return json.Marshal(doc)
}

// rewriteSimpleHTML rewrites the PEP 503 rendering.
func rewriteSimpleHTML(body []byte, page *url.URL, base string, allow pkgcache.HostAllowlist) []byte {
	return hrefRe.ReplaceAllFunc(body, func(m []byte) []byte {
		groups := hrefRe.FindSubmatch(m)
		if len(groups) != 3 {
			return m
		}
		quoted := string(groups[2])
		quote := quoted[:1]
		raw := html.UnescapeString(quoted[1 : len(quoted)-1])
		rewritten, ok := rewriteFileURL(raw, page, base, allow)
		if !ok {
			return m
		}
		return []byte(string(groups[1]) + quote + html.EscapeString(rewritten) + quote)
	})
}

// rewriteFileURL turns one distribution-file link into an artifact URL on
// this proxy, preserving the "#sha256=…" fragment that pip verifies the
// download against.
//
// A relative link is resolved against the page it came from, because PEP 503
// permits either form and self-hosted indexes (devpi, a static directory
// listing) use relative links. A link on a host outside the allowlist is
// left ALONE so the client fetches it directly — the right outcome for a
// third-party or private file host this proxy has no business relaying.
func rewriteFileURL(raw string, page *url.URL, base string, allow pkgcache.HostAllowlist) (string, bool) {
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	abs := page.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return "", false
	}
	fragment := abs.Fragment
	abs.Fragment = ""
	target := abs.String()
	if !allow.Allows(target) {
		return "", false
	}
	out := pkgcache.ArtifactURL(base, target)
	if fragment != "" {
		out += "#" + fragment
	}
	return out, true
}
