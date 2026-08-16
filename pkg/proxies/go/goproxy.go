// Package goproxy implements proxies.CacheProxy for Go modules.
//
// It implements the Go module proxy protocol (GOPROXY spec) and caches
// immutable responses (.info, .mod, .zip) on disk. Mutable endpoints
// (list, @latest) are passed through to the upstream proxy without caching.
//
// ephemerd runs one shared instance on the bridge gateway IP so all
// job containers can reach it. Containers see it as GOPROXY=http://<gateway>:<port>.
// Jobs have no write access to the cache — they just make HTTP requests.
//
// # Why one shared cache across every job is safe
//
// This is load-bearing and worth writing down, because the cache key is the
// request path and nothing else — no job, repo or owner identity takes part,
// so a module a job for repo A pulled is served to a job for repo B.
//
//  1. Jobs cannot write the cache. They speak HTTP; only this daemon writes
//     files.
//  2. Key and content cannot disagree. cacheAndServe stores upstream's
//     response for a path under the key derived from that same path, so a
//     job can pick WHICH key gets populated but never WHAT goes in it.
//  3. The `go` client authenticates modules itself, via go.sum and the
//     checksum database. sumdb requests are passed straight through and
//     never cached, so a mismatched .zip fails verification in the client.
//     Note this is a property of the CLIENT, not of this proxy — this proxy
//     verifies nothing, and a job that opts itself out (GOFLAGS=-mod=mod,
//     GONOSUMDB, GOPRIVATE) only harms itself.
//  4. Mutable endpoints (/@v/list, /@latest) are passed through, so a job
//     cannot pin another job's view of "latest".
//
// What sharing DOES expose is disk: any job can request arbitrarily many
// module versions and fill the node. That is what Prune bounds.
package goproxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies"
)

const (
	// DefaultMaxCacheBytes is the ceiling applied when Config.MaxCacheBytes
	// is unset. See config.ModuleProxyConfig.MaxCacheGB for why 20 GiB.
	DefaultMaxCacheBytes int64 = 20 << 30

	// DefaultPruneInterval is how often eviction runs when
	// Config.PruneInterval is unset.
	DefaultPruneInterval = time.Hour

	// tmpPrefix is the prefix os.CreateTemp is given for in-flight cache
	// writes. Files with it are counted toward the cache size but are
	// never evicted — removing one would break the rename about to
	// publish it.
	tmpPrefix = ".goproxy-"

	// touchInterval throttles the modtime refresh done on cache hits, so a
	// hot module costs at most one metadata write per hour rather than one
	// per request.
	touchInterval = time.Hour
)

// Config for the Go module caching proxy.
type Config struct {
	CacheDir   string // on-disk cache directory
	Upstream   string // upstream proxy URL (default: https://proxy.golang.org)
	ListenAddr string // address to listen on (e.g., "10.88.0.1:8082")
	Cleanup    bool   // wipe cache dir on Stop

	// MaxCacheBytes bounds the on-disk cache. Prune evicts
	// least-recently-used files until the directory is back under it.
	// Zero selects DefaultMaxCacheBytes; a negative value means unbounded.
	MaxCacheBytes int64

	// PruneInterval is how often the eviction pass runs. Zero selects
	// DefaultPruneInterval; a negative value disables periodic pruning
	// (Prune can still be called directly).
	PruneInterval time.Duration

	Log *slog.Logger
}

// Compile-time interface check.
var _ proxies.CacheProxy = (*Proxy)(nil)

// Proxy is a caching Go module proxy server.
type Proxy struct {
	cfg      Config
	server   *http.Server
	listener net.Listener
	client   *http.Client
	inflight sync.Map // prevents duplicate upstream fetches for the same path

	// pruneStop is closed by Stop to end the eviction loop. stopOnce keeps
	// a second Stop from closing it twice.
	pruneStop chan struct{}
	stopOnce  sync.Once
}

// New creates a Go module caching proxy. Call Start() to begin serving.
func New(cfg Config) *Proxy {
	if cfg.Upstream == "" {
		cfg.Upstream = "https://proxy.golang.org"
	}
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	if cfg.MaxCacheBytes == 0 {
		cfg.MaxCacheBytes = DefaultMaxCacheBytes
	}
	if cfg.PruneInterval == 0 {
		cfg.PruneInterval = DefaultPruneInterval
	}

	return &Proxy{
		cfg: cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		pruneStop: make(chan struct{}),
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
			p.cfg.Log.Error("go module proxy server error", "error", err)
		}
	}()

	if p.cfg.PruneInterval > 0 {
		go p.pruneLoop()
	}

	p.cfg.Log.Info("go module proxy started",
		"addr", ln.Addr().String(),
		"cache", p.cfg.CacheDir,
		"max_bytes", p.cfg.MaxCacheBytes,
		"prune_interval", p.cfg.PruneInterval)
	return nil
}

// pruneLoop bounds the cache for as long as the proxy is serving. The first
// pass runs immediately: the cache may have been inherited from a previous
// daemon (cleanup = false) already over the cap, and waiting an interval to
// notice that is exactly the window a full disk needs.
func (p *Proxy) pruneLoop() {
	if _, err := p.Prune(); err != nil {
		p.cfg.Log.Warn("go module cache prune failed", "error", err)
	}
	ticker := time.NewTicker(p.cfg.PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.pruneStop:
			return
		case <-ticker.C:
			if _, err := p.Prune(); err != nil {
				p.cfg.Log.Warn("go module cache prune failed", "error", err)
			}
		}
	}
}

// Stop shuts down the proxy and optionally wipes the cache.
func (p *Proxy) Stop() error {
	var errs []error

	if p.pruneStop != nil {
		p.stopOnce.Do(func() { close(p.pruneStop) })
	}

	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.server.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutting down proxy: %w", err))
		}
	}

	if p.cfg.Cleanup {
		p.cfg.Log.Info("cleaning up go module cache", "dir", p.cfg.CacheDir)
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
func (p *Proxy) EnvVars() []string {
	return []string{
		"GOPROXY=http://" + p.Addr() + ",direct",
	}
}

// Name returns the proxy name for logging.
func (p *Proxy) Name() string { return "go" }

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// sumdb requests: always pass through, never cache.
	if strings.HasPrefix(path, "/sumdb/") {
		p.reverseProxy(w, r)
		return
	}

	// Mutable endpoints: pass through without caching.
	if strings.HasSuffix(path, "/@v/list") || strings.HasSuffix(path, "/@latest") {
		p.reverseProxy(w, r)
		return
	}

	// Immutable endpoints: .info, .mod, .zip — cache on disk.
	if strings.HasSuffix(path, ".info") || strings.HasSuffix(path, ".mod") || strings.HasSuffix(path, ".zip") {
		p.cacheAndServe(w, r)
		return
	}

	// Unknown path — pass through.
	p.reverseProxy(w, r)
}

func (p *Proxy) cacheAndServe(w http.ResponseWriter, r *http.Request) {
	cachePath, err := p.cachePath(r.URL.Path)
	if err != nil {
		// Do not forward it upstream either — a path this function refuses
		// is malformed by the module proxy protocol's own grammar.
		p.cfg.Log.Warn("rejecting module proxy request", "path", r.URL.Path, "error", err)
		http.Error(w, "bad request path", http.StatusBadRequest)
		return
	}

	if info, err := os.Stat(cachePath); err == nil && info.Size() > 0 {
		p.cfg.Log.Debug("cache hit", "path", r.URL.Path)
		p.touch(cachePath, info)
		http.ServeFile(w, r, cachePath)
		return
	}

	mu := p.getInflightMutex(r.URL.Path)
	mu.Lock()
	defer mu.Unlock()

	if info, err := os.Stat(cachePath); err == nil && info.Size() > 0 {
		p.cfg.Log.Debug("cache hit (after lock)", "path", r.URL.Path)
		p.touch(cachePath, info)
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

	tmpFile, err := os.CreateTemp(filepath.Dir(cachePath), ".goproxy-*")
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

// touch refreshes a cache file's modtime so Prune, which uses modtime as its
// LRU key, sees real usage rather than only the time the file was written.
//
// Modtime is the key because atime is not portable: relatime/noatime is the
// norm on Linux and NTFS last-access updates are off by default on Windows.
// Modtime is otherwise never rewritten here — the write path publishes files
// with os.Rename — so without this a module downloaded once and then served
// from cache for a month would still look a month old and be evicted first.
func (p *Proxy) touch(path string, info os.FileInfo) {
	if time.Since(info.ModTime()) < touchInterval {
		return
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		p.cfg.Log.Debug("touching cache file", "path", path, "error", err)
	}
}

// Prune evicts least-recently-used files until the cache directory is at or
// below MaxCacheBytes, then removes the directories that emptied. It returns
// the number of bytes freed.
//
// This is the only thing bounding the cache. Every job on the node shares it
// and any job can ask for arbitrarily many module versions, so without a
// bound one job can fill the disk and degrade every job that follows it on
// that node. Safe to call at any time; it only ever removes cache files.
func (p *Proxy) Prune() (int64, error) {
	limit := p.cfg.MaxCacheBytes
	if limit <= 0 {
		return 0, nil // explicitly unbounded
	}

	type cacheFile struct {
		path string
		size int64
		mod  time.Time
	}
	var (
		files []cacheFile
		total int64
	)
	err := filepath.WalkDir(p.cfg.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				return nil // raced with another prune or a rename
			}
			return ierr
		}
		total += info.Size()
		if strings.HasPrefix(d.Name(), tmpPrefix) {
			// Counted above (they are real bytes) but never evicted.
			return nil
		}
		files = append(files, cacheFile{path: path, size: info.Size(), mod: info.ModTime()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walking module cache: %w", err)
	}
	if total <= limit {
		return 0, nil
	}

	// Least recently used first.
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var freed int64
	for _, f := range files {
		if total <= limit {
			break
		}
		if rerr := os.Remove(f.path); rerr != nil && !os.IsNotExist(rerr) {
			// A file we cannot remove (in use on Windows, permissions)
			// still occupies its bytes, so do not credit them as freed.
			p.cfg.Log.Debug("evicting cache file", "path", f.path, "error", rerr)
			continue
		}
		total -= f.size
		freed += f.size
	}

	p.removeEmptyDirs()
	p.cfg.Log.Info("pruned go module cache",
		"freed_bytes", freed, "remaining_bytes", total, "max_bytes", limit)
	return freed, nil
}

// removeEmptyDirs deletes cache subdirectories left empty by eviction,
// deepest first. The cache root itself is never removed — the proxy is still
// serving out of it.
//
// os.Remove refuses to delete a non-empty directory, which is precisely the
// filter wanted here, so no separate emptiness check is needed.
func (p *Proxy) removeEmptyDirs() {
	var dirs []string
	_ = filepath.WalkDir(p.cfg.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a dir we cannot read is simply skipped
		}
		if d.IsDir() && filepath.Clean(path) != filepath.Clean(p.cfg.CacheDir) {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Deepest first, so a parent emptied by removing its children is itself
	// removable in the same pass.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(os.PathSeparator)) > strings.Count(dirs[j], string(os.PathSeparator))
	})
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			p.cfg.Log.Debug("removing empty cache dir", "path", dir, "error", err)
		}
	}
}

// errUnsafePath rejects a request path that must never reach the filesystem.
var errUnsafePath = errors.New("unsafe request path")

const (
	// maxRequestPathLen caps the whole request path. Real module paths are
	// well under this; the cap keeps a pathological request from producing
	// a path the OS rejects halfway through creating it.
	maxRequestPathLen = 1024
	// maxPathSegmentLen caps one path component, matching the NAME_MAX
	// every filesystem ephemerd runs on enforces.
	maxPathSegmentLen = 255
)

// cachePath maps a request path to its on-disk cache file, or returns an
// error if the path is not safe to put on the filesystem.
//
// SECURITY: urlPath is the DECODED path, chosen entirely by a job container,
// and filepath.Join cleans — so a "../" segment would traverse out of the
// cache root, and the sha256 prefix does not help because it is derived from
// the same attacker-influenced string. Nothing in this package used to check
// that: safety rested on http.ServeMux redirecting unclean paths before the
// handler ran, which is an undocumented dependency on net/http internals.
// The path is now validated here, so containment is a property of this
// function rather than of the router in front of it. See the table tests in
// goproxy_test.go.
func (p *Proxy) cachePath(urlPath string) (string, error) {
	rel, err := sanitizeRequestPath(urlPath)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(rel))
	prefix := fmt.Sprintf("%x", h[:2])
	full := filepath.Join(p.cfg.CacheDir, prefix, filepath.FromSlash(rel))
	// Belt and braces: sanitizeRequestPath should make this unreachable,
	// but the containment invariant is the thing that actually matters, so
	// assert it on the result rather than trusting the rules above.
	if !underRoot(p.cfg.CacheDir, full) {
		return "", fmt.Errorf("%w: %q escapes the cache root", errUnsafePath, urlPath)
	}
	return full, nil
}

// sanitizeRequestPath turns a request path into a relative, slash-separated
// cache key, rejecting anything that could mean something other than "a file
// below the cache root" on any platform ephemerd runs on.
//
// The rules are deliberately stricter than "no ..": the Go module proxy
// protocol only ever asks for paths built from module paths and versions,
// whose grammar is letters, digits and "-._~/!". Everything outside that is
// either an encoding trick or a bug.
func sanitizeRequestPath(urlPath string) (string, error) {
	rel := strings.TrimPrefix(urlPath, "/")
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", errUnsafePath)
	}
	if len(rel) > maxRequestPathLen {
		return "", fmt.Errorf("%w: path is %d bytes, limit %d", errUnsafePath, len(rel), maxRequestPathLen)
	}
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		if c < 0x20 || c == 0x7f {
			// NUL truncates paths in syscalls; newline and friends have no
			// business in a module path at all.
			return "", fmt.Errorf("%w: control byte %#02x", errUnsafePath, c)
		}
		// '\' separates paths on Windows (so "..\..\x" traverses there) and
		// ':' introduces a drive letter ("C:\x") or an NTFS alternate data
		// stream ("file:stream"). Neither is legal in a module path, and
		// both are rejected on EVERY platform so the cache layout and this
		// function's behavior are identical everywhere.
		if c == '\\' || c == ':' {
			return "", fmt.Errorf("%w: %q is not allowed in a module path", errUnsafePath, rune(c))
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "":
			// Catches "//server/share" (a UNC path once separators are
			// converted), "a//b", and any trailing slash.
			return "", fmt.Errorf("%w: empty path segment", errUnsafePath)
		case ".", "..":
			return "", fmt.Errorf("%w: %q path segment", errUnsafePath, seg)
		}
		if len(seg) > maxPathSegmentLen {
			return "", fmt.Errorf("%w: path segment is %d bytes, limit %d", errUnsafePath, len(seg), maxPathSegmentLen)
		}
	}
	return rel, nil
}

// underRoot reports whether target is root itself or a descendant of it.
//
// Uses filepath.Rel on cleaned absolute paths rather than a string prefix,
// so it is correct with either separator, is not fooled by a shared prefix
// (".../gomod2" is not under ".../gomod"), and returns false across Windows
// volumes (where Rel errors).
func underRoot(root, target string) bool {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (p *Proxy) getInflightMutex(path string) *sync.Mutex {
	v, _ := p.inflight.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}
