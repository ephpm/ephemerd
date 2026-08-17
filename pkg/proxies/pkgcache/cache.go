// Package pkgcache is the shared engine behind ephemerd's language package
// caches (npm, pip, pub). It provides three things the individual proxies
// would otherwise each reinvent:
//
//   - a BOUNDED on-disk store: every cache has a byte budget and evicts
//     least-recently-used entries to stay under it, so a warm CI node never
//     becomes the next disk-full incident (see [image_gc] / [buildkit] GC in
//     config.example.toml for the history);
//   - TRAVERSAL-SAFE key→path mapping, with two independent checks;
//   - a pull-through fetch that FAILS OPEN: stale-on-error, and a documented
//     hand-off to the caller when nothing is cached at all.
//
// It is deliberately protocol-agnostic. Which URLs are immutable, which are
// mutable, and how a metadata document is rewritten are decisions each
// ecosystem's proxy makes.
package pkgcache

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes is the per-cache disk budget applied when an operator does
// not set one. Deliberately modest: three caches at this size is 15 GiB, and
// these nodes have filled their disks before.
const DefaultMaxBytes int64 = 5 << 30 // 5 GiB

// evictTarget is the fraction of the budget a triggered eviction pass frees
// down to. Evicting to exactly the cap would re-trigger on the very next
// write; the gap is the same anti-thrash idea as [image_gc]'s two
// watermarks, just expressed as one ratio.
const evictTarget = 0.90

// metaExt is the sidecar extension. Sidecars hold upstream validators and
// are never served directly.
const metaExt = ".meta"

// tmpPrefix marks in-progress writes. Files with this prefix are ignored by
// the index scan and swept at startup.
const tmpPrefix = ".pkgcache-tmp-"

// Config configures a Cache.
type Config struct {
	// Root is the on-disk cache directory. Created if missing.
	Root string
	// MaxBytes is the disk budget. Zero takes DefaultMaxBytes. A NEGATIVE
	// value disables the budget entirely — supported so an operator can opt
	// out knowingly, never by accident.
	MaxBytes int64
	// Log receives eviction and error reporting.
	Log *slog.Logger
}

// entry is one indexed cache object: the body plus its sidecar.
type entry struct {
	// key is the cache key (slash-separated, as handed to KeyPath).
	key string
	// path is the absolute on-disk path of the body.
	path string
	// bytes is the body size plus the sidecar size — what eviction frees.
	bytes int64
	// used is the last access time, seeded from the file's mtime so LRU
	// order survives a daemon restart.
	used time.Time
}

// Cache is a bounded, LRU-evicting on-disk store.
//
// Safe for concurrent use. The index (sizes and access times) lives in
// memory and is rebuilt by scanning Root at startup, so an operator running
// `ephemerd cache clear` underneath a live daemon degrades to a slightly
// wrong byte total, never to a crash.
type Cache struct {
	root     string
	maxBytes int64
	log      *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	total   int64

	// locks collapses concurrent misses for the same key: ten jobs starting
	// together must not each pull the same 200 MB wheel.
	locks sync.Map
}

// New creates (or adopts) a cache rooted at cfg.Root and indexes whatever is
// already on disk.
func New(cfg Config) (*Cache, error) {
	if cfg.Root == "" {
		return nil, errors.New("pkgcache: empty cache root")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("pkgcache: resolving root %q: %w", cfg.Root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("pkgcache: creating root %q: %w", root, err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	maxBytes := cfg.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}

	c := &Cache{
		root:     root,
		maxBytes: maxBytes,
		log:      log,
		entries:  make(map[string]*entry),
	}
	c.scan()
	// A previous run may have been killed mid-write, or configured with a
	// larger budget. Come back under the cap before serving anything.
	c.evictLocked()
	return c, nil
}

// Root returns the absolute cache root.
func (c *Cache) Root() string { return c.root }

// MaxBytes returns the configured disk budget (negative means unbounded).
func (c *Cache) MaxBytes() int64 { return c.maxBytes }

// Bytes returns the currently indexed on-disk size.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Len returns the number of indexed entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// KeyPath maps a slash-separated cache key onto an absolute path under the
// cache root.
//
// TWO independent traversal defences, because this is the one function a
// hostile URL reaches the filesystem through:
//
//  1. every segment is validated by SafeSegments, which rejects "..", ".",
//     empty segments, backslashes, drive-letter colons and control bytes;
//  2. the RESOLVED path is then required to be under the root, which catches
//     anything the segment rules might not have anticipated (and is the
//     check cmd/ephemerd/cache.go's resolveCacheRoot applies for the same
//     reason).
func (c *Cache) KeyPath(key string) (string, error) {
	segs, err := SafeSegments(key)
	if err != nil {
		return "", fmt.Errorf("cache key %q: %w", key, err)
	}
	for _, s := range segs {
		if strings.HasSuffix(s, metaExt) {
			return "", fmt.Errorf("cache key %q collides with the sidecar extension", key)
		}
		if strings.HasPrefix(s, tmpPrefix) {
			return "", fmt.Errorf("cache key %q collides with the temp-file prefix", key)
		}
	}
	p := filepath.Join(append([]string{c.root}, segs...)...)
	if !isUnder(p, c.root) {
		return "", fmt.Errorf("cache key %q resolves to %q, outside the cache root %q", key, p, c.root)
	}
	return p, nil
}

// isUnder reports whether target is root or a descendant of root. Uses
// filepath.Rel so it is separator-correct on Windows and not fooled by a
// shared string prefix (".../cache/npm2" is not under ".../cache/npm").
func isUnder(target, root string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// Lock returns the per-key mutex used to collapse concurrent misses.
func (c *Cache) Lock(key string) *sync.Mutex {
	v, _ := c.locks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Open returns a readable handle on a cached body plus its metadata.
// Reports ok=false when the entry is absent or unreadable. The caller MUST
// close the returned file.
//
// Opening counts as a use for LRU purposes.
func (c *Cache) Open(key string) (*os.File, Meta, bool) {
	p, err := c.KeyPath(key)
	if err != nil {
		return nil, Meta{}, false
	}
	f, err := os.Open(p) //nolint:gosec // p is validated by KeyPath
	if err != nil {
		return nil, Meta{}, false
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		return nil, Meta{}, false
	}
	meta := c.readMeta(p)
	if meta.Size == 0 {
		meta.Size = info.Size()
	}
	c.touch(key, p)
	return f, meta, true
}

// Read returns a cached body in full. Convenience for the small mutable
// documents (packuments, index pages, version listings) that always have to
// be rewritten in memory anyway.
func (c *Cache) Read(key string) ([]byte, Meta, bool) {
	f, meta, ok := c.Open(key)
	if !ok {
		return nil, Meta{}, false
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, Meta{}, false
	}
	return body, meta, true
}

// Write stores body under key with the given metadata, atomically (temp file
// then rename) so a concurrent reader never observes a partial object.
func (c *Cache) Write(key string, body []byte, meta Meta) error {
	w, err := c.Writer(key)
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		w.Abort()
		return err
	}
	return w.Commit(meta)
}

// touch records a use for LRU ordering. The on-disk mtime is refreshed only
// coarsely (at most once an hour per entry) so a hot cache does not turn
// every read into a write — the same reasoning as a filesystem's relatime.
func (c *Cache) touch(key, path string) {
	now := time.Now()
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return
	}
	stale := now.Sub(e.used) > time.Hour
	e.used = now
	c.mu.Unlock()

	if stale {
		if err := os.Chtimes(path, now, now); err != nil {
			c.log.Debug("refreshing cache entry mtime", "path", path, "error", err)
		}
	}
}

// admit indexes a freshly written entry and runs an eviction pass if the
// budget is now exceeded.
func (c *Cache) admit(key, path string, bytes int64) {
	c.mu.Lock()
	if old, ok := c.entries[key]; ok {
		c.total -= old.bytes
	}
	c.entries[key] = &entry{key: key, path: path, bytes: bytes, used: time.Now()}
	c.total += bytes
	c.evictLocked()
	c.mu.Unlock()
}

// Removed: forget(key). Its doc claimed it ran "when a removal succeeds, and
// when a body turns out to be unreadable", but it had no callers and neither
// case would use it today. Eviction does its own accounting inline (see
// evictLocked, which decrements and deletes under the lock it already holds),
// and an unreadable sidecar deliberately leaves the entry indexed and servable
// rather than dropping it (see readMeta). It was a leftover from an earlier
// design, kept alive only by its comment.

// evictLocked frees least-recently-used entries until the total is at or
// below evictTarget of the budget. Caller must hold c.mu (New calls it
// before the cache is shared, which is equivalent).
//
// An entry whose removal fails — most likely on Windows, where a file being
// served still has an open handle — is left indexed and retried on the next
// pass rather than being dropped from the accounting.
func (c *Cache) evictLocked() {
	if c.maxBytes < 0 || c.total <= c.maxBytes {
		return
	}
	target := int64(float64(c.maxBytes) * evictTarget)

	victims := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		victims = append(victims, e)
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].used.Equal(victims[j].used) {
			return victims[i].key < victims[j].key
		}
		return victims[i].used.Before(victims[j].used)
	})

	var freed int64
	var removed int
	for _, e := range victims {
		if c.total <= target {
			break
		}
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			c.log.Debug("cache eviction could not remove entry; will retry", "path", e.path, "error", err)
			continue
		}
		if err := os.Remove(e.path + metaExt); err != nil && !os.IsNotExist(err) {
			c.log.Debug("cache eviction could not remove sidecar", "path", e.path+metaExt, "error", err)
		}
		pruneEmptyDirs(filepath.Dir(e.path), c.root)
		c.total -= e.bytes
		freed += e.bytes
		removed++
		delete(c.entries, e.key)
	}

	if removed > 0 {
		c.log.Info("package cache evicted least-recently-used entries",
			"root", c.root, "entries", removed, "freed_bytes", freed,
			"total_bytes", c.total, "max_bytes", c.maxBytes)
	}
}

// scan rebuilds the in-memory index from disk. Called once, before the
// cache is shared, so it takes no lock.
func (c *Cache) scan() {
	err := filepath.WalkDir(c.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not fatal — it simply is not indexed.
			return nil //nolint:nilerr // best-effort index rebuild
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			// A write interrupted by a crash or a kill. Nothing references
			// it; reclaim the space now.
			if rerr := os.Remove(p); rerr != nil {
				c.log.Debug("removing stale temp file", "path", p, "error", rerr)
			}
			return nil
		}
		if strings.HasSuffix(name, metaExt) {
			return nil // accounted with its body
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // best-effort index rebuild
		}
		rel, rerr := filepath.Rel(c.root, p)
		if rerr != nil {
			return nil //nolint:nilerr // best-effort index rebuild
		}
		key := filepath.ToSlash(rel)
		size := info.Size()
		if mi, merr := os.Stat(p + metaExt); merr == nil {
			size += mi.Size()
		}
		c.entries[key] = &entry{key: key, path: p, bytes: size, used: info.ModTime()}
		c.total += size
		return nil
	})
	if err != nil {
		c.log.Warn("indexing package cache", "root", c.root, "error", err)
	}
	if len(c.entries) > 0 {
		c.log.Info("package cache indexed", "root", c.root, "entries", len(c.entries), "bytes", c.total)
	}
}

// pruneEmptyDirs removes now-empty directories left behind by an eviction,
// walking up towards (but never removing) root. Best-effort: a non-empty
// directory simply stops the walk.
func pruneEmptyDirs(dir, root string) {
	for isUnder(dir, root) && dir != root {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
