// Package cargoproxy implements proxies.CacheProxy for Rust crates.
//
// It is a caching reverse proxy in front of the crates.io sparse registry.
// Cargo's modern default registry access is the sparse HTTP protocol
// (CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse): the index is served over
// plain HTTP at https://index.crates.io and crate archives (.crate) are
// downloaded from https://static.crates.io.
//
// This proxy serves both under one origin, keyed by path prefix:
//
//	/index/...   -> https://index.crates.io/...   (sparse index config + metadata)
//	/dl/...      -> https://static.crates.io/...   (crate .crate downloads)
//
// The sparse index's config.json advertises a "dl" download endpoint; we
// rewrite it to point back at our /dl/ prefix so crate downloads also flow
// through the cache. Index metadata files (mutable — they gain new versions
// over time) are passed through without caching; immutable .crate archives
// are cached on disk.
//
// Containers are pointed at this proxy purely via env vars using cargo's
// source-replacement mechanism (see EnvVars): a replacement registry source
// named "ephemerd-crates" backed by our sparse endpoint, with crates-io
// replaced by it. No .cargo/config.toml file is needed — every knob has a
// CARGO_* env-var form.
//
// ephemerd runs one shared instance on the bridge gateway IP so all job
// containers can reach it. Jobs have no write access to the cache — they
// just make HTTP requests.
package cargoproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

// Config for the Rust crates caching proxy.
type Config struct {
	CacheDir   string // on-disk cache directory
	Upstream   string // sparse index upstream URL (default: https://index.crates.io)
	Downstream string // crate download upstream URL (default: https://static.crates.io)
	ListenAddr string // address to listen on (e.g., "10.88.0.1:8083")
	Cleanup    bool   // wipe cache dir on Stop
	Log        *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is a caching Rust crates proxy server.
type Proxy struct {
	cfg      Config
	server   *http.Server
	listener net.Listener
	client   *http.Client
	inflight sync.Map // prevents duplicate upstream fetches for the same path
}

// New creates a Rust crates caching proxy. Call Start() to begin serving.
func New(cfg Config) *Proxy {
	if cfg.Upstream == "" {
		cfg.Upstream = "https://index.crates.io"
	}
	if cfg.Downstream == "" {
		cfg.Downstream = "https://static.crates.io"
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	cfg.Downstream = strings.TrimRight(cfg.Downstream, "/")

	return &Proxy{
		cfg: cfg,
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Start begins serving the proxy. Returns after the listener is bound.
func (p *Proxy) Start() error {
	if err := os.MkdirAll(p.cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	ln, err := net.Listen("tcp", p.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", p.cfg.ListenAddr, err)
	}
	p.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)

	p.server = &http.Server{Handler: mux}

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.cfg.Log.Error("cargo crates proxy server error", "error", err)
		}
	}()

	p.cfg.Log.Info("cargo crates proxy started", "addr", ln.Addr().String(), "cache", p.cfg.CacheDir)
	return nil
}

// Stop shuts down the proxy and optionally wipes the cache.
func (p *Proxy) Stop() error {
	var errs []error

	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutting down proxy: %w", err))
		}
	}

	if p.cfg.Cleanup {
		p.cfg.Log.Info("cleaning up cargo crates cache", "dir", p.cfg.CacheDir)
		if err := os.RemoveAll(p.cfg.CacheDir); err != nil {
			errs = append(errs, fmt.Errorf("cleaning cache: %w", err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Addr returns the address the proxy is listening on.
func (p *Proxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.cfg.ListenAddr
}

// EnvVars returns the environment variables to inject into job containers.
//
// Cargo has no single "point crates.io at a mirror" env var, but every knob
// in .cargo/config.toml has a CARGO_* form, so a full source replacement can
// be expressed purely in the environment:
//
//   - Force the sparse protocol for the built-in crates-io registry.
//   - Define a replacement registry "ephemerd-crates" whose sparse index is
//     our proxy's /index/ endpoint (the "sparse+" prefix selects the sparse
//     protocol for a named registry).
//   - Replace the crates-io source with that registry via
//     CARGO_SOURCE_crates-io_REPLACE_WITH.
//
// This is the env equivalent of:
//
//	[registries.ephemerd-crates]
//	index = "sparse+http://<addr>/index/"
//	[source.crates-io]
//	replace-with = "ephemerd-crates"
func (p *Proxy) EnvVars() []string {
	base := "http://" + p.Addr()
	return []string{
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse",
		"CARGO_REGISTRIES_EPHEMERD_CRATES_INDEX=sparse+" + base + "/index/",
		"CARGO_SOURCE_CRATES_IO_REPLACE_WITH=ephemerd-crates",
	}
}

// Name returns the proxy name for logging.
func (p *Proxy) Name() string { return "cargo" }

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/index/config.json":
		// The sparse index config advertises the crate download endpoint.
		// Rewrite it to route .crate downloads back through our /dl/ prefix
		// so they get cached too. Never cache it — it's tiny and we mutate it.
		p.serveIndexConfig(w, r)
	case strings.HasPrefix(path, "/index/"):
		// Sparse index metadata: mutable (new versions appear over time),
		// so pass through without caching.
		p.reverseProxy(w, r, p.cfg.Upstream, strings.TrimPrefix(path, "/index"))
	case strings.HasPrefix(path, "/dl/"):
		// Crate .crate archives: immutable, cache on disk.
		p.cacheAndServe(w, r, p.cfg.Downstream, strings.TrimPrefix(path, "/dl"))
	default:
		http.NotFound(w, r)
	}
}

// serveIndexConfig fetches the sparse index config.json and rewrites its "dl"
// field to point at our /dl/ download prefix so crate downloads are proxied.
func (p *Proxy) serveIndexConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := p.client.Get(p.cfg.Upstream + "/config.json")
	if err != nil {
		p.cfg.Log.Warn("upstream index config fetch failed", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.cfg.Log.Debug("closing upstream response body", "error", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.cfg.Log.Warn("reading upstream index config", "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// The crates.io sparse config exposes a "dl" download template. Point it
	// at our own /dl/ prefix so the crate archives flow through the cache.
	// The crates.io "dl" value has no {crate}/{version} markers, so cargo
	// appends the standard /{crate}/{version}/download suffix itself; a bare
	// base URL is the correct replacement.
	dl := `"dl": "http://` + p.Addr() + `/dl"`
	rewritten := rewriteDL(body, dl)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(rewritten); err != nil {
		p.cfg.Log.Debug("writing index config", "error", err)
	}
}

// rewriteDL replaces the JSON "dl": "<...>" member with the given replacement.
// Kept dependency-free (no json unmarshal) since the config is a tiny, stable
// object and we only touch one field.
func rewriteDL(body []byte, replacement string) []byte {
	start := bytes.Index(body, []byte(`"dl"`))
	if start < 0 {
		return body
	}
	// Find the value string: first quote after the colon, then its closing quote.
	colon := bytes.IndexByte(body[start:], ':')
	if colon < 0 {
		return body
	}
	valStart := bytes.IndexByte(body[start+colon:], '"')
	if valStart < 0 {
		return body
	}
	valStart += start + colon
	valEnd := bytes.IndexByte(body[valStart+1:], '"')
	if valEnd < 0 {
		return body
	}
	valEnd += valStart + 1

	var out bytes.Buffer
	out.Write(body[:start])
	out.WriteString(replacement)
	out.Write(body[valEnd+1:])
	return out.Bytes()
}

func (p *Proxy) cacheAndServe(w http.ResponseWriter, r *http.Request, upstreamBase, upstreamPath string) {
	cachePath := p.cachePath(r.URL.Path)

	if info, err := os.Stat(cachePath); err == nil && info.Size() > 0 {
		p.cfg.Log.Debug("cache hit", "path", r.URL.Path)
		http.ServeFile(w, r, cachePath)
		return
	}

	mu := p.getInflightMutex(r.URL.Path)
	mu.Lock()
	defer mu.Unlock()

	if info, err := os.Stat(cachePath); err == nil && info.Size() > 0 {
		p.cfg.Log.Debug("cache hit (after lock)", "path", r.URL.Path)
		http.ServeFile(w, r, cachePath)
		return
	}

	p.cfg.Log.Debug("cache miss", "path", r.URL.Path)
	upstreamURL := upstreamBase + upstreamPath
	resp, err := p.client.Get(upstreamURL)
	if err != nil {
		p.cfg.Log.Warn("upstream fetch failed", "url", upstreamURL, "error", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.cfg.Log.Debug("closing upstream response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			p.cfg.Log.Debug("error forwarding upstream error response", "error", err)
		}
		return
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		p.cfg.Log.Warn("creating cache dir", "error", err)
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, resp.Body); err != nil {
			p.cfg.Log.Debug("error proxying response", "error", err)
		}
		return
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(cachePath), ".cargoproxy-*")
	if err != nil {
		p.cfg.Log.Warn("creating temp file", "error", err)
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, resp.Body); err != nil {
			p.cfg.Log.Debug("error proxying response", "error", err)
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	mw := io.MultiWriter(tmpFile, w)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		p.cfg.Log.Warn("writing cache", "error", err)
		if err := tmpFile.Close(); err != nil {
			p.cfg.Log.Debug("closing temp file after error", "error", err)
		}
		if err := os.Remove(tmpFile.Name()); err != nil {
			p.cfg.Log.Debug("removing temp file after write error", "error", err)
		}
		return
	}

	if err := tmpFile.Close(); err != nil {
		p.cfg.Log.Warn("closing temp file", "error", err)
		if err := os.Remove(tmpFile.Name()); err != nil {
			p.cfg.Log.Debug("removing temp file after close error", "error", err)
		}
		return
	}

	if err := os.Rename(tmpFile.Name(), cachePath); err != nil {
		p.cfg.Log.Warn("renaming cache file", "error", err)
		if err := os.Remove(tmpFile.Name()); err != nil {
			p.cfg.Log.Debug("removing temp file after rename error", "error", err)
		}
	}
}

func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request, upstreamBase, upstreamPath string) {
	upstreamURL := upstreamBase + upstreamPath
	resp, err := p.client.Get(upstreamURL)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.cfg.Log.Debug("closing upstream response body", "error", err)
		}
	}()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		p.cfg.Log.Debug("error proxying response", "error", err)
	}
}

func (p *Proxy) cachePath(urlPath string) string {
	clean := strings.TrimPrefix(urlPath, "/")
	h := sha256.Sum256([]byte(clean))
	prefix := fmt.Sprintf("%x", h[:2])
	return filepath.Join(p.cfg.CacheDir, prefix, filepath.FromSlash(clean))
}

func (p *Proxy) getInflightMutex(path string) *sync.Mutex {
	v, _ := p.inflight.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}
