package pkgcache

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCache(t *testing.T, root string, maxBytes int64) *Cache {
	t.Helper()
	c, err := New(Config{
		Root:     root,
		MaxBytes: maxBytes,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestCacheWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)

	if _, _, ok := c.Read("packument/full/express"); ok {
		t.Fatal("empty cache reported a hit")
	}
	body := []byte(`{"name":"express"}`)
	if err := c.Write("packument/full/express", body, Meta{ETag: `"v1"`, ContentType: "application/json"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, meta, ok := c.Read("packument/full/express")
	if !ok {
		t.Fatal("Read reported a miss after Write")
	}
	if string(got) != string(body) {
		t.Errorf("Read = %q, want %q", got, body)
	}
	if meta.ETag != `"v1"` || meta.ContentType != "application/json" {
		t.Errorf("metadata not round-tripped: %+v", meta)
	}
	if meta.Size != int64(len(body)) {
		t.Errorf("meta.Size = %d, want %d", meta.Size, len(body))
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
	if c.Bytes() < int64(len(body)) {
		t.Errorf("Bytes = %d, want at least %d", c.Bytes(), len(body))
	}
}

func TestCacheWriteRejectsUnsafeKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := newTestCache(t, filepath.Join(root, "cache"), 0)

	if err := c.Write("../../pwned", []byte("x"), Meta{}); err == nil {
		t.Fatal("Write accepted a traversing key")
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); err == nil {
		t.Fatal("a traversing key wrote a file outside the cache root")
	}
}

// TestCacheEvictsLRUOverBudget is the disk-bound guarantee: a cache handed
// more bytes than its budget must shed the least-recently-used entries, not
// grow forever.
func TestCacheEvictsLRUOverBudget(t *testing.T) {
	t.Parallel()
	const objSize = 4096
	// Budget for ~10 objects; the eviction target is 90% of it.
	c := newTestCache(t, t.TempDir(), 10*objSize)

	body := make([]byte, objSize)
	// Write 30 objects, touching key 0 along the way so it stays hot.
	for i := range 30 {
		key := fmt.Sprintf("dl/%02d/obj", i)
		if err := c.Write(key, body, Meta{}); err != nil {
			t.Fatalf("Write %s: %v", key, err)
		}
		// Give each entry a distinct access time, in the past and ordered by
		// i, so LRU order is deterministic and the entry just admitted is
		// always the newest.
		c.mu.Lock()
		if e, ok := c.entries[key]; ok {
			e.used = time.Now().Add(-time.Duration(30-i) * time.Second)
		}
		c.mu.Unlock()
	}

	if c.Bytes() > c.MaxBytes() {
		t.Errorf("cache is %d bytes, over its %d byte budget", c.Bytes(), c.MaxBytes())
	}
	if c.Len() == 30 {
		t.Fatal("nothing was evicted")
	}
	if c.Len() == 0 {
		t.Fatal("everything was evicted")
	}
	// The oldest keys must be gone and the newest must survive.
	if _, _, ok := c.Read("dl/00/obj"); ok {
		t.Error("the least-recently-used entry survived eviction")
	}
	if _, _, ok := c.Read("dl/29/obj"); !ok {
		t.Error("the most-recently-used entry was evicted")
	}

	// On-disk reality must match the accounting.
	var onDisk int64
	err := filepath.WalkDir(c.Root(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // best effort
		}
		onDisk += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if onDisk > c.MaxBytes() {
		t.Errorf("on-disk size %d exceeds the budget %d", onDisk, c.MaxBytes())
	}
}

// TestCacheLRUOrderRespectsReads pins that a READ counts as a use: an entry
// that keeps being served must outlive newer entries nobody wants.
func TestCacheLRUOrderRespectsReads(t *testing.T) {
	t.Parallel()
	const objSize = 1024
	c := newTestCache(t, t.TempDir(), 4*objSize)
	body := make([]byte, objSize)

	for _, k := range []string{"dl/a/x", "dl/b/x", "dl/c/x"} {
		if err := c.Write(k, body, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	// Age everything, then re-read "a" so it becomes the most recent.
	old := time.Now().Add(-time.Hour)
	c.mu.Lock()
	for _, e := range c.entries {
		e.used = old
	}
	c.mu.Unlock()
	if _, _, ok := c.Read("dl/a/x"); !ok {
		t.Fatal("expected a hit")
	}

	// Push well past the budget so an eviction pass has to run.
	for i := range 6 {
		if err := c.Write(fmt.Sprintf("dl/new%d/x", i), body, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := c.Read("dl/b/x"); ok {
		t.Error("an untouched old entry survived while newer ones were written")
	}
}

func TestCacheUnboundedWhenNegative(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), -1)
	body := make([]byte, 1024)
	for i := range 50 {
		if err := c.Write(fmt.Sprintf("dl/%02d/x", i), body, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	if c.Len() != 50 {
		t.Errorf("Len = %d, want 50: a negative budget must mean unbounded", c.Len())
	}
}

// TestCacheRescansOnRestart covers the daemon-restart path: the index is
// rebuilt from disk, so the budget still applies to content written by a
// previous run.
func TestCacheRescansOnRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := newTestCache(t, root, -1)
	body := make([]byte, 2048)
	for i := range 20 {
		if err := first.Write(fmt.Sprintf("dl/%02d/x", i), body, Meta{}); err != nil {
			t.Fatal(err)
		}
	}
	wrote := first.Bytes()

	// Reopen with the same (unbounded) budget: everything is re-indexed.
	second := newTestCache(t, root, -1)
	if second.Len() != 20 {
		t.Errorf("after restart Len = %d, want 20", second.Len())
	}
	if second.Bytes() != wrote {
		t.Errorf("after restart Bytes = %d, want %d", second.Bytes(), wrote)
	}
	if _, _, ok := second.Read("dl/07/x"); !ok {
		t.Error("an entry from the previous run was not readable after restart")
	}

	// Reopen with a small budget: the pre-existing content must be trimmed
	// immediately, before anything is served.
	third := newTestCache(t, root, 5*2048)
	if third.Bytes() > third.MaxBytes() {
		t.Errorf("after restart with a smaller budget: %d bytes, budget %d", third.Bytes(), third.MaxBytes())
	}
}

func TestCacheSweepsStaleTempFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stale := filepath.Join(root, tmpPrefix+"interrupted")
	if err := os.WriteFile(stale, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestCache(t, root, 0)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a temp file left by an interrupted write was not swept")
	}
	if c.Bytes() != 0 {
		t.Errorf("Bytes = %d, want 0", c.Bytes())
	}
}

func TestWriterAbortLeavesNothing(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	w, err := c.Writer("dl/aa/bb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	w.Abort()

	if _, _, ok := c.Read("dl/aa/bb"); ok {
		t.Error("an aborted write was readable")
	}
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Errorf("an aborted write was indexed: len=%d bytes=%d", c.Len(), c.Bytes())
	}
	var files []string
	_ = filepath.WalkDir(c.Root(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Errorf("aborted write left files behind: %v", files)
	}
}

func TestCacheConcurrentWrites(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 64*1024)
	body := make([]byte, 1024)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("dl/%02d/x", i%16)
			if err := c.Write(key, body, Meta{}); err != nil {
				t.Errorf("Write: %v", err)
			}
			_, _, _ = c.Read(key)
		}()
	}
	wg.Wait()
	if c.Bytes() > c.MaxBytes() {
		t.Errorf("Bytes = %d, over budget %d", c.Bytes(), c.MaxBytes())
	}
}

func TestPruneEmptyDirsStopsAtRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	pruneEmptyDirs(deep, root)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the cache root itself was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Error("empty intermediate directories were not pruned")
	}
}

func TestIsUnder(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "var", "lib", "ephemerd")
	if !isUnder(root, root) {
		t.Error("a root must be under itself")
	}
	if !isUnder(filepath.Join(root, "cache", "npm"), root) {
		t.Error("a descendant must be under its root")
	}
	if isUnder(root+"2", root) {
		t.Error("a shared string prefix must not count as containment")
	}
	if isUnder(filepath.Dir(root), root) {
		t.Error("a parent must not count as under its child")
	}
}

func TestCacheOpenRejectsDirectory(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	if err := os.MkdirAll(filepath.Join(c.Root(), "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if f, _, ok := c.Open("a/b"); ok {
		_ = f.Close()
		t.Error("Open returned a directory as a cache hit")
	}
}

func TestCacheCorruptSidecarIsIgnored(t *testing.T) {
	t.Parallel()
	c := newTestCache(t, t.TempDir(), 0)
	if err := c.Write("listing/http", []byte("body"), Meta{ETag: `"x"`}); err != nil {
		t.Fatal(err)
	}
	p, err := c.KeyPath("listing/http")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+metaExt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, meta, ok := c.Read("listing/http")
	if !ok || string(body) != "body" {
		t.Fatal("a corrupt sidecar made the body unreadable")
	}
	if meta.ETag != "" || !meta.Fetched.IsZero() {
		t.Errorf("a corrupt sidecar was trusted: %+v", meta)
	}
}

func TestWeakETagAndMatching(t *testing.T) {
	t.Parallel()
	tag := WeakETag([]byte("hello"))
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Errorf("WeakETag = %q, want a quoted value", tag)
	}
	if WeakETag([]byte("hello")) != tag {
		t.Error("WeakETag is not deterministic")
	}
	if WeakETag([]byte("world")) == tag {
		t.Error("WeakETag collided")
	}
	if !MatchesETag(tag, tag) {
		t.Error("an exact tag must match")
	}
	if !MatchesETag("W/"+tag, tag) {
		t.Error("a weak prefix must match")
	}
	if !MatchesETag(`"other", `+tag, tag) {
		t.Error("a tag in a list must match")
	}
	if !MatchesETag("*", tag) {
		t.Error("the wildcard must match")
	}
	if MatchesETag(`"other"`, tag) {
		t.Error("an unrelated tag must not match")
	}
	if MatchesETag("", tag) || MatchesETag(tag, "") {
		t.Error("an empty side must never match")
	}
}
