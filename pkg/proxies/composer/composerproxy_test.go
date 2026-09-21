package composerproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

// --- harness ---------------------------------------------------------------

// fakePackagist serves both halves of what Composer talks to: the metadata
// repository, and the host the dist URLs inside it point at. One server, so
// the dist host is the configured upstream's host and therefore allowlisted
// — which is the case the rewrite is supposed to fire on.
type fakePackagist struct {
	*httptest.Server
	metaHits atomic.Int64
	distHits atomic.Int64
	archive  []byte
}

func newFakePackagist(t *testing.T) *fakePackagist {
	t.Helper()
	pk := &fakePackagist{archive: []byte(strings.Repeat("ZIPBYTES", 512))}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		pk.distHits.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(pk.archive)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		pk.metaHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"meta-v1"`)
		// A realistic p2 document: the version carries a dist URL and
		// several other URLs that must NOT be touched.
		_, _ = fmt.Fprintf(w, `{
		  "minified": "composer/2.0",
		  "packages": {
		    "acme/lib": [
		      {
		        "name": "acme/lib",
		        "version": "1.0.0",
		        "homepage": "https://acme.test/lib",
		        "source": {"type":"git","url":"https://github.com/acme/lib.git","reference":"deadbeef"},
		        "support": {"issues":"https://github.com/acme/lib/issues","source":"https://github.com/acme/lib"},
		        "dist": {
		          "type": "zip",
		          "url": "%s/repos/acme/lib/zipball/deadbeef",
		          "reference": "deadbeef",
		          "shasum": ""
		        }
		      }
		    ]
		  }
		}`, pk.URL)
	})
	pk.Server = httptest.NewServer(mux)
	t.Cleanup(pk.Close)
	return pk
}

func startProxy(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	if cfg.CacheDir == "" {
		cfg.CacheDir = t.TempDir()
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return p
}

func client() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func get(t *testing.T, rawURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// distTarget decodes the upstream URL back out of a rewritten dist URL.
func distTarget(t *testing.T, rewritten string) string {
	t.Helper()
	u, err := url.Parse(rewritten)
	if err != nil {
		t.Fatalf("rewritten dist URL %q is unparseable: %v", rewritten, err)
	}
	return u.Query().Get("u")
}

// --- the rewrite, which is the whole point ---------------------------------

// TestRewriteDistURLs is the difference between a cache that saves a few
// kilobytes of JSON and one that saves the download: dist.url must come
// through the proxy, and nothing else in the document may move.
func TestRewriteDistURLs(t *testing.T) {
	t.Parallel()
	const base = "http://gw:8088"
	const distURL = "https://api.github.com/repos/acme/lib/zipball/deadbeef"
	in := []byte(`{
	  "packages": {
	    "acme/lib": [{
	      "name": "acme/lib",
	      "homepage": "https://acme.test/lib",
	      "source": {"type":"git","url":"https://github.com/acme/lib.git"},
	      "support": {"issues":"https://github.com/acme/lib/issues","source":"https://github.com/acme/lib"},
	      "dist": {"type":"zip","url":"` + distURL + `","reference":"deadbeef","shasum":"abc"}
	    }]
	  }
	}`)

	out, err := rewriteDistURLs(in, base, defaultAllowedHosts)
	if err != nil {
		t.Fatalf("rewriteDistURLs: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	ver := doc["packages"].(map[string]any)["acme/lib"].([]any)[0].(map[string]any)

	dist := ver["dist"].(map[string]any)
	got := dist["url"].(string)
	if !strings.HasPrefix(got, base+distPrefix) {
		t.Fatalf("dist.url = %q, want it pointed at %s%s", got, base, distPrefix)
	}
	if target := distTarget(t, got); target != distURL {
		t.Errorf("dist.url carries upstream %q, want %q", target, distURL)
	}
	// The rest of the dist object is how Composer verifies what it
	// installs; moving the URL must not move these.
	if dist["reference"] != "deadbeef" || dist["shasum"] != "abc" || dist["type"] != "zip" {
		t.Errorf("dist metadata was altered: %v", dist)
	}

	// The URLs that are NOT the byte source. github.com is ON the allowlist,
	// so a rewriter that keyed off the host rather than the field name would
	// happily corrupt all three of these.
	if ver["homepage"] != "https://acme.test/lib" {
		t.Errorf("homepage was rewritten to %q", ver["homepage"])
	}
	src := ver["source"].(map[string]any)
	if src["url"] != "https://github.com/acme/lib.git" {
		t.Errorf("source.url was rewritten to %q", src["url"])
	}
	sup := ver["support"].(map[string]any)
	if sup["issues"] != "https://github.com/acme/lib/issues" {
		t.Errorf("support.issues was rewritten to %q", sup["issues"])
	}
	if sup["source"] != "https://github.com/acme/lib" {
		t.Errorf("support.source was rewritten to %q", sup["source"])
	}
	if ver["name"] != "acme/lib" {
		t.Errorf("name = %q", ver["name"])
	}
}

// TestRewriteDistURLsLeavesUnfrontedHostsAlone: a package served from a host
// this proxy will not relay must still install, straight from upstream.
func TestRewriteDistURLsLeavesUnfrontedHostsAlone(t *testing.T) {
	t.Parallel()
	const base = "http://gw:8088"
	out, err := rewriteDistURLs([]byte(`{"packages":{"acme/lib":[
	  {"dist":{"url":"https://dist.acme-internal.test/lib-1.0.0.zip"}},
	  {"dist":{"url":"not-a-url"}},
	  {"dist":{"url":""}},
	  {"dist":{"url":7}},
	  {"dist":"nope"}
	]}}`), base, defaultAllowedHosts)
	if err != nil {
		t.Fatalf("rewriteDistURLs: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	vers := doc["packages"].(map[string]any)["acme/lib"].([]any)

	if got := vers[0].(map[string]any)["dist"].(map[string]any)["url"]; got != "https://dist.acme-internal.test/lib-1.0.0.zip" {
		t.Errorf("a non-allowlisted dist URL was rewritten to %q", got)
	}
	if got := vers[1].(map[string]any)["dist"].(map[string]any)["url"]; got != "not-a-url" {
		t.Errorf("a non-URL dist value was rewritten to %q", got)
	}
	if got := vers[2].(map[string]any)["dist"].(map[string]any)["url"]; got != "" {
		t.Errorf("an empty dist URL was rewritten to %q", got)
	}
	if got := vers[3].(map[string]any)["dist"].(map[string]any)["url"]; got != float64(7) {
		t.Errorf("a non-string dist URL was rewritten to %v", got)
	}
	if got := vers[4].(map[string]any)["dist"]; got != "nope" {
		t.Errorf(`a "dist" that is not an object was altered to %v`, got)
	}

	for _, in := range []string{`{"packages":"nope"}`, `[]`, `null`, `"scalar"`, `{"dist":null}`} {
		if _, err := rewriteDistURLs([]byte(in), base, defaultAllowedHosts); err != nil {
			t.Errorf("rewriteDistURLs(%s): %v", in, err)
		}
	}
	if _, err := rewriteDistURLs([]byte("not json"), base, defaultAllowedHosts); err == nil {
		t.Error("unparseable metadata must be an error so the caller can fail loudly rather than serve unrewritten bytes")
	}
}

// --- the open-proxy guard --------------------------------------------------

// TestServeDistRefusesNonAllowlistedHosts is the SSRF fence. The dist route
// takes its upstream from a client-supplied query parameter, so the
// allowlist has to be re-checked HERE and not only at rewrite time —
// otherwise any job can hand ephemerd a URL and have it fetched with the
// daemon's network identity.
//
// The proxy is configured with the real Packagist upstream (never contacted
// here), so anything on loopback is off the allowlist — which is exactly the
// case this fence exists for.
func TestServeDistRefusesNonAllowlistedHosts(t *testing.T) {
	t.Parallel()
	p := startProxy(t, Config{})
	base := "http://" + p.Addr()

	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "cloud-metadata-credentials")
	}))
	defer secret.Close()

	for _, target := range []string{
		secret.URL + "/latest/meta-data/iam/",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:10000/",
		"http://[::1]:10000/",
		"file:///etc/passwd",
		"gopher://evil.test/",
		"https://evil.test/payload.zip",
		// A lookalike that must not match "github.com" as a parent domain.
		"https://evilgithub.com/payload.zip",
	} {
		u := base + distPrefix + "archive?u=" + url.QueryEscape(target)
		resp := get(t, u)
		got := body(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("dist fetch of %q returned %d, want 403", target, resp.StatusCode)
		}
		if strings.Contains(string(got), "cloud-metadata-credentials") {
			t.Fatalf("the proxy relayed a non-allowlisted host: %q", got)
		}
	}
}

func TestServeDistRequiresAnUpstream(t *testing.T) {
	t.Parallel()
	p := startProxy(t, Config{})
	base := "http://" + p.Addr()

	resp := get(t, base+distPrefix+"archive")
	_ = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a dist request with no u= returned %d, want 400", resp.StatusCode)
	}
}

// --- end to end ------------------------------------------------------------

// TestMetadataIsCachedAndDistIsFetchedThroughTheProxy walks the whole path a
// `composer install` takes: read the metadata, follow the dist URL in it.
func TestMetadataIsCachedAndDistIsFetchedThroughTheProxy(t *testing.T) {
	t.Parallel()
	pk := newFakePackagist(t)
	p := startProxy(t, Config{Upstream: pk.URL, MetadataTTL: time.Hour})
	base := "http://" + p.Addr()
	const path = "/p2/acme/lib.json"

	first := body(t, get(t, base+path))
	second := body(t, get(t, base+path))
	if pk.metaHits.Load() != 1 {
		t.Errorf("upstream metadata hits = %d, want 1", pk.metaHits.Load())
	}
	if string(first) != string(second) {
		t.Error("the cached metadata differs from the first response")
	}

	var doc map[string]any
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	ver := doc["packages"].(map[string]any)["acme/lib"].([]any)[0].(map[string]any)
	distURL := ver["dist"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(distURL, base+distPrefix) {
		t.Fatalf("dist.url = %q, want it pointed at the proxy", distURL)
	}
	if got := ver["source"].(map[string]any)["url"]; got != "https://github.com/acme/lib.git" {
		t.Errorf("source.url was rewritten to %q", got)
	}

	// Follow it exactly as Composer would, repeatedly.
	for i := range 3 {
		resp := get(t, distURL)
		got := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status %d (%q)", i, resp.StatusCode, got)
		}
		if string(got) != string(pk.archive) {
			t.Fatalf("attempt %d: archive bytes differ", i)
		}
	}
	// The archives are the bytes that matter. Caching them is the point;
	// the key must survive Cache.KeyPath for that to happen, which a
	// host:port-derived key does not.
	if pk.distHits.Load() != 1 {
		t.Errorf("upstream archive hits = %d, want 1: the archive was not cached", pk.distHits.Load())
	}
}

func TestUpstream404IsPassedThrough(t *testing.T) {
	t.Parallel()
	pk := newFakePackagist(t)
	p := startProxy(t, Config{Upstream: pk.URL})
	base := "http://" + p.Addr()

	resp := get(t, base+"/p2/acme/missing.json")
	_ = body(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("metadata status = %d, want 404: a nonexistent package is a real answer", resp.StatusCode)
	}

	u := base + distPrefix + "archive?u=" + url.QueryEscape(pk.URL+"/repos/acme/missing/zipball/deadbeef")
	resp2 := get(t, u)
	_ = body(t, resp2)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("archive status = %d, want 404", resp2.StatusCode)
	}
}

// TestWritesAreRejected: read-through cache, never a Packagist front end.
func TestWritesAreRejected(t *testing.T) {
	t.Parallel()
	pk := newFakePackagist(t)
	p := startProxy(t, Config{Upstream: pk.URL})
	base := "http://" + p.Addr()

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequestWithContext(t.Context(), method, base+"/p2/acme/lib.json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, resp.StatusCode)
		}
	}
	if pk.metaHits.Load() != 0 {
		t.Error("a write method reached the upstream repository")
	}
}

func TestHealthAndEnvVars(t *testing.T) {
	t.Parallel()
	pk := newFakePackagist(t)
	p := startProxy(t, Config{Upstream: pk.URL})

	if !p.Healthy() {
		t.Error("a running proxy reported unhealthy")
	}
	if p.Name() != "composer" {
		t.Errorf("Name = %q, want composer", p.Name())
	}

	env := p.EnvVars()
	if len(env) != 1 {
		t.Fatalf("EnvVars = %v, want exactly COMPOSER_REPO_PACKAGIST", env)
	}
	k, v, _ := strings.Cut(env[0], "=")
	if k != "COMPOSER_REPO_PACKAGIST" {
		t.Fatalf("EnvVars = %v, want COMPOSER_REPO_PACKAGIST", env)
	}
	// It must be reachable: the port actually bound, not the ":0" a test
	// (or an operator) asked for.
	if v != "http://"+p.Addr() {
		t.Fatalf("COMPOSER_REPO_PACKAGIST = %q, want the bound address http://%s", v, p.Addr())
	}
	resp := get(t, v+"/p2/acme/lib.json")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the advertised base URL answered %d", resp.StatusCode)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(Config{CacheDir: t.TempDir(), ListenAddr: "127.0.0.1:0", Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Upstream != DefaultUpstream {
		t.Errorf("Upstream = %q, want %q", p.cfg.Upstream, DefaultUpstream)
	}
	if p.cfg.MetadataTTL != DefaultMetadataTTL {
		t.Errorf("MetadataTTL = %v, want %v", p.cfg.MetadataTTL, DefaultMetadataTTL)
	}

	// A self-hosted mirror must be allowed to serve its own archives without
	// the operator restating it in allowed_hosts, trailing slash and all.
	p2, err := New(Config{CacheDir: t.TempDir(), ListenAddr: "127.0.0.1:0", Upstream: "https://packagist.acme.test/", Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	if p2.cfg.Upstream != "https://packagist.acme.test" {
		t.Errorf("Upstream = %q, want the trailing slash trimmed", p2.cfg.Upstream)
	}
	if !p2.allow.Allows("https://packagist.acme.test/dist/acme/lib.zip") {
		t.Error("the configured upstream host is not on the allowlist")
	}

	// And allowed_hosts extends it.
	p3, err := New(Config{CacheDir: t.TempDir(), ListenAddr: "127.0.0.1:0", AllowedHosts: []string{"dist.acme.test"}, Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	if !p3.allow.Allows("https://dist.acme.test/lib.zip") {
		t.Error("allowed_hosts did not extend the allowlist")
	}
}

// TestNestedMetadataPathsAreBothCached: Packagist serves packages.json
// alongside p2/<vendor>/<name>.json, so a path-shaped cache key can need the
// same name to be a file and a directory at once — and the loser is cached
// silently-not-at-all. Query strings are the other half of the same problem:
// "?" is rejected outright by pkgcache.SafeSegments.
func TestNestedMetadataPathsAreBothCached(t *testing.T) {
	t.Parallel()
	pk := newFakePackagist(t)
	p := startProxy(t, Config{Upstream: pk.URL, MetadataTTL: time.Hour})
	base := "http://" + p.Addr()

	paths := []string{
		"/p2/acme",                   // the parent, fetched first
		"/p2/acme/lib.json",          // nested under it
		"/packages.json?v=2",         // a query-carrying document
		"/packages.json?v=2&full=1",  // a different one at the same path
		"/p2/acme/lib.json~dev.json", // a sibling
	}
	for _, path := range paths {
		_ = body(t, get(t, base+path))
	}
	want := int64(len(paths))
	if got := pk.metaHits.Load(); got != want {
		t.Fatalf("upstream hits = %d, want %d", got, want)
	}
	for _, path := range paths {
		_ = body(t, get(t, base+path))
	}
	if got := pk.metaHits.Load(); got != want {
		t.Errorf("upstream hits = %d after re-reading every document, want %d: something is not being cached", got, want)
	}
}

// TestDistKeyIsFilesystemSafe pins the reason the dist key is a URL hash: a
// key built from host+path breaks the moment an upstream carries a port,
// and Cache.Writer failing is a SILENT loss of caching rather than an error
// anyone sees.
func TestDistKeyIsFilesystemSafe(t *testing.T) {
	t.Parallel()
	c := startProxy(t, Config{}).srv.Cache()
	for _, raw := range []string{
		"https://api.github.com/repos/acme/lib/zipball/deadbeef",
		"https://packagist.acme.test:8443/dist/acme/lib-1.0.0.zip",
		"http://127.0.0.1:59123/repos/acme/lib/zipball/deadbeef",
		"https://gitlab.com/acme/lib/-/archive/v1.0.0/lib-v1.0.0.zip?ref=v1",
	} {
		if _, err := c.KeyPath(pkgcache.ArtifactKey(raw)); err != nil {
			t.Errorf("the dist cache key for %q is not usable on disk: %v", raw, err)
		}
	}
}
