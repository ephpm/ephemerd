// Package cargoproxy implements proxies.CacheProxy for the Rust ecosystem.
//
// It is a pull-through cache for three upstreams that a Rust CI job hits on
// every run:
//
//	/index/…              the crates.io SPARSE registry index (mutable)
//	/crates/{c}/{v}/…     .crate tarballs (immutable, content-addressed)
//	/rustup/…             rustup toolchain artifacts (dated ones immutable)
//
// ephemerd runs one shared instance on the bridge gateway IP so every job
// container can reach it. Jobs only ever issue HTTP GETs; they have no write
// access to the cache.
//
// Why both a mount and env vars: rustup takes its mirror from the
// environment (RUSTUP_DIST_SERVER), but Cargo does NOT — source replacement
// is only honoured from a config file, so the proxy generates a
// .cargo/config.toml on the host and declares it as a read-only mount at the
// container's filesystem root, where Cargo's ancestor-directory config search
// always finds it. See containerConfigTOML/containerConfigDest.
package cargoproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

// hostGOOS is the OS this daemon runs on. Job containers share the host's
// OS (Linux jobs on Windows/macOS hosts run inside a Linux VM that has its
// own ephemerd), so it doubles as the default container OS.
const hostGOOS = goruntime.GOOS

// Route prefixes. These are part of the contract with the generated
// container config (containerConfigTOML) and with the rewritten config.json.
const (
	indexRoute  = "/index"
	cratesRoute = "/crates"
	rustupRoute = "/rustup"
	healthRoute = "/healthz"
)

// Defaults. Kept here rather than in the config package so the proxy is
// usable standalone (and testable) without a config file.
const (
	// DefaultIndexUpstream is the crates.io sparse index.
	DefaultIndexUpstream = "https://index.crates.io"
	// DefaultRustupUpstream is the rustup/toolchain distribution server.
	DefaultRustupUpstream = "https://static.rust-lang.org"
	// DefaultIndexTTL is how long a cached sparse-index entry is served
	// without contacting upstream. Short by design: the index is mutable
	// and a new dependency version must not take an hour to become
	// visible. Revalidation after the TTL is a conditional GET, so the
	// steady-state cost is a 304, not a re-download.
	DefaultIndexTTL = 10 * time.Minute
	// defaultTimeout bounds a single upstream request.
	defaultTimeout = 120 * time.Second
)

// Config for the Cargo caching proxy.
type Config struct {
	// CacheDir is the on-disk cache root (e.g. <data>/cache/cargo).
	CacheDir string
	// ConfDir is where the container-side Cargo config is generated. It is
	// deliberately OUTSIDE CacheDir so `ephemerd cache clear cargo` cannot
	// pull the mounted config out from under a running job.
	ConfDir string
	// IndexUpstream is the sparse registry index base URL.
	IndexUpstream string
	// RustupUpstream is the rustup distribution server base URL.
	RustupUpstream string
	// ListenAddr is the address to bind (e.g. "10.88.0.1:8083"). It is also
	// the address advertised to containers — see advertiseBase.
	ListenAddr string
	// IndexTTL is the revalidation interval for mutable index entries.
	// Zero means "unset" and takes DefaultIndexTTL; a NEGATIVE value means
	// "revalidate on every request" (useful for tests and for operators who
	// want the index never served without a conditional GET).
	IndexTTL time.Duration
	// Cleanup wipes the cache dir on Stop. Defaults to false: a
	// pull-through cache that is emptied on every restart saves nothing.
	Cleanup bool
	// ContainerOS is the OS of the job containers this proxy serves, used
	// only to pick the mount destination. Defaults to the host GOOS.
	ContainerOS string
	Log         *slog.Logger
}

// Compile-time interface checks.
var (
	_ proxies.CacheProxy    = (*Proxy)(nil)
	_ proxies.MountProvider = (*Proxy)(nil)
)

// Proxy is a caching proxy for the Cargo registry and rustup distribution.
type Proxy struct {
	cfg      Config
	server   *http.Server
	listener net.Listener
	client   *http.Client
	inflight sync.Map // per-path mutex: collapses duplicate upstream fetches

	mu sync.RWMutex
	// dlTemplate is the upstream registry's "dl" template, learned from
	// config.json. Guarded because index requests (writers) and crate
	// downloads (readers) run concurrently.
	dlTemplate string
}

// New creates a Cargo caching proxy. Call Start() to begin serving.
func New(cfg Config) *Proxy {
	if cfg.IndexUpstream == "" {
		cfg.IndexUpstream = DefaultIndexUpstream
	}
	if cfg.RustupUpstream == "" {
		cfg.RustupUpstream = DefaultRustupUpstream
	}
	if cfg.IndexTTL == 0 {
		cfg.IndexTTL = DefaultIndexTTL
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.IndexUpstream = strings.TrimRight(cfg.IndexUpstream, "/")
	cfg.RustupUpstream = strings.TrimRight(cfg.RustupUpstream, "/")

	return &Proxy{
		cfg:        cfg,
		client:     &http.Client{Timeout: defaultTimeout},
		dlTemplate: defaultDLTemplate,
	}
}

// Start begins serving the proxy. Returns after the listener is bound and
// the container-side Cargo config has been written.
func (p *Proxy) Start() error {
	if err := os.MkdirAll(p.cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cargo cache dir: %w", err)
	}
	if err := p.writeContainerConfig(); err != nil {
		return fmt.Errorf("writing container cargo config: %w", err)
	}
	// Recover the upstream "dl" template from a previous run so the first
	// crate download after a restart does not have to guess.
	p.loadDLTemplateFromCache()

	// proxies.Listen, not net.Listen: the bridge gateway IP is not assigned
	// to any interface until CNI creates the bridge for the first job.
	ln, err := proxies.Listen(p.cfg.ListenAddr, p.cfg.Log)
	if err != nil {
		return err
	}
	p.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)
	p.server = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}

	go func() {
		if err := p.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.cfg.Log.Error("cargo proxy server error", "error", err)
		}
	}()

	p.cfg.Log.Info("cargo proxy started",
		"addr", ln.Addr().String(),
		"advertised", p.advertiseBase(),
		"cache", p.cfg.CacheDir,
		"index_upstream", p.cfg.IndexUpstream,
		"rustup_upstream", p.cfg.RustupUpstream,
		"index_ttl", p.cfg.IndexTTL)
	return nil
}

// Stop shuts down the proxy and optionally wipes the cache.
func (p *Proxy) Stop() error {
	var errs []error

	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutting down cargo proxy: %w", err))
		}
	}

	if p.cfg.Cleanup {
		p.cfg.Log.Info("cleaning up cargo cache", "dir", p.cfg.CacheDir)
		if err := os.RemoveAll(p.cfg.CacheDir); err != nil {
			errs = append(errs, fmt.Errorf("cleaning cargo cache: %w", err))
		}
	}

	return errors.Join(errs...)
}

// Addr returns the address the proxy is listening on.
func (p *Proxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.cfg.ListenAddr
}

// advertiseBase is the base URL containers use to reach this proxy. It is
// built from the CONFIGURED address, not the bound one: after a wildcard
// fallback the bound address is "[::]:8083", which means nothing inside a
// container.
func (p *Proxy) advertiseBase() string {
	addr := p.cfg.ListenAddr
	if addr == "" {
		addr = p.Addr()
	}
	return "http://" + addr
}

// EnvVars returns environment variables to inject into job containers.
//
// Only rustup is configured here. Cargo's registry redirect cannot be done
// with environment variables (see Mounts).
func (p *Proxy) EnvVars() []string {
	return []string{
		"RUSTUP_DIST_SERVER=" + p.advertiseBase() + rustupRoute,
	}
}

// Mounts returns the read-only bind mount that carries the generated Cargo
// config into job containers.
func (p *Proxy) Mounts() []proxies.Mount {
	return []proxies.Mount{{
		Source:      p.hostConfigDir(),
		Destination: containerConfigDest(p.containerOS()),
		ReadOnly:    true,
	}}
}

// Name returns the proxy name for logging.
func (p *Proxy) Name() string { return "cargo" }

func (p *Proxy) containerOS() string {
	if p.cfg.ContainerOS != "" {
		return p.cfg.ContainerOS
	}
	return hostGOOS
}

// hostConfigDir is the host-side directory that is mounted as the
// container's ".cargo" directory.
func (p *Proxy) hostConfigDir() string {
	return filepath.Join(p.cfg.ConfDir, ".cargo")
}

// writeContainerConfig regenerates the container-side Cargo config. Written
// atomically so a job that starts mid-write never sees a truncated file.
func (p *Proxy) writeContainerConfig() error {
	dir := p.hostConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cargo conf dir %q: %w", dir, err)
	}
	target := filepath.Join(dir, "config.toml")
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp cargo config: %w", err)
	}
	if _, err := tmp.WriteString(containerConfigTOML(p.advertiseBase())); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing temp cargo config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("closing temp cargo config: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		p.cfg.Log.Debug("chmod cargo config", "error", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("installing cargo config at %q: %w", target, err)
	}
	p.cfg.Log.Info("wrote container cargo config", "path", target, "mount", containerConfigDest(p.containerOS()))
	return nil
}

// --- routing ---------------------------------------------------------------

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path
	switch {
	case path == healthRoute:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")

	case path == indexRoute+"/config.json":
		p.handleRegistryConfig(w, r)

	case strings.HasPrefix(path, indexRoute+"/"):
		p.handleIndex(w, r, strings.TrimPrefix(path, indexRoute))

	case strings.HasPrefix(path, cratesRoute+"/"):
		p.handleCrate(w, r)

	case strings.HasPrefix(path, rustupRoute+"/"):
		p.handleRustup(w, r, strings.TrimPrefix(path, rustupRoute))

	default:
		http.NotFound(w, r)
	}
}

// handleRegistryConfig serves the registry's config.json with "dl" rewritten
// to point crate downloads at this proxy. The upstream copy is cached (and
// revalidated) like any other index entry, and its "dl" template is retained
// so crate requests can be mapped back to the real CDN.
func (p *Proxy) handleRegistryConfig(w http.ResponseWriter, r *http.Request) {
	cachePath, err := indexCachePath(p.cfg.CacheDir, "/config.json")
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	body, _, err := p.fetchCached(r.Context(), cachePath, p.cfg.IndexUpstream+"/config.json", false, p.cfg.IndexTTL)
	if err != nil {
		p.cfg.Log.Warn("registry config.json unavailable", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	p.setDLTemplate(parseDLTemplate(body))

	rewritten, err := rewriteConfigJSON(body, p.advertiseBase())
	if err != nil {
		// Cannot rewrite: send the upstream config unchanged so cargo
		// downloads straight from the CDN rather than failing.
		p.cfg.Log.Warn("serving upstream registry config unrewritten", "error", err)
		rewritten = body
	}

	w.Header().Set("Content-Type", "application/json")
	// No ETag: the body is ours, not upstream's, so upstream validators
	// would be wrong. The document is tiny.
	w.Header().Set("Cache-Control", "no-cache")
	writeBody(w, r, rewritten)
}

// handleIndex serves a sparse-index entry. Index data is MUTABLE, so a
// cached entry is served for IndexTTL and then revalidated with a
// conditional GET.
func (p *Proxy) handleIndex(w http.ResponseWriter, r *http.Request, indexPath string) {
	cachePath, err := indexCachePath(p.cfg.CacheDir, indexPath)
	if err != nil {
		p.cfg.Log.Warn("rejecting index path", "path", indexPath, "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	body, meta, err := p.fetchCached(r.Context(), cachePath, p.cfg.IndexUpstream+indexPath, isImmutableIndexPath(indexPath), p.cfg.IndexTTL)
	if err != nil {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// Nothing cached and upstream is unreachable. Cargo treats this as
		// fatal either way; report it honestly.
		p.cfg.Log.Warn("index fetch failed", "path", indexPath, "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Cheap client-side revalidation: Cargo keeps its own copy in CARGO_HOME
	// and sends If-None-Match, so most warm-cache requests can be a 304.
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
		if matchesETag(r.Header.Get("If-None-Match"), meta.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if meta.LastModified != "" {
		w.Header().Set("Last-Modified", meta.LastModified)
	}
	w.Header().Set("Content-Type", contentTypeOr(meta.ContentType, "text/plain; charset=utf-8"))
	writeBody(w, r, body)
}

// handleCrate serves a .crate tarball. Tarballs are IMMUTABLE — a published
// (crate, version) is never republished with different bytes — so a cached
// file is served forever and never revalidated.
func (p *Proxy) handleCrate(w http.ResponseWriter, r *http.Request) {
	name, version, ok := parseCrateDownloadPath(r.URL.Path)
	if !ok {
		p.cfg.Log.Warn("rejecting crate download path", "path", r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cachePath, err := crateCachePath(p.cfg.CacheDir, name, version)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	upstreamURL, ok := expandDL(p.dlTemplateOrDefault(), name, version)
	if !ok {
		upstreamURL, _ = expandDL(defaultDLTemplate, name, version)
	}

	body, _, err := p.fetchCached(r.Context(), cachePath, upstreamURL, true, 0)
	if err != nil {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// FAIL OPEN: we could not fetch it, but the container can reach the
		// internet directly. Redirect to the real CDN so the build proceeds
		// (slower, uncached) instead of failing.
		p.cfg.Log.Warn("crate fetch failed; redirecting job to upstream",
			"crate", name, "version", version, "upstream", upstreamURL, "error", err)
		http.Redirect(w, r, upstreamURL, http.StatusTemporaryRedirect)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	// Immutable by construction — let Cargo and any intermediary cache hard.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	writeBody(w, r, body)
}

// handleRustup serves rustup distribution artifacts. Dated artifacts are
// immutable; channel manifests are revalidated on the index TTL.
func (p *Proxy) handleRustup(w http.ResponseWriter, r *http.Request, distPath string) {
	cachePath, err := rustupCachePath(p.cfg.CacheDir, distPath)
	if err != nil {
		p.cfg.Log.Warn("rejecting rustup path", "path", distPath, "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	upstreamURL := p.cfg.RustupUpstream + distPath
	immutable := isImmutableRustupPath(distPath)

	body, meta, err := p.fetchCached(r.Context(), cachePath, upstreamURL, immutable, p.cfg.IndexTTL)
	if err != nil {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		// FAIL OPEN, same reasoning as crate tarballs.
		p.cfg.Log.Warn("rustup fetch failed; redirecting job to upstream",
			"path", distPath, "error", err)
		http.Redirect(w, r, upstreamURL, http.StatusTemporaryRedirect)
		return
	}

	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	w.Header().Set("Content-Type", contentTypeOr(meta.ContentType, "application/octet-stream"))
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	writeBody(w, r, body)
}

// --- cache core ------------------------------------------------------------

// notFoundError marks an upstream 404/410 so handlers can pass it through as
// a 404 rather than treating it as an outage.
type notFoundError struct{ url string }

func (e *notFoundError) Error() string { return "upstream 404 for " + e.url }

func isNotFound(err error) bool {
	var nf *notFoundError
	return errors.As(err, &nf)
}

// fetchCached is the single read path for every route.
//
// It resolves the freshness decision (decideFreshness), then:
//
//   - serveCached  → return the on-disk bytes, no network at all;
//   - revalidate   → conditional GET; 304 refreshes the timestamp and the
//     cached bytes are returned; 200 replaces them;
//   - fetchFresh   → unconditional GET, then store.
//
// FAIL-OPEN AT THIS LAYER: if the upstream request errors or returns 5xx and
// we HAVE a cached copy, the stale copy is served rather than propagating
// the failure. A registry outage then degrades to "slightly stale index"
// instead of a red CI job. Callers layer a second fail-open on top (a
// redirect to the origin) for the case where nothing is cached.
func (p *Proxy) fetchCached(ctx context.Context, cachePath, upstreamURL string, immutable bool, ttl time.Duration) ([]byte, entryMeta, error) {
	// Fast path: a fresh (or immutable) hit needs no lock and no network.
	if body, meta, ok := p.readCache(cachePath); ok {
		if decideFreshness(true, immutable, meta.Fetched, time.Now(), ttl) == serveCached {
			p.cfg.Log.Debug("cargo cache hit", "url", upstreamURL, "immutable", immutable)
			return body, meta, nil
		}
	}

	// Collapse concurrent misses for the same object: without this, ten
	// parallel jobs starting together each pull the same 40 MB toolchain.
	mu := p.lockFor(cachePath)
	mu.Lock()
	defer mu.Unlock()

	body, meta, cached := p.readCache(cachePath)
	decision := decideFreshness(cached, immutable, meta.Fetched, time.Now(), ttl)
	if decision == serveCached {
		p.cfg.Log.Debug("cargo cache hit (after lock)", "url", upstreamURL)
		return body, meta, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		if cached {
			return body, meta, nil
		}
		return nil, entryMeta{}, fmt.Errorf("building upstream request for %s: %w", upstreamURL, err)
	}
	if decision == revalidate {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	p.cfg.Log.Debug("cargo cache miss", "url", upstreamURL, "decision", decision.String())
	resp, err := p.client.Do(req)
	if err != nil {
		if cached {
			p.cfg.Log.Warn("upstream unreachable; serving stale cache", "url", upstreamURL, "error", err)
			return body, meta, nil
		}
		return nil, entryMeta{}, fmt.Errorf("fetching %s: %w", upstreamURL, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			p.cfg.Log.Debug("closing upstream body", "error", cerr)
		}
	}()

	switch {
	case resp.StatusCode == http.StatusNotModified && cached:
		meta.Fetched = time.Now()
		p.writeMeta(cachePath, meta)
		return body, meta, nil

	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, entryMeta{}, &notFoundError{url: upstreamURL}

	case resp.StatusCode != http.StatusOK:
		if cached {
			p.cfg.Log.Warn("upstream error; serving stale cache",
				"url", upstreamURL, "status", resp.StatusCode)
			return body, meta, nil
		}
		return nil, entryMeta{}, fmt.Errorf("fetching %s: upstream status %d", upstreamURL, resp.StatusCode)
	}

	fresh, err := io.ReadAll(resp.Body)
	if err != nil {
		if cached {
			p.cfg.Log.Warn("upstream read failed; serving stale cache", "url", upstreamURL, "error", err)
			return body, meta, nil
		}
		return nil, entryMeta{}, fmt.Errorf("reading %s: %w", upstreamURL, err)
	}

	newMeta := entryMeta{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		Fetched:      time.Now(),
	}
	// A write failure must not fail the request — the bytes are already in
	// hand, the job just loses the caching benefit.
	if err := p.writeCache(cachePath, fresh, newMeta, immutable); err != nil {
		p.cfg.Log.Warn("caching response failed; serving anyway", "url", upstreamURL, "error", err)
	}
	return fresh, newMeta, nil
}

// readCache loads a cached body and its sidecar metadata. A body with no
// sidecar is still usable (it reports a zero Fetched time, so mutable
// entries revalidate and immutable ones are served as-is).
func (p *Proxy) readCache(cachePath string) ([]byte, entryMeta, bool) {
	body, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, entryMeta{}, false
	}
	var meta entryMeta
	if raw, err := os.ReadFile(metaPath(cachePath)); err == nil {
		if err := json.Unmarshal(raw, &meta); err != nil {
			p.cfg.Log.Debug("discarding unreadable cache metadata", "path", cachePath, "error", err)
			meta = entryMeta{}
		}
	}
	return body, meta, true
}

// writeCache stores a body and its metadata atomically (write temp, rename)
// so a concurrent reader never sees a partial file. Immutable entries get no
// sidecar: there is nothing to revalidate, and skipping it halves the inode
// count for the tarball cache.
func (p *Proxy) writeCache(cachePath string, body []byte, meta entryMeta, immutable bool) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir %q: %w", dir, err)
	}
	if err := atomicWrite(cachePath, body); err != nil {
		return err
	}
	if immutable {
		return nil
	}
	p.writeMeta(cachePath, meta)
	return nil
}

// writeMeta persists the sidecar. Best-effort: a missing sidecar only costs
// a revalidation next time.
func (p *Proxy) writeMeta(cachePath string, meta entryMeta) {
	raw, err := json.Marshal(meta)
	if err != nil {
		p.cfg.Log.Debug("encoding cache metadata", "path", cachePath, "error", err)
		return
	}
	if err := atomicWrite(metaPath(cachePath), raw); err != nil {
		p.cfg.Log.Debug("writing cache metadata", "path", cachePath, "error", err)
	}
}

func atomicWrite(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".cargoproxy-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %q: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("closing temp file for %q: %w", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("renaming temp file to %q: %w", target, err)
	}
	return nil
}

func (p *Proxy) lockFor(key string) *sync.Mutex {
	v, _ := p.inflight.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (p *Proxy) setDLTemplate(tmpl string) {
	if tmpl == "" {
		return
	}
	p.mu.Lock()
	p.dlTemplate = tmpl
	p.mu.Unlock()
}

func (p *Proxy) dlTemplateOrDefault() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.dlTemplate == "" {
		return defaultDLTemplate
	}
	return p.dlTemplate
}

// loadDLTemplateFromCache recovers the upstream "dl" template across a
// daemon restart. Cargo caches config.json in CARGO_HOME too, so it may go
// straight to a crate download without asking us for config.json first.
func (p *Proxy) loadDLTemplateFromCache() {
	cachePath, err := indexCachePath(p.cfg.CacheDir, "/config.json")
	if err != nil {
		return
	}
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return
	}
	p.setDLTemplate(parseDLTemplate(raw))
}

// --- small helpers ---------------------------------------------------------

// writeBody writes the response body, honouring HEAD (headers only).
func writeBody(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		// The client went away mid-response; nothing to do but note it.
		_ = err
	}
}

// matchesETag reports whether an If-None-Match header matches the given tag.
// Handles the "*" wildcard, comma-separated lists, and the W/ weak prefix.
func matchesETag(ifNoneMatch, tag string) bool {
	if ifNoneMatch == "" || tag == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	want := strings.TrimPrefix(tag, "W/")
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

func contentTypeOr(got, fallback string) string {
	if got == "" {
		return fallback
	}
	return got
}
