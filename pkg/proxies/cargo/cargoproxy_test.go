package cargoproxy

import (
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

// NOTE: every test here runs against an httptest fake upstream. Nothing in
// this file touches crates.io, static.rust-lang.org, or any other network.

// alwaysRevalidate is a negative TTL, which Config documents as "never serve
// a mutable entry without a conditional GET". Tests use it to reach the
// aged-out state deterministically instead of sleeping. A ZERO TTL will not
// do: New() reads zero as "unset" and substitutes DefaultIndexTTL.
const alwaysRevalidate = -1 * time.Second

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeUpstream is a stand-in for index.crates.io / static.crates.io /
// static.rust-lang.org, with per-path hit counting so tests can assert that
// the cache actually prevented a second fetch.
type fakeUpstream struct {
	*httptest.Server

	mu       sync.Mutex
	hits     map[string]int
	bodies   map[string]string
	etags    map[string]string
	status   map[string]int
	failing  atomic.Bool
	condHits atomic.Int64 // conditional (If-None-Match) requests received
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		hits:   map[string]int{},
		bodies: map[string]string{},
		etags:  map[string]string{},
		status: map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		body, known := f.bodies[r.URL.Path]
		etag := f.etags[r.URL.Path]
		status := f.status[r.URL.Path]
		f.mu.Unlock()

		if f.failing.Load() {
			http.Error(w, "upstream down", http.StatusInternalServerError)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if !known {
			http.NotFound(w, r)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			f.condHits.Add(1)
			if etag != "" && inm == etag {
				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) set(path, body, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[path] = body
	if etag != "" {
		f.etags[path] = etag
	}
}

func (f *fakeUpstream) setStatus(path string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = code
}

func (f *fakeUpstream) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// newTestProxy starts a proxy bound to loopback with both upstreams pointed
// at the fake, and returns it plus its base URL.
func newTestProxy(t *testing.T, up *fakeUpstream, mutate func(*Config)) (*Proxy, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		CacheDir:       filepath.Join(dir, "cache"),
		ConfDir:        filepath.Join(dir, "conf"),
		IndexUpstream:  up.URL,
		RustupUpstream: up.URL,
		ListenAddr:     "127.0.0.1:0",
		IndexTTL:       time.Hour,
		ContainerOS:    "linux",
		// Off by default so the routing tests are not racing a background
		// goroutine that rewrites the config file. The watchdog gets its own
		// test.
		HealthInterval: -1,
		Log:            discardLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p := New(cfg)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return p, "http://" + p.Addr()
}

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request for %s: %v", url, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	// Do not follow redirects: the fail-open path is a 307 we want to assert.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s: %v", url, err)
	}
	return resp, string(body)
}

// --- index (mutable) -------------------------------------------------------

// TestIndex_CachedWithinTTL: the sparse index is mutable, but inside the TTL
// it must be served from disk with no upstream request at all.
func TestIndex_CachedWithinTTL(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", `{"name":"serde","vers":"1.0.203"}`, `"v1"`)
	_, base := newTestProxy(t, up, nil)

	for i := range 3 {
		resp, body := get(t, base+"/index/se/rd/serde", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
		if !strings.Contains(body, `"serde"`) {
			t.Fatalf("request %d: body = %q", i, body)
		}
	}
	if got := up.hitCount("/se/rd/serde"); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (later requests must come from cache)", got)
	}
}

// TestIndex_RevalidatesAfterTTL: past the TTL the proxy must NOT serve stale
// data blindly — it must issue a conditional GET, and it must pick up new
// content when the entry actually changed.
func TestIndex_RevalidatesAfterTTL(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", "v1-body", `"v1"`)
	_, base := newTestProxy(t, up, func(c *Config) { c.IndexTTL = alwaysRevalidate })

	if _, body := get(t, base+"/index/se/rd/serde", nil); body != "v1-body" {
		t.Fatalf("first body = %q, want v1-body", body)
	}

	// Unchanged upstream: the revalidation must be conditional and the
	// cached bytes reused.
	before := up.condHits.Load()
	if _, body := get(t, base+"/index/se/rd/serde", nil); body != "v1-body" {
		t.Fatalf("second body = %q, want v1-body", body)
	}
	if up.condHits.Load() <= before {
		t.Error("revalidation was not conditional (no If-None-Match sent)")
	}

	// Changed upstream: a new version was published, the proxy must see it.
	up.set("/se/rd/serde", "v2-body", `"v2"`)
	if _, body := get(t, base+"/index/se/rd/serde", nil); body != "v2-body" {
		t.Errorf("after upstream change body = %q, want v2-body (stale index served)", body)
	}
}

// TestIndex_ServesClient304 keeps warm-cache traffic cheap: Cargo keeps its
// own copy and sends If-None-Match, which must produce a bodyless 304.
func TestIndex_ServesClient304(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", "body", `"v1"`)
	_, base := newTestProxy(t, up, nil)

	resp, _ := get(t, base+"/index/se/rd/serde", nil)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("proxy did not surface the upstream ETag")
	}

	resp2, body2 := get(t, base+"/index/se/rd/serde", map[string]string{"If-None-Match": etag})
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", resp2.StatusCode)
	}
	if body2 != "" {
		t.Errorf("304 carried a body: %q", body2)
	}
}

// TestIndex_FailOpenToStaleCache: a registry outage must not turn into a red
// CI job when we already hold the data.
func TestIndex_FailOpenToStaleCache(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", "cached-body", `"v1"`)
	_, base := newTestProxy(t, up, func(c *Config) { c.IndexTTL = alwaysRevalidate })

	if _, body := get(t, base+"/index/se/rd/serde", nil); body != "cached-body" {
		t.Fatalf("priming failed: %q", body)
	}

	up.failing.Store(true)
	resp, body := get(t, base+"/index/se/rd/serde", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (stale cache should be served)", resp.StatusCode)
	}
	if body != "cached-body" {
		t.Errorf("body = %q, want the stale cached copy", body)
	}
}

// TestIndex_FailsOpenWithRedirect is the fix for the blocker that stalled
// this change: GOPROXY has "|direct", Cargo has nothing. Once
// source.crates-io is replaced there is no second source to fall back to, so
// an index request the proxy cannot satisfy MUST become a redirect to the
// real registry rather than a 502 — otherwise an upstream blip while the
// cache is cold turns into a red build for every Rust job on the node.
func TestIndex_FailsOpenWithRedirect(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", "body", `"v1"`)
	_, base := newTestProxy(t, up, nil)

	// Upstream broken, nothing cached: the worst case.
	up.failing.Store(true)

	resp, _ := get(t, base+"/index/se/rd/serde", nil)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 — a 502 here has no fallback and fails the build", resp.StatusCode)
	}
	if want := up.URL + "/se/rd/serde"; resp.Header.Get("Location") != want {
		t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), want)
	}
}

// TestRegistryConfig_FailsOpenWithRedirect: config.json is the FIRST thing
// Cargo asks for, so failing it fails everything after it. Redirected to the
// real registry, Cargo gets a genuine document whose "dl" points at the
// crates.io CDN and simply bypasses the cache for that build.
func TestRegistryConfig_FailsOpenWithRedirect(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)
	up.failing.Store(true)

	resp, _ := get(t, base+"/index/config.json", nil)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	if want := up.URL + "/config.json"; resp.Header.Get("Location") != want {
		t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), want)
	}
}

// TestIndex_UpstreamNotFoundIsPassedThrough: a crate that does not exist is
// a 404, not a 502 — Cargo needs to distinguish them.
func TestIndex_UpstreamNotFoundIsPassedThrough(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)

	resp, _ := get(t, base+"/index/no/pe/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestIndex_RejectsTraversal keeps a hostile request from writing outside
// the cache root.
func TestIndex_RejectsTraversal(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)

	// http.Client normalises "..", so build the raw request by hand.
	resp, _ := get(t, base+"/index/se/rd/%2e%2e%2f%2e%2e%2fescape", nil)
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 400 or 404 for a traversal attempt", resp.StatusCode)
	}
}

// --- config.json rewrite ---------------------------------------------------

func TestRegistryConfig_RewritesDLToProxy(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+"UPSTREAM"+`/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io"}`, "")
	p, base := newTestProxy(t, up, nil)

	resp, body := get(t, base+"/index/config.json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"dl":"`+p.advertiseBase()+`/crates"`) {
		t.Errorf("dl was not rewritten to the proxy: %s", body)
	}
	if !strings.Contains(body, `"api":"https://crates.io"`) {
		t.Errorf("api must be preserved so cargo publish still works: %s", body)
	}
}

// --- crate tarballs (immutable) --------------------------------------------

func TestCrate_CachedForeverAfterFirstFetch(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	up.set("/crates/serde/serde-1.0.203.crate", "TARBALL-BYTES", `"e1"`)
	_, base := newTestProxy(t, up, func(c *Config) { c.IndexTTL = alwaysRevalidate })

	// Cargo asks for config.json first; that is what teaches us the dl template.
	if resp, _ := get(t, base+"/index/config.json", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("config.json status = %d", resp.StatusCode)
	}

	for i := range 3 {
		resp, body := get(t, base+"/crates/serde/1.0.203/download", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
		if body != "TARBALL-BYTES" {
			t.Fatalf("request %d: body = %q", i, body)
		}
	}

	// Immutable content: exactly one upstream fetch, and no revalidation
	// even though the index TTL is zero.
	if got := up.hitCount("/crates/serde/serde-1.0.203.crate"); got != 1 {
		t.Errorf("upstream hits = %d, want 1 — .crate tarballs are immutable and must never be refetched", got)
	}
}

// TestCrate_ImmutableCacheSurvivesUpstreamOutage: once cached, a tarball is
// served with the network completely gone.
func TestCrate_ImmutableCacheSurvivesUpstreamOutage(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	up.set("/crates/serde/serde-1.0.203.crate", "TARBALL-BYTES", "")
	_, base := newTestProxy(t, up, nil)

	get(t, base+"/index/config.json", nil)
	get(t, base+"/crates/serde/1.0.203/download", nil)

	up.failing.Store(true)
	resp, body := get(t, base+"/crates/serde/1.0.203/download", nil)
	if resp.StatusCode != http.StatusOK || body != "TARBALL-BYTES" {
		t.Errorf("status = %d body = %q, want a cache hit despite the outage", resp.StatusCode, body)
	}
}

// TestCrate_FailsOpenWithRedirect is the core fail-open guarantee: when the
// proxy cannot produce the tarball, the job must be sent to the real CDN
// rather than handed an error.
func TestCrate_FailsOpenWithRedirect(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	_, base := newTestProxy(t, up, nil)
	get(t, base+"/index/config.json", nil)

	// 500 from upstream with nothing cached.
	up.setStatus("/crates/serde/serde-9.9.9.crate", http.StatusInternalServerError)

	resp, _ := get(t, base+"/crates/serde/9.9.9/download", nil)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (fail open to upstream)", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasSuffix(loc, "/crates/serde/serde-9.9.9.crate") {
		t.Errorf("Location = %q, want the upstream tarball URL", loc)
	}
}

// TestCrate_UnknownVersionIs404 keeps a genuine "no such version" distinct
// from an outage — redirecting on a 404 would just make Cargo fetch a 404.
func TestCrate_UnknownVersionIs404(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	_, base := newTestProxy(t, up, nil)
	get(t, base+"/index/config.json", nil)

	resp, _ := get(t, base+"/crates/serde/0.0.0/download", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCrate_RejectsMalformedPaths(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)

	for _, p := range []string{
		"/crates/serde/1.0.0",
		"/crates/serde/1.0.0/fetch",
		"/crates/serde%20x/1.0.0/download",
	} {
		resp, _ := get(t, base+p, nil)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200, want a rejection", p)
		}
	}
}

// TestCrate_ConcurrentMissesFetchUpstreamOnce pins the single-flight
// behaviour that stops N parallel jobs pulling the same tarball N times.
func TestCrate_ConcurrentMissesFetchUpstreamOnce(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	up.set("/crates/tokio/tokio-1.2.3.crate", "BIG-TARBALL", "")
	_, base := newTestProxy(t, up, nil)
	get(t, base+"/index/config.json", nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(base + "/crates/tokio/1.2.3/download")
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := up.hitCount("/crates/tokio/tokio-1.2.3.crate"); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (concurrent misses must be collapsed)", got)
	}
}

// TestStop_IsPromptAfterConcurrentBurst is the regression guard for the
// intermittent CI failure that stalled this change:
//
//	--- FAIL: TestCrate_ConcurrentMissesFetchUpstreamOnce (5.01s)
//	    Stop: shutting down cargo proxy: context deadline exceeded
//
// A burst of parallel requests makes Go's http.Transport dial more
// connections than it uses; the spares are accepted but never send a request,
// and http.Server.Shutdown refuses to close such a connection for five
// seconds. Stop must not inherit that wait. See proxies.Server.
//
// The 5.01s in the failure is not a coincidence — it is exactly net/http's
// "new connection" grace — so a threshold of two seconds distinguishes the
// bug from a slow machine without being flaky.
func TestStop_IsPromptAfterConcurrentBurst(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/crates/{crate}/{crate}-{version}.crate"}`, "")
	up.set("/crates/tokio/tokio-1.2.3.crate", "BIG-TARBALL", "")

	dir := t.TempDir()
	p := New(Config{
		CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
		IndexUpstream: up.URL, RustupUpstream: up.URL,
		ListenAddr: "127.0.0.1:0", ContainerOS: "linux",
		HealthInterval: -1, Log: discardLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + p.Addr()
	get(t, base+"/index/config.json", nil)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(base + "/crates/tokio/1.2.3/download")
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Stop took %v; it is waiting out net/http's grace for connections that never sent a request", elapsed)
	}
}

// TestStop_CancelsInFlightUpstreamFetch: a handler blocked on an upstream
// that never answers must not hold the daemon's shutdown open. The request
// context descends from the proxy's, so Stop cancels the fetch and returns
// within its grace window rather than waiting out the 120s client timeout.
func TestStop_CancelsInFlightUpstreamFetch(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{})

	var once sync.Once
	stuck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(blocked) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Order matters: the release must fire BEFORE Close, because
	// httptest.Server.Close waits for outstanding handlers.
	defer stuck.Close()
	defer close(release)

	dir := t.TempDir()
	p := New(Config{
		CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
		IndexUpstream: stuck.URL, RustupUpstream: stuck.URL,
		ListenAddr: "127.0.0.1:0", ContainerOS: "linux",
		HealthInterval: -1, Log: discardLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base := "http://" + p.Addr()

	go func() {
		resp, err := http.Get(base + "/index/se/rd/serde")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	<-blocked

	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// shutdownGrace plus slack. Without context propagation this would sit
	// until the upstream client timeout (defaultTimeout, 120s).
	if elapsed := time.Since(start); elapsed > shutdownGrace+3*time.Second {
		t.Errorf("Stop took %v with a request stuck on a dead upstream; the fetch context is not being cancelled", elapsed)
	}
}

// --- rustup ----------------------------------------------------------------

func TestRustup_DatedArtifactCachedForever(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/dist/2026-08-01/rust-std-nightly.tar.xz", "TOOLCHAIN", `"e1"`)
	_, base := newTestProxy(t, up, func(c *Config) { c.IndexTTL = alwaysRevalidate })

	for range 3 {
		resp, body := get(t, base+"/rustup/dist/2026-08-01/rust-std-nightly.tar.xz", nil)
		if resp.StatusCode != http.StatusOK || body != "TOOLCHAIN" {
			t.Fatalf("status = %d body = %q", resp.StatusCode, body)
		}
	}
	if got := up.hitCount("/dist/2026-08-01/rust-std-nightly.tar.xz"); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (dated toolchain artifacts are immutable)", got)
	}
}

func TestRustup_ChannelManifestRevalidates(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/dist/channel-rust-nightly.toml", "manifest-v1", `"m1"`)
	_, base := newTestProxy(t, up, func(c *Config) { c.IndexTTL = alwaysRevalidate })

	if _, body := get(t, base+"/rustup/dist/channel-rust-nightly.toml", nil); body != "manifest-v1" {
		t.Fatalf("first fetch = %q", body)
	}
	// The channel manifest is rewritten daily in place; the proxy must not
	// pin jobs to yesterday's nightly.
	up.set("/dist/channel-rust-nightly.toml", "manifest-v2", `"m2"`)
	if _, body := get(t, base+"/rustup/dist/channel-rust-nightly.toml", nil); body != "manifest-v2" {
		t.Errorf("second fetch = %q, want manifest-v2 (stale channel manifest served)", body)
	}
}

func TestRustup_FailsOpenWithRedirect(t *testing.T) {
	up := newFakeUpstream(t)
	up.setStatus("/dist/2026-08-01/x.tar.xz", http.StatusInternalServerError)
	_, base := newTestProxy(t, up, nil)

	resp, _ := get(t, base+"/rustup/dist/2026-08-01/x.tar.xz", nil)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (fail open)", resp.StatusCode)
	}
	if want := up.URL + "/dist/2026-08-01/x.tar.xz"; resp.Header.Get("Location") != want {
		t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), want)
	}
}

// --- lifecycle / wiring ----------------------------------------------------

// TestStart_WritesContainerConfigAndDeclaresMount is what makes the proxy
// actually get used: without the generated file (and the mount that carries
// it) Cargo silently keeps talking to crates.io.
func TestStart_WritesContainerConfigAndDeclaresMount(t *testing.T) {
	up := newFakeUpstream(t)
	p, _ := newTestProxy(t, up, nil)

	mounts := p.Mounts()
	if len(mounts) != 1 {
		t.Fatalf("Mounts() = %d entries, want 1", len(mounts))
	}
	m := mounts[0]
	if m.Destination != "/.cargo" {
		t.Errorf("mount destination = %q, want /.cargo", m.Destination)
	}
	if !m.ReadOnly {
		t.Error("the cargo config mount must be read-only — a job must not rewrite the next job's config")
	}

	raw, err := os.ReadFile(filepath.Join(m.Source, "config.toml"))
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	if !strings.Contains(string(raw), `replace-with = "ephemerd"`) {
		t.Errorf("generated config is not a source replacement:\n%s", raw)
	}
	if !strings.Contains(string(raw), "sparse+"+p.advertiseBase()+"/index/") {
		t.Errorf("generated config does not point at this proxy:\n%s", raw)
	}
}

// TestConfigDirIsOutsideCacheDir: `ephemerd cache clear cargo` wipes the
// cache root. If the mounted config lived there it would vanish from under a
// running job.
func TestConfigDirIsOutsideCacheDir(t *testing.T) {
	up := newFakeUpstream(t)
	p, _ := newTestProxy(t, up, nil)

	confAbs, err := filepath.Abs(p.Mounts()[0].Source)
	if err != nil {
		t.Fatalf("abs conf dir: %v", err)
	}
	cacheAbs, err := filepath.Abs(p.cfg.CacheDir)
	if err != nil {
		t.Fatalf("abs cache dir: %v", err)
	}
	rel, err := filepath.Rel(cacheAbs, confAbs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("container config dir %q is inside the clearable cache dir %q", confAbs, cacheAbs)
	}
}

func TestEnvVars_PointRustupAtTheProxy(t *testing.T) {
	up := newFakeUpstream(t)
	p, _ := newTestProxy(t, up, nil)

	env := p.EnvVars()
	if len(env) != 1 {
		t.Fatalf("EnvVars() = %v, want exactly the rustup dist server", env)
	}
	want := "RUSTUP_DIST_SERVER=" + p.advertiseBase() + "/rustup"
	if env[0] != want {
		t.Errorf("EnvVars()[0] = %q, want %q", env[0], want)
	}
}

// TestAdvertiseBase_UsesConfiguredAddress: after a wildcard fallback the
// bound address is "[::]:port", which is useless inside a container. The
// advertised address must stay the configured gateway address.
func TestAdvertiseBase_UsesConfiguredAddress(t *testing.T) {
	p := New(Config{ListenAddr: "10.88.0.1:8083", Log: discardLogger()})
	if got := p.advertiseBase(); got != "http://10.88.0.1:8083" {
		t.Errorf("advertiseBase() = %q, want http://10.88.0.1:8083", got)
	}
}

func TestStop_CleanupHonoursConfig(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/se/rd/serde", "body", "")

	t.Run("cleanup disabled keeps the cache", func(t *testing.T) {
		dir := t.TempDir()
		p := New(Config{
			CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
			IndexUpstream: up.URL, RustupUpstream: up.URL,
			ListenAddr: "127.0.0.1:0", ContainerOS: "linux", Cleanup: false, Log: discardLogger(),
		})
		if err := p.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		get(t, "http://"+p.Addr()+"/index/se/rd/serde", nil)
		if err := p.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "cache", "index", "se", "rd", "serde")); err != nil {
			t.Errorf("cache was wiped despite cleanup=false: %v", err)
		}
	})

	t.Run("cleanup enabled wipes the cache", func(t *testing.T) {
		dir := t.TempDir()
		p := New(Config{
			CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
			IndexUpstream: up.URL, RustupUpstream: up.URL,
			ListenAddr: "127.0.0.1:0", ContainerOS: "linux", Cleanup: true, Log: discardLogger(),
		})
		if err := p.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		get(t, "http://"+p.Addr()+"/index/se/rd/serde", nil)
		if err := p.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "cache")); !os.IsNotExist(err) {
			t.Errorf("cache survived cleanup=true (err=%v)", err)
		}
	})
}

// TestDLTemplateSurvivesRestart: Cargo caches config.json in CARGO_HOME, so
// after a daemon restart it may go straight to a crate download. The proxy
// must recover the upstream dl template from its own cache.
func TestDLTemplateSurvivesRestart(t *testing.T) {
	up := newFakeUpstream(t)
	up.set("/config.json", `{"dl":"`+up.URL+`/mirror/{crate}/{version}.crate"}`, "")
	up.set("/mirror/serde/1.0.203.crate", "TARBALL", "")

	dir := t.TempDir()
	mk := func() *Proxy {
		return New(Config{
			CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
			IndexUpstream: up.URL, RustupUpstream: up.URL,
			ListenAddr: "127.0.0.1:0", ContainerOS: "linux", Log: discardLogger(),
		})
	}

	p1 := mk()
	if err := p1.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	get(t, "http://"+p1.Addr()+"/index/config.json", nil)
	if err := p1.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	p2 := mk()
	if err := p2.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { _ = p2.Stop() }()

	if got := p2.dlTemplateOrDefault(); got != up.URL+"/mirror/{crate}/{version}.crate" {
		t.Fatalf("dl template after restart = %q, want the cached upstream template", got)
	}
	resp, body := get(t, "http://"+p2.Addr()+"/crates/serde/1.0.203/download", nil)
	if resp.StatusCode != http.StatusOK || body != "TARBALL" {
		t.Errorf("status = %d body = %q, want the tarball via the recovered template", resp.StatusCode, body)
	}
}

func TestHealthz(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)
	if resp, _ := get(t, base+healthRoute, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
}

// --- self-health withdrawal (fail-open layer 3) ----------------------------

// TestWatchdog_WithdrawsConfigWhenTheListenerStopsAnswering is the mitigation
// for the design blocker: a proxy that is up when the job starts but wedged
// when Cargo runs would fail the build, because a Cargo source replacement
// has no fallback. The proxy therefore withdraws its own config — the mount
// is a directory, so a container already running sees the swap and its next
// `cargo` invocation goes straight to crates.io.
func TestWatchdog_WithdrawsConfigWhenTheListenerStopsAnswering(t *testing.T) {
	up := newFakeUpstream(t)
	dir := t.TempDir()
	p := New(Config{
		CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
		IndexUpstream: up.URL, RustupUpstream: up.URL,
		ListenAddr: "127.0.0.1:0", ContainerOS: "linux",
		HealthInterval: 10 * time.Millisecond,
		Log:            discardLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	configPath := filepath.Join(p.Mounts()[0].Source, "config.toml")
	readConfig := func() string {
		t.Helper()
		raw, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("reading generated config: %v", err)
		}
		return string(raw)
	}

	// Healthy: the caching config stays installed even as the watchdog ticks.
	waitFor(t, func() bool { return strings.Contains(readConfig(), `replace-with = "ephemerd"`) },
		"caching config to be installed while healthy")

	// Simulate the wedge by stopping the HTTP server underneath the proxy.
	// The daemon lives on and the watchdog keeps running; only the socket
	// stops answering — the shape of the self-upgrade listener wedge seen on
	// Windows nodes.
	if err := p.srv.Shutdown(0); err != nil {
		t.Fatalf("stopping the inner server: %v", err)
	}

	waitFor(t, func() bool { return !strings.Contains(readConfig(), "replace-with") },
		"the proxy to withdraw its config after the listener stopped answering")

	if body := readConfig(); !strings.Contains(body, "[net]") {
		t.Errorf("withdrawn config is not a usable cargo config:\n%s", body)
	}
}

// TestWatchdog_RestoresConfigOnRecovery: withdrawal must not be permanent, or
// a single blip would cost the whole fleet its cache until the next restart.
func TestWatchdog_RestoresConfigOnRecovery(t *testing.T) {
	up := newFakeUpstream(t)
	dir := t.TempDir()
	p := New(Config{
		CacheDir: filepath.Join(dir, "cache"), ConfDir: filepath.Join(dir, "conf"),
		IndexUpstream: up.URL, RustupUpstream: up.URL,
		ListenAddr: "127.0.0.1:0", ContainerOS: "linux",
		HealthInterval: 10 * time.Millisecond,
		Log:            discardLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	configPath := filepath.Join(p.Mounts()[0].Source, "config.toml")
	has := func(s string) bool {
		raw, err := os.ReadFile(configPath)
		return err == nil && strings.Contains(string(raw), s)
	}

	// Force the withdrawal directly rather than racing a real outage.
	p.setConfigActive(false, "test")
	waitFor(t, func() bool { return !has("replace-with") }, "config to be withdrawn")

	// The listener is still fine, so the next probe restores it.
	waitFor(t, func() bool { return has(`replace-with = "ephemerd"`) },
		"the watchdog to restore the caching config once probes succeed again")
}

// waitFor polls cond until it holds, failing the test on timeout. Polling,
// not sleeping: the watchdog interval is short and the assertion is about
// eventual state, not timing.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestUnknownRouteIs404(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)
	if resp, _ := get(t, base+"/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNonGetMethodsRejected(t *testing.T) {
	up := newFakeUpstream(t)
	_, base := newTestProxy(t, up, nil)

	req, err := http.NewRequest(http.MethodPost, base+"/index/se/rd/serde", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
