// Package composerproxy implements proxies.CacheProxy for PHP Composer.
//
// It is a caching reverse proxy in front of the Packagist metadata repository
// (https://repo.packagist.org). Composer fetches package metadata (the
// packages.json entry point plus per-package p2/<vendor>/<name>.json files)
// from that repo; distribution zips come from mirrors and GitHub, which this
// proxy does not front.
//
// Composer honors the COMPOSER_REPO_PACKAGIST env var to override the default
// Packagist repository URL, so containers are pointed at this proxy purely via
// that env var (see EnvVars). Immutable, hashed metadata files (the
// "%package%$%hash%.json" metadata-URL form Packagist serves) are cached on
// disk; the mutable packages.json entry point is passed through without
// caching.
//
// ephemerd runs one shared instance on the bridge gateway IP so all job
// containers can reach it. Jobs have no write access to the cache — they just
// make HTTP requests.
package composerproxy

import (
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

// Config for the Composer/Packagist caching proxy.
type Config struct {
	CacheDir   string // on-disk cache directory
	Upstream   string // upstream Packagist repo URL (default: https://repo.packagist.org)
	ListenAddr string // address to listen on (e.g., "10.88.0.1:8084")
	Cleanup    bool   // wipe cache dir on Stop
	Log        *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is a caching Composer/Packagist proxy server.
type Proxy struct {
	cfg      Config
	server   *http.Server
	listener net.Listener
	client   *http.Client
	inflight sync.Map // prevents duplicate upstream fetches for the same path
}

// New creates a Composer/Packagist caching proxy. Call Start() to begin serving.
func New(cfg Config) *Proxy {
	if cfg.Upstream == "" {
		cfg.Upstream = "https://repo.packagist.org"
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")

	return &Proxy{
		cfg: cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
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
			p.cfg.Log.Error("composer packagist proxy server error", "error", err)
		}
	}()

	p.cfg.Log.Info("composer packagist proxy started", "addr", ln.Addr().String(), "cache", p.cfg.CacheDir)
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
		p.cfg.Log.Info("cleaning up composer packagist cache", "dir", p.cfg.CacheDir)
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
// Composer reads COMPOSER_REPO_PACKAGIST to override the default Packagist
// repository URL, so pointing it at our proxy is a single env var. Metadata
// (and only metadata) then flows through the cache; distribution zips still
// come from their original mirrors/GitHub.
func (p *Proxy) EnvVars() []string {
	return []string{
		"COMPOSER_REPO_PACKAGIST=http://" + p.Addr(),
	}
}

// Name returns the proxy name for logging.
func (p *Proxy) Name() string { return "composer" }

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// The packages.json entry point is mutable (it references the current
	// metadata URLs and provider hashes) — pass through without caching.
	if path == "/packages.json" || path == "/" {
		p.reverseProxy(w, r)
		return
	}

	// Per-package metadata files are content-addressed / hashed and served
	// under /p2/ (and legacy /p/) — immutable, so cache on disk.
	if strings.HasPrefix(path, "/p2/") || strings.HasPrefix(path, "/p/") {
		p.cacheAndServe(w, r)
		return
	}

	// Unknown path — pass through.
	p.reverseProxy(w, r)
}

func (p *Proxy) cacheAndServe(w http.ResponseWriter, r *http.Request) {
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
	upstreamURL := p.cfg.Upstream + r.URL.Path
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

	tmpFile, err := os.CreateTemp(filepath.Dir(cachePath), ".composerproxy-*")
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

func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request) {
	upstreamURL := p.cfg.Upstream + r.URL.Path
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
