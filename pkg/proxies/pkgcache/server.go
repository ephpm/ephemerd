package pkgcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

// ServerConfig configures the HTTP scaffolding shared by the npm, pip and
// pub proxies.
type ServerConfig struct {
	// Name is the proxy name, used in logs ("npm", "pip", "pub").
	Name string
	// ListenAddr is the address to bind AND the address advertised to
	// containers — normally the bridge gateway, e.g. "10.88.0.1:8084".
	ListenAddr string
	// CacheDir is the on-disk cache root.
	CacheDir string
	// MaxBytes is the cache disk budget (see pkgcache.Config).
	MaxBytes int64
	// Cleanup wipes the cache dir on Stop. Defaults to false: a
	// pull-through cache emptied on every restart saves nothing.
	Cleanup bool
	Log     *slog.Logger
}

// Server is the common lifecycle for a cache proxy: bind, serve, shut down,
// and answer a health probe. Each ecosystem proxy embeds one and supplies
// its own routing handler.
type Server struct {
	cfg      ServerConfig
	cache    *Cache
	fetch    *Fetcher
	handler  http.Handler
	server   *http.Server
	listener net.Listener
}

// NewServer creates the cache and the HTTP scaffolding. handler receives
// every request that is not the health probe.
func NewServer(cfg ServerConfig, newHandler func(*Cache, *Fetcher) http.Handler) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cache, err := New(Config{Root: cfg.CacheDir, MaxBytes: cfg.MaxBytes, Log: cfg.Log})
	if err != nil {
		return nil, err
	}
	f := NewFetcher(cache, cfg.Log)
	return &Server{cfg: cfg, cache: cache, fetch: f, handler: newHandler(cache, f)}, nil
}

// Cache returns the underlying bounded store.
func (s *Server) Cache() *Cache { return s.cache }

// Fetcher returns the shared pull-through fetcher.
func (s *Server) Fetcher() *Fetcher { return s.fetch }

// Log returns the proxy's logger.
func (s *Server) Log() *slog.Logger { return s.cfg.Log }

// Start binds the listener and begins serving.
func (s *Server) Start() error {
	// proxies.Listen, not net.Listen: on the Linux bridge path the gateway
	// IP is not assigned to any interface until CNI creates the bridge for
	// the FIRST job container, which happens long after daemon boot. A
	// direct bind fails with EADDRNOTAVAIL every time and the proxy would
	// silently never start.
	ln, err := proxies.Listen(s.cfg.ListenAddr, s.cfg.Log)
	if err != nil {
		return err
	}
	s.listener = ln
	s.server = &http.Server{
		Handler:           http.HandlerFunc(s.route),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Log.Error("package cache proxy server error", "proxy", s.cfg.Name, "error", err)
		}
	}()
	s.cfg.Log.Info("package cache proxy started",
		"proxy", s.cfg.Name,
		"addr", ln.Addr().String(),
		"advertised", s.AdvertiseBase(),
		"cache", s.cache.Root(),
		"cache_bytes", s.cache.Bytes(),
		"max_bytes", s.cache.MaxBytes())
	return nil
}

// Stop shuts the listener down and, if configured, wipes the cache.
func (s *Server) Stop() error {
	var errs []error
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutting down %s proxy: %w", s.cfg.Name, err))
		}
	}
	if s.cfg.Cleanup {
		s.cfg.Log.Info("cleaning up package cache", "proxy", s.cfg.Name, "dir", s.cache.Root())
		if err := os.RemoveAll(s.cache.Root()); err != nil {
			errs = append(errs, fmt.Errorf("cleaning %s cache: %w", s.cfg.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Addr returns the bound address, or the configured one before Start.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.ListenAddr
}

// advertiseAddr is the host:port containers should use to reach this proxy.
//
// The HOST comes from the CONFIGURED address, never the bound one: after the
// wildcard fallback in proxies.Listen the bound address is "[::]:8084",
// which means nothing inside a container.
//
// The PORT comes from the listener whenever an ephemeral port was requested
// (":0"), since the configured value is then not an address anyone can
// connect to.
func (s *Server) advertiseAddr() string {
	addr := s.cfg.ListenAddr
	if addr == "" {
		return s.Addr()
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if (port == "" || port == "0") && s.listener != nil {
		if _, bound, berr := net.SplitHostPort(s.listener.Addr().String()); berr == nil {
			return net.JoinHostPort(host, bound)
		}
	}
	return addr
}

// AdvertiseBase is the base URL job containers use to reach this proxy.
func (s *Server) AdvertiseBase() string { return "http://" + s.advertiseAddr() }

// AdvertiseHost is the host part of AdvertiseBase, for tools (pip) that want
// a bare host rather than a URL.
func (s *Server) AdvertiseHost() string {
	if host, _, err := net.SplitHostPort(s.advertiseAddr()); err == nil {
		return host
	}
	return s.advertiseAddr()
}

// AdvertiseHostPort is the host:port form of the advertised address.
func (s *Server) AdvertiseHostPort() string { return s.advertiseAddr() }

// Healthy probes the proxy's own health endpoint over the loopback port it
// is actually bound to. main.go calls this before injecting the proxy's env
// vars into job containers: for npm, pip and pub the env var itself has NO
// fallback (unlike GOPROXY's "|direct"), so the only way to fail open at the
// tool level is to not point the tool here in the first place when the
// proxy is not answering.
func (s *Server) Healthy() bool {
	if s.listener == nil {
		return false
	}
	addr := s.listener.Addr().String()
	if _, port, err := net.SplitHostPort(addr); err == nil {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + HealthRoute) //nolint:noctx // short, bounded probe
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// route gates methods and handles the health probe, then delegates.
//
// Only GET and HEAD are served. Everything else — npm publish, npm audit's
// POST, a pub upload — is rejected here and NEVER relayed upstream: this is
// a read-through cache for public content, not a registry front end. See
// each proxy's docs for what that costs (short version: `npm audit` degrades
// to a warning, publishing must target the real registry directly).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthRoute {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "this is a read-only pull-through cache: only GET and HEAD are served", http.StatusMethodNotAllowed)
		return
	}
	s.handler.ServeHTTP(w, r)
}

// --- response helpers shared by the proxies --------------------------------

// WriteDocument serves a (possibly rewritten) metadata document.
//
// The ETag is computed over the bytes WE serve, not the upstream ones: every
// proxy rewrites download URLs inside these documents, so replaying the
// upstream validator would tell the client that a body it has never seen is
// unchanged. Clients that send If-None-Match still get their 304.
func WriteDocument(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	etag := WeakETag(body)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", contentType)
	// Mutable: a client may hold it, but must ask before reusing it.
	w.Header().Set("Cache-Control", "no-cache")
	if MatchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		// The client went away mid-response. Nothing to do.
		_ = err
	}
}

// WeakETag is a stable validator for a body we generated.
func WeakETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// MatchesETag reports whether an If-None-Match header matches tag. Handles
// the "*" wildcard, comma-separated lists and the W/ weak prefix.
func MatchesETag(ifNoneMatch, tag string) bool {
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
