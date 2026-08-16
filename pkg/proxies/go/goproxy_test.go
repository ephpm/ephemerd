package goproxy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustWrite(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Logf("write error: %v", err)
	}
}

func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Logf("drain error: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close error: %v", err)
	}
}

func startProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Logf("Stop() error: %v", err)
		}
	})
	return p
}

func TestCacheMiss_FetchesFromUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustWrite(t, w, []byte("module-content"))
	}))
	defer upstream.Close()

	p := startProxy(t, upstream.URL)

	resp, err := http.Get("http://" + p.Addr() + "/example.com/mod/@v/v1.0.0.zip")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("close error: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(body) != "module-content" {
		t.Errorf("body = %q, want %q", body, "module-content")
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCacheHit_ServesFromDisk(t *testing.T) {
	var fetchCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		mustWrite(t, w, []byte("from-upstream"))
	}))
	defer upstream.Close()

	p := startProxy(t, upstream.URL)
	url := "http://" + p.Addr() + "/example.com/mod/@v/v1.0.0.mod"

	// First request: cache miss.
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("first GET error: %v", err)
	}
	drainAndClose(t, resp)

	if fetchCount.Load() != 1 {
		t.Fatalf("expected 1 upstream fetch after first request, got %d", fetchCount.Load())
	}

	// Second request: cache hit.
	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("second GET error: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Logf("close error: %v", err)
	}

	if fetchCount.Load() != 1 {
		t.Errorf("expected 1 upstream fetch after second request (cache hit), got %d", fetchCount.Load())
	}
	if string(body) != "from-upstream" {
		t.Errorf("cached body = %q, want %q", body, "from-upstream")
	}
}

func TestMutableEndpoints_NotCached(t *testing.T) {
	var fetchCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		mustWrite(t, w, []byte("version-list"))
	}))
	defer upstream.Close()

	p := startProxy(t, upstream.URL)

	for _, path := range []string{"/@v/list", "/@latest"} {
		fetchCount.Store(0)
		url := "http://" + p.Addr() + "/example.com/mod" + path

		for range 3 {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET %s error: %v", path, err)
			}
			drainAndClose(t, resp)
		}

		if fetchCount.Load() != 3 {
			t.Errorf("%s: expected 3 upstream fetches (not cached), got %d", path, fetchCount.Load())
		}
	}
}

func TestConcurrentRequests_SingleUpstreamFetch(t *testing.T) {
	var fetchCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		mustWrite(t, w, []byte("zip-data"))
	}))
	defer upstream.Close()

	p := startProxy(t, upstream.URL)
	url := "http://" + p.Addr() + "/example.com/mod/@v/v2.0.0.zip"

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				t.Errorf("GET error: %v", err)
				return
			}
			drainAndClose(t, resp)
		}()
	}
	wg.Wait()

	if fetchCount.Load() > 1 {
		t.Errorf("expected at most 1 upstream fetch for concurrent requests, got %d", fetchCount.Load())
	}
}

func TestUpstreamError_ForwardsStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	p := startProxy(t, upstream.URL)

	resp, err := http.Get("http://" + p.Addr() + "/example.com/mod/@v/v9.9.9.zip")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	drainAndClose(t, resp)

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCleanup_WipesCacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.MkdirAll(cacheDir+"/test", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheDir+"/test/file.zip", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(Config{
		CacheDir:   cacheDir,
		Upstream:   "http://unused",
		ListenAddr: "127.0.0.1:0",
		Cleanup:    true,
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("cache dir should be removed after Stop with cleanup=true")
	}
}

func TestNoCleanup_PreservesCacheDir(t *testing.T) {
	cacheDir := t.TempDir()

	p := New(Config{
		CacheDir:   cacheDir,
		Upstream:   "http://unused",
		ListenAddr: "127.0.0.1:0",
		Cleanup:    false,
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache dir should be preserved after Stop with cleanup=false: %v", err)
	}
}

// --- Path traversal ---
//
// cachePath turns a job-controlled, already-DECODED URL path into a
// filesystem path. filepath.Join cleans, so a "../" segment used to traverse
// straight out of the cache root, and the sha256 prefix does not help because
// it is derived from the same string. Nothing pinned that: containment
// happened to hold only because http.ServeMux redirects unclean paths before
// the handler runs. These tests assert containment as a property of
// cachePath itself.

// hostilePaths are the inputs a job could put on the wire. Percent-encoded
// forms appear here in DECODED form because net/http hands the handler
// r.URL.Path already decoded — that decoding is the whole reason "%2e%2e%2f"
// is interesting.
//
// mustReject marks the ones that carry a traversal/encoding trick and are
// refused outright. The rest are merely unusual (a leading "/etc/passwd" is
// just an ordinary relative key once the leading slash is stripped) and only
// have to stay contained.
var hostilePaths = []struct {
	name       string
	path       string
	mustReject bool
}{
	{"dotdot segments", "/../../../etc/passwd", true},
	{"dotdot mid-path", "/example.com/mod/../../../../etc/passwd", true},
	{"decoded percent-encoded dotdot", "/../..//etc/passwd", true},
	{"trailing dotdot", "/example.com/mod/..", true},
	{"single dot segments", "/./././etc/passwd", true},
	{"absolute unix path", "//etc/passwd", true},
	{"etc passwd plain", "/etc/passwd", false},
	{"backslash separators", `/..\..\..\Windows\System32\config\SAM`, true},
	{"mixed separators", `/example.com\..\..\evil.zip`, true},
	{"nul byte", "/example.com/mod\x00/@v/v1.0.0.zip", true},
	{"newline", "/example.com/mod\n/@v/v1.0.0.zip", true},
	{"carriage return", "/example.com/\r/@v/v1.0.0.zip", true},
	{"windows drive letter", `/C:\Windows\System32\drivers\etc\hosts`, true},
	{"windows drive after slash", "/C:/Windows/System32/drivers/etc/hosts", true},
	{"unc-ish share", "//server/share/evil.zip", true},
	{"unc-ish backslashes", `/\\server\share\evil.zip`, true},
	{"ntfs alternate data stream", "/example.com/mod/@v/v1.0.0.zip:evil", true},
	{"empty path", "", true},
	{"root only", "/", true},
	{"very long path", "/" + strings.Repeat("a", 64*1024) + "/@v/v1.0.0.zip", true},
	{"very long single segment", "/" + strings.Repeat("b", 300) + "/@v/v1.0.0.zip", true},
	{"deep dotdot climb", "/" + strings.Repeat("../", 200) + "etc/passwd", true},
}

func TestCachePath_HostileInputsStayUnderCacheDir(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "gomod")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", Log: testLogger()})

	for _, tt := range hostilePaths {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.cachePath(tt.path)
			if err != nil {
				if !errors.Is(err, errUnsafePath) {
					t.Errorf("cachePath(%q) error = %v, want it to wrap errUnsafePath", tt.path, err)
				}
				return // rejected outright: the safest outcome
			}
			// If it was accepted, it MUST be strictly inside the cache dir.
			if !underRoot(cacheDir, got) {
				t.Fatalf("cachePath(%q) = %q, which escapes cache dir %q", tt.path, got, cacheDir)
			}
			if filepath.Clean(got) == filepath.Clean(cacheDir) {
				t.Fatalf("cachePath(%q) = the cache dir itself", tt.path)
			}
		})
	}
}

// TestCachePath_RejectsHostileInputs is the stronger claim for the inputs
// that carry an actual trick: they are refused, not silently rewritten into
// some neighbouring path. Kept separate from the containment test so a
// future decision to sanitize instead of reject only fails this one.
func TestCachePath_RejectsHostileInputs(t *testing.T) {
	p := New(Config{CacheDir: t.TempDir(), Upstream: "http://unused", Log: testLogger()})
	for _, tt := range hostilePaths {
		if !tt.mustReject {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			if got, err := p.cachePath(tt.path); err == nil {
				t.Errorf("cachePath(%q) = %q, want an error", tt.path, got)
			}
		})
	}
}

// TestCachePath_AcceptsRealModulePaths guards against the traversal rules
// being so strict they break the protocol this proxy exists to serve.
func TestCachePath_AcceptsRealModulePaths(t *testing.T) {
	cacheDir := t.TempDir()
	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", Log: testLogger()})

	paths := []string{
		"/example.com/mod/@v/v1.0.0.zip",
		"/github.com/ephpm/ephemerd/@v/v0.1.7.info",
		"/github.com/!burnt!sushi/toml/@v/v1.4.0.mod",
		"/gopkg.in/yaml.v3/@v/v3.0.1.zip",
		"/github.com/some/mod/v2/@v/v2.1.0-rc.1.zip",
		"/golang.org/x/net/@v/v0.0.0-20220225172249-27dd8689420f.mod",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got, err := p.cachePath(path)
			if err != nil {
				t.Fatalf("cachePath(%q) error = %v, want it accepted", path, err)
			}
			if !underRoot(cacheDir, got) {
				t.Fatalf("cachePath(%q) = %q, outside cache dir %q", path, got, cacheDir)
			}
		})
	}
}

// TestCachePath_IsDeterministic pins that the same request always maps to
// the same file — the cache would silently stop hitting otherwise.
func TestCachePath_IsDeterministic(t *testing.T) {
	p := New(Config{CacheDir: t.TempDir(), Upstream: "http://unused", Log: testLogger()})
	const path = "/example.com/mod/@v/v1.0.0.zip"
	a, err := p.cachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.cachePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("cachePath not deterministic: %q vs %q", a, b)
	}
}

// TestHandler_HostilePathsWriteNothingOutsideCacheDir drives the real HTTP
// handler with the encoded forms as well, so the guarantee is checked end to
// end rather than only at the cachePath boundary. The upstream would happily
// serve every one of these, so anything landing outside the cache dir is a
// real write-primitive.
func TestHandler_HostilePathsWriteNothingOutsideCacheDir(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustWrite(t, w, []byte("PWNED"))
	}))
	defer upstream.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "gomod")
	sentinel := filepath.Join(root, "outside")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}

	p := New(Config{
		CacheDir:      cacheDir,
		Upstream:      upstream.URL,
		ListenAddr:    "127.0.0.1:0",
		PruneInterval: -1, // no background pass racing the assertions
		Log:           testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		if err := p.Stop(); err != nil {
			t.Logf("Stop() error: %v", err)
		}
	}()

	requests := []string{
		"/../outside/pwned.zip",
		"/..%2f..%2foutside%2fpwned.zip",
		"/%2e%2e%2f%2e%2e%2foutside%2fpwned.zip",
		"/example.com/mod/@v/..%2f..%2f..%2foutside%2fpwned.mod",
		"/..%5c..%5coutside%5cpwned.zip",
		"/%00/../outside/pwned.info",
		"/C:%5Coutside%5Cpwned.zip",
	}
	for _, req := range requests {
		resp, err := http.Get("http://" + p.Addr() + req) //nolint:noctx // short-lived test request
		if err != nil {
			// A path net/http itself refuses to put on the wire is fine —
			// the request never reached the daemon at all.
			t.Logf("GET %s: %v (request rejected client-side)", req, err)
			continue
		}
		drainAndClose(t, resp)
	}

	// Nothing may exist under the sentinel dir.
	entries, err := os.ReadDir(sentinel)
	if err != nil {
		t.Fatalf("reading sentinel dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("handler wrote %d entry/entries outside the cache dir: %v", len(entries), entries)
	}
	// And every file that was created must be inside the cache dir.
	walkAssertUnder(t, root, cacheDir)
}

// walkAssertUnder fails if any regular file under root is not under want.
func walkAssertUnder(t *testing.T, root, want string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are not our concern here
		}
		if !underRoot(want, path) {
			t.Errorf("file %q was created outside the cache dir %q", path, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", root, err)
	}
}

// TestUnderRoot_SeparatorAware pins the containment predicate itself,
// including the shared-prefix case a strings.HasPrefix check gets wrong.
func TestUnderRoot_SeparatorAware(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "gomod")
	tests := []struct {
		target string
		want   bool
	}{
		{root, true},
		{filepath.Join(root, "ab12", "example.com", "v1.0.0.zip"), true},
		{filepath.Join(root, ".", "x"), true},
		{filepath.Join(base, "gomod2", "x"), false},
		{filepath.Join(base, "other"), false},
		{filepath.Join(root, "..", "escaped"), false},
		{base, false},
	}
	for _, tt := range tests {
		if got := underRoot(root, tt.target); got != tt.want {
			t.Errorf("underRoot(%q, %q) = %v, want %v", root, tt.target, got, tt.want)
		}
	}
}

// --- Cache size bound / eviction ---

// writeCacheFile creates a cache file of the given size with a modtime aged
// by `age`, and returns its path.
func writeCacheFile(t *testing.T, cacheDir, rel string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(cacheDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// cacheSize sums every regular file under dir.
func cacheSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // missing entries count as zero
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("sizing %q: %v", dir, err)
	}
	return total
}

// TestPrune_EvictsLRUUntilUnderCap is the core of the bound: the oldest
// files go first, and only as many as it takes to get under the cap.
func TestPrune_EvictsLRUUntilUnderCap(t *testing.T) {
	cacheDir := t.TempDir()
	oldest := writeCacheFile(t, cacheDir, "aa/old.zip", 1000, 72*time.Hour)
	middle := writeCacheFile(t, cacheDir, "bb/mid.zip", 1000, 24*time.Hour)
	newest := writeCacheFile(t, cacheDir, "cc/new.zip", 1000, time.Minute)

	p := New(Config{
		CacheDir:      cacheDir,
		Upstream:      "http://unused",
		MaxCacheBytes: 2500,
		PruneInterval: -1,
		Log:           testLogger(),
	})

	freed, err := p.Prune()
	if err != nil {
		t.Fatalf("Prune() error: %v", err)
	}
	if freed != 1000 {
		t.Errorf("freed = %d, want 1000 (exactly one file)", freed)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("least-recently-used file should have been evicted: %v", err)
	}
	for _, keep := range []string{middle, newest} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("file %q should have survived: %v", keep, err)
		}
	}
	if got := cacheSize(t, cacheDir); got > 2500 {
		t.Errorf("cache is %d bytes after prune, want <= 2500", got)
	}
	// The directory the evicted file lived in is gone too.
	if _, err := os.Stat(filepath.Dir(oldest)); !os.IsNotExist(err) {
		t.Errorf("emptied cache dir should have been removed: %v", err)
	}
	// ...but the cache root itself is still there; the proxy serves from it.
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache root must survive pruning: %v", err)
	}
}

// TestPrune_BoundsAFillingCache is the availability case from the issue: one
// job pulling module after module must not be able to run the node's disk
// away.
func TestPrune_BoundsAFillingCache(t *testing.T) {
	cacheDir := t.TempDir()
	const (
		fileSize = 4096
		files    = 200
		cap_     = 50 * fileSize
	)
	for i := range files {
		writeCacheFile(t, cacheDir,
			fmt.Sprintf("%02x/example.com/mod/@v/v1.0.%d.zip", i%16, i),
			fileSize, time.Duration(files-i)*time.Minute)
	}
	if got := cacheSize(t, cacheDir); got != files*fileSize {
		t.Fatalf("setup: cache is %d bytes, want %d", got, files*fileSize)
	}

	p := New(Config{
		CacheDir:      cacheDir,
		Upstream:      "http://unused",
		MaxCacheBytes: cap_,
		PruneInterval: -1,
		Log:           testLogger(),
	})
	if _, err := p.Prune(); err != nil {
		t.Fatalf("Prune() error: %v", err)
	}

	got := cacheSize(t, cacheDir)
	if got > cap_ {
		t.Errorf("cache is %d bytes after prune, want <= %d", got, cap_)
	}
	// Pruning must not be a wipe — the point is a WARM bounded cache.
	if got == 0 {
		t.Error("prune emptied the cache entirely; it should evict down to the cap, not clear it")
	}
	// A second pass with nothing to do frees nothing.
	freed, err := p.Prune()
	if err != nil {
		t.Fatalf("second Prune() error: %v", err)
	}
	if freed != 0 {
		t.Errorf("second Prune() freed %d bytes, want 0 (already under cap)", freed)
	}
}

func TestPrune_UnderCapIsNoOp(t *testing.T) {
	cacheDir := t.TempDir()
	kept := writeCacheFile(t, cacheDir, "aa/small.zip", 100, time.Hour)

	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", MaxCacheBytes: 1 << 20, PruneInterval: -1, Log: testLogger()})
	freed, err := p.Prune()
	if err != nil {
		t.Fatalf("Prune() error: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("file under the cap should be untouched: %v", err)
	}
}

func TestPrune_NegativeCapIsUnbounded(t *testing.T) {
	cacheDir := t.TempDir()
	kept := writeCacheFile(t, cacheDir, "aa/big.zip", 4096, 100*time.Hour)

	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", MaxCacheBytes: -1, PruneInterval: -1, Log: testLogger()})
	freed, err := p.Prune()
	if err != nil {
		t.Fatalf("Prune() error: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 with an explicitly unbounded cache", freed)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("nothing should be evicted when the cap is negative: %v", err)
	}
}

// TestPrune_SkipsInFlightTempFiles: the write path publishes with os.Rename
// from a ".goproxy-*" temp file. Evicting one mid-write would break that
// rename, so temp files count toward the size but are never removed.
func TestPrune_SkipsInFlightTempFiles(t *testing.T) {
	cacheDir := t.TempDir()
	tmp := writeCacheFile(t, cacheDir, "aa/"+tmpPrefix+"123", 2000, 99*time.Hour)
	old := writeCacheFile(t, cacheDir, "bb/old.zip", 2000, 98*time.Hour)

	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", MaxCacheBytes: 2500, PruneInterval: -1, Log: testLogger()})
	if _, err := p.Prune(); err != nil {
		t.Fatalf("Prune() error: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("in-flight temp file must not be evicted: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the evictable file should have gone instead: %v", err)
	}
}

func TestPrune_MissingCacheDirIsNotAnError(t *testing.T) {
	p := New(Config{
		CacheDir:      filepath.Join(t.TempDir(), "does-not-exist"),
		Upstream:      "http://unused",
		MaxCacheBytes: 1,
		PruneInterval: -1,
		Log:           testLogger(),
	})
	if freed, err := p.Prune(); err != nil || freed != 0 {
		t.Errorf("Prune() on a missing cache dir = (%d, %v), want (0, nil)", freed, err)
	}
}

// TestTouch_RefreshesStaleModtime pins the LRU key. Cache hits never rewrite
// the file (the write path renames), so without this refresh a module that is
// served daily still looks as old as its first download and gets evicted
// ahead of something nothing has touched since.
func TestTouch_RefreshesStaleModtime(t *testing.T) {
	cacheDir := t.TempDir()
	path := writeCacheFile(t, cacheDir, "aa/hot.zip", 10, 48*time.Hour)
	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", PruneInterval: -1, Log: testLogger()})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	p.touch(path, info)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(after.ModTime()) > time.Minute {
		t.Errorf("modtime = %v, want refreshed to ~now", after.ModTime())
	}
}

// TestTouch_ThrottlesRecentFiles: a hot module must not cost a metadata
// write on every single request.
func TestTouch_ThrottlesRecentFiles(t *testing.T) {
	cacheDir := t.TempDir()
	path := writeCacheFile(t, cacheDir, "aa/fresh.zip", 10, time.Minute)
	p := New(Config{CacheDir: cacheDir, Upstream: "http://unused", PruneInterval: -1, Log: testLogger()})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := info.ModTime()
	p.touch(path, info)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before) {
		t.Errorf("modtime changed (%v -> %v); a file touched within the throttle window should be left alone", before, after.ModTime())
	}
}

// TestCacheHit_TouchesModtime wires the two together through the handler:
// serving from cache marks the file as recently used.
func TestCacheHit_TouchesModtime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mustWrite(t, w, []byte("module-content"))
	}))
	defer upstream.Close()

	cacheDir := t.TempDir()
	p := New(Config{
		CacheDir:      cacheDir,
		Upstream:      upstream.URL,
		ListenAddr:    "127.0.0.1:0",
		PruneInterval: -1,
		Log:           testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		if err := p.Stop(); err != nil {
			t.Logf("Stop() error: %v", err)
		}
	}()

	const modPath = "/example.com/mod/@v/v1.0.0.zip"
	resp, err := http.Get("http://" + p.Addr() + modPath) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	drainAndClose(t, resp)

	file, err := p.cachePath(modPath)
	if err != nil {
		t.Fatalf("cachePath error: %v", err)
	}
	// Age it past the throttle window, then hit the cache again.
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file, stale, stale); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get("http://" + p.Addr() + modPath) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("second GET error: %v", err)
	}
	drainAndClose(t, resp)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("cache hit left modtime at %v; LRU would evict a hot module first", info.ModTime())
	}
}
