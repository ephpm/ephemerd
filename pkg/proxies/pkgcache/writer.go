package pkgcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Meta is the sidecar record stored beside every cached body. It carries the
// upstream validators, so a mutable entry that has aged out can be
// revalidated with a conditional GET (one 304 instead of a re-download), and
// the content type, so the proxy never has to sniff.
type Meta struct {
	// ETag / LastModified are the upstream validators, replayed on
	// revalidation and echoed to clients that do their own caching.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	// ContentType is the upstream Content-Type, or a proxy-chosen one for
	// documents the proxy rewrote.
	ContentType string `json:"content_type,omitempty"`
	// Fetched is when the body was last known-good from upstream. Drives
	// the TTL for mutable entries.
	Fetched time.Time `json:"fetched"`
	// Size is the body length in bytes.
	Size int64 `json:"size,omitempty"`
	// URL is the upstream URL this body came from. Not used for lookups —
	// artifact keys are hashes, so without this the cache is unreadable by
	// a human debugging a node.
	URL string `json:"url,omitempty"`
}

// Writer stages a cache body in a temp file and installs it atomically on
// Commit. Bodies are streamed rather than buffered: a PyPI wheel or an npm
// tarball can be hundreds of megabytes and must never be held in RAM,
// especially with several jobs downloading at once.
type Writer struct {
	cache  *Cache
	key    string
	target string
	tmp    *os.File
	n      int64
	done   bool
}

// Writer opens a staged writer for key.
func (c *Cache) Writer(key string) (*Writer, error) {
	target, err := c.KeyPath(key)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("staging cache entry %q: %w", key, err)
	}
	return &Writer{cache: c, key: key, target: target, tmp: tmp}, nil
}

// Write implements io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.tmp.Write(p)
	w.n += int64(n)
	return n, err
}

// Abort discards the staged entry. Safe to call after Commit (no-op).
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	name := w.tmp.Name()
	if err := w.tmp.Close(); err != nil {
		w.cache.log.Debug("closing aborted cache entry", "path", name, "error", err)
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		w.cache.log.Debug("removing aborted cache entry", "path", name, "error", err)
	}
}

// Commit installs the staged body and its sidecar, then indexes the entry —
// which may trigger an LRU eviction pass if the cache is now over budget.
func (w *Writer) Commit(meta Meta) error {
	if w.done {
		return fmt.Errorf("cache entry %q already finalised", w.key)
	}
	w.done = true
	name := w.tmp.Name()
	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("closing cache entry %q: %w", w.key, err)
	}
	if err := os.Rename(name, w.target); err != nil {
		// On Windows a rename over a file another goroutine (or another
		// process) still has open fails with ERROR_ACCESS_DENIED — which is
		// exactly the case where two jobs pull the same tarball at once, or
		// one is being served while a mutable document refreshes. If the
		// destination exists, the cache already holds a copy: keep it, drop
		// the staged one, and carry on. Only a rename that leaves NOTHING in
		// place is an error.
		_ = os.Remove(name)
		if info, serr := os.Stat(w.target); serr == nil && !info.IsDir() {
			w.cache.log.Debug("cache entry already present; keeping the existing copy",
				"key", w.key, "error", err)
			w.cache.admit(w.key, w.target, info.Size())
			return nil
		}
		return fmt.Errorf("installing cache entry %q: %w", w.key, err)
	}

	meta.Size = w.n
	if meta.Fetched.IsZero() {
		meta.Fetched = time.Now()
	}
	metaBytes := w.cache.writeMeta(w.target, meta)
	w.cache.admit(w.key, w.target, w.n+metaBytes)
	return nil
}

// writeMeta persists the sidecar and reports its on-disk size. Best-effort:
// a missing sidecar only costs one revalidation next time, so a failure is
// logged and swallowed rather than failing the request that produced it.
func (c *Cache) writeMeta(bodyPath string, meta Meta) int64 {
	raw, err := json.Marshal(meta)
	if err != nil {
		c.log.Debug("encoding cache metadata", "path", bodyPath, "error", err)
		return 0
	}
	target := bodyPath + metaExt
	tmp, err := os.CreateTemp(filepath.Dir(bodyPath), tmpPrefix+"*")
	if err != nil {
		c.log.Debug("staging cache metadata", "path", target, "error", err)
		return 0
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		c.log.Debug("writing cache metadata", "path", target, "error", err)
		return 0
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		c.log.Debug("closing cache metadata", "path", target, "error", err)
		return 0
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		c.log.Debug("installing cache metadata", "path", target, "error", err)
		return 0
	}
	return int64(len(raw))
}

// readMeta loads a sidecar. A body with no (or a corrupt) sidecar is still
// usable: the zero Fetched time makes a mutable entry revalidate and leaves
// an immutable one servable as-is.
func (c *Cache) readMeta(bodyPath string) Meta {
	raw, err := os.ReadFile(bodyPath + metaExt) //nolint:gosec // bodyPath comes from KeyPath
	if err != nil {
		return Meta{}
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		c.log.Debug("discarding unreadable cache metadata", "path", bodyPath, "error", err)
		return Meta{}
	}
	return m
}

// refreshMeta rewrites a sidecar in place (used after a 304, to reset the
// freshness clock without touching the body).
func (c *Cache) refreshMeta(key string, meta Meta) {
	p, err := c.KeyPath(key)
	if err != nil {
		return
	}
	c.writeMeta(p, meta)
}
