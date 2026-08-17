package pubproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

// --- harness ---------------------------------------------------------------

type fakePub struct {
	*httptest.Server
	listingHits atomic.Int64
	archiveHits atomic.Int64
	publishHits atomic.Int64
	fail        atomic.Bool
	archive     []byte
}

func newFakePub(t *testing.T) *fakePub {
	t.Helper()
	pub := &fakePub{archive: []byte(strings.Repeat("ARCHIVE", 1024))}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/archives/", func(w http.ResponseWriter, _ *http.Request) {
		pub.archiveHits.Add(1)
		if pub.fail.Load() {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pub.archive)
	})
	mux.HandleFunc("/api/packages/versions/new", func(w http.ResponseWriter, _ *http.Request) {
		pub.publishHits.Add(1)
		_, _ = io.WriteString(w, `{"url":"https://pub.dev/upload","fields":{}}`)
	})
	mux.HandleFunc("/api/packages/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/packages/")
		if strings.HasSuffix(name, "/advisories") {
			_, _ = io.WriteString(w, `{"advisories":[]}`)
			return
		}
		if name == "nosuchpackage" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		pub.listingHits.Add(1)
		if pub.fail.Load() {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"listing-v1"`)
		w.Header().Set("Content-Type", "application/vnd.pub.v2+json")
		_, _ = fmt.Fprintf(w, `{
		  "name": "http",
		  "latest": {
		    "version": "1.2.0",
		    "archive_url": "%s/api/archives/http-1.2.0.tar.gz",
		    "archive_sha256": "cafebabe",
		    "pubspec": {"name": "http", "version": "1.2.0"}
		  },
		  "versions": [
		    {
		      "version": "1.2.0",
		      "archive_url": "%s/api/archives/http-1.2.0.tar.gz",
		      "archive_sha256": "cafebabe",
		      "pubspec": {"name": "http", "version": "1.2.0", "environment": {"sdk": "^3.0.0"}}
		    },
		    {
		      "version": "0.13.6",
		      "archive_url": "https://third-party.invalid/http-0.13.6.tar.gz",
		      "retracted": true
		    }
		  ]
		}`, pub.Server.URL, pub.Server.URL)
	})

	pub.Server = httptest.NewServer(mux)
	t.Cleanup(pub.Close)
	return pub
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

func noRedirect() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func get(t *testing.T, client *http.Client, rawURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
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

func archiveURLFrom(t *testing.T, raw []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("listing is not JSON: %v", err)
	}
	versions := doc["versions"].([]any)
	return versions[0].(map[string]any)["archive_url"].(string)
}

// --- tests -----------------------------------------------------------------

func TestListingCacheHitAndMiss(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL, ListingTTL: time.Hour})
	base := "http://" + p.Addr()

	first := body(t, get(t, noRedirect(), base+"/api/packages/http"))
	if pub.listingHits.Load() != 1 {
		t.Fatalf("upstream listing hits = %d, want 1", pub.listingHits.Load())
	}
	second := body(t, get(t, noRedirect(), base+"/api/packages/http"))
	if pub.listingHits.Load() != 1 {
		t.Errorf("a second request went upstream: hits = %d", pub.listingHits.Load())
	}
	if string(first) != string(second) {
		t.Error("the cached listing differs from the first response")
	}
}

func TestListingArchiveURLsAreRewritten(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	base := "http://" + p.Addr()

	var doc map[string]any
	if err := json.Unmarshal(body(t, get(t, noRedirect(), base+"/api/packages/http")), &doc); err != nil {
		t.Fatal(err)
	}

	latest := doc["latest"].(map[string]any)
	if !strings.HasPrefix(latest["archive_url"].(string), base+pkgcache.ArtifactRoute+"/") {
		t.Errorf("latest.archive_url = %q, want it rewritten", latest["archive_url"])
	}
	// The checksum pub verifies against must survive untouched.
	if latest["archive_sha256"] != "cafebabe" {
		t.Error("archive_sha256 was altered")
	}
	// The pubspec drives resolution: it must survive verbatim.
	if latest["pubspec"].(map[string]any)["version"] != "1.2.0" {
		t.Error("the pubspec was altered")
	}

	versions := doc["versions"].([]any)
	v0 := versions[0].(map[string]any)
	if !strings.HasPrefix(v0["archive_url"].(string), base+pkgcache.ArtifactRoute+"/") {
		t.Errorf("versions[0].archive_url = %q, want it rewritten", v0["archive_url"])
	}
	if v0["pubspec"].(map[string]any)["environment"].(map[string]any)["sdk"] != "^3.0.0" {
		t.Error("a nested pubspec field was dropped")
	}

	v1 := versions[1].(map[string]any)
	if v1["archive_url"] != "https://third-party.invalid/http-0.13.6.tar.gz" {
		t.Errorf("a non-allowlisted archive_url was rewritten to %q", v1["archive_url"])
	}
	if v1["retracted"] != true {
		t.Error("the retracted flag was dropped")
	}
}

func TestArchiveIsCachedOnce(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	base := "http://" + p.Addr()

	archiveURL := archiveURLFrom(t, body(t, get(t, noRedirect(), base+"/api/packages/http")))
	for i := range 3 {
		resp := get(t, noRedirect(), archiveURL)
		got := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, resp.StatusCode)
		}
		if string(got) != string(pub.archive) {
			t.Fatalf("attempt %d: archive bytes differ", i)
		}
	}
	if pub.archiveHits.Load() != 1 {
		t.Errorf("upstream archive hits = %d, want 1", pub.archiveHits.Load())
	}
}

// TestCanonicalArchivePathIsCached covers a pubspec.lock that already
// records the canonical /api/archives/ URL.
func TestCanonicalArchivePathIsCached(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	base := "http://" + p.Addr()

	for range 2 {
		resp := get(t, noRedirect(), base+"/api/archives/http-1.2.0.tar.gz")
		if got := body(t, resp); string(got) != string(pub.archive) {
			t.Fatalf("archive bytes differ (status %d)", resp.StatusCode)
		}
	}
	if pub.archiveHits.Load() != 1 {
		t.Errorf("upstream archive hits = %d, want 1", pub.archiveHits.Load())
	}
}

func TestFailOpenRedirectsWhenUpstreamIsDown(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	base := "http://" + p.Addr()
	pub.Close()

	resp := get(t, noRedirect(), base+"/api/packages/http")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("listing status = %d, want 307 to the origin", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, pub.URL) {
		t.Errorf("Location = %q, want the origin", loc)
	}

	resp2 := get(t, noRedirect(), base+"/api/archives/http-1.2.0.tar.gz")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("archive status = %d, want 307 to the origin", resp2.StatusCode)
	}
}

func TestFailOpenServesStaleListing(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL, ListingTTL: -time.Second})
	base := "http://" + p.Addr()

	warm := body(t, get(t, noRedirect(), base+"/api/packages/http"))
	pub.fail.Store(true)

	resp := get(t, noRedirect(), base+"/api/packages/http")
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the stale cache", resp.StatusCode)
	}
	if string(got) != string(warm) {
		t.Error("the stale listing differs from the warm one")
	}
}

func TestUpstream404IsPassedThrough(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	resp := get(t, noRedirect(), "http://"+p.Addr()+"/api/packages/nosuchpackage")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	p := startProxy(t, Config{Upstream: pub.URL, CacheDir: cacheDir})
	base := "http://" + p.Addr()

	hostile := []string{
		"/api/packages/../../../../etc/passwd",
		"/api/packages/..%2f..%2f..%2fetc%2fpasswd",
		"/api/archives/../../escaped.tar.gz",
		"/api/archives/..%2f..%2fescaped.tar.gz",
		`/api/archives/..\..\escaped.tar.gz`,
		"/api/packages/%2e%2e/escaped",
		"/api/archives/C:/Windows/System32/config/SAM",
		"/../escaped",
		pkgcache.ArtifactRoute + "/..%2f..%2fescaped/x.tar.gz",
	}
	for _, path := range hostile {
		resp := get(t, noRedirect(), base+path)
		_ = body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %q returned 200; traversal must never succeed", path)
		}
	}

	parent := filepath.Dir(cacheDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "cache" {
			t.Errorf("a request created %q outside the cache root", filepath.Join(parent, e.Name()))
		}
	}
}

func TestArtifactRouteRefusesNonAllowlistedHosts(t *testing.T) {
	t.Parallel()
	p := startProxy(t, Config{}) // production defaults: pub.dev upstream
	base := "http://" + p.Addr()

	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "cloud-metadata-credentials")
	}))
	defer secret.Close()

	for _, target := range []string{
		secret.URL + "/latest/meta-data/iam/",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
	} {
		resp := get(t, noRedirect(), pkgcache.ArtifactURL(base, target))
		got := body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("artifact fetch of %q returned 200", target)
		}
		if strings.Contains(string(got), "cloud-metadata-credentials") {
			t.Fatalf("the proxy relayed a non-allowlisted host: %q", got)
		}
	}
}

// TestPublishIsNotProxied: the upload itself is a POST (rejected by the
// method gate) and the session-creation GET must not be routed as a package
// listing either.
func TestPublishIsNotProxied(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	base := "http://" + p.Addr()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequestWithContext(t.Context(), method, base+"/api/packages/versions/new", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := noRedirect().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, resp.StatusCode)
		}
	}

	// The listing route must not swallow the publish path either.
	if _, _, ok := strings.Cut("versions/new", "/"); !ok {
		t.Fatal("test premise wrong")
	}
	if pubNameRe.MatchString("versions/new") {
		t.Error("the package-name pattern accepted a two-segment publish path")
	}
	if pub.publishHits.Load() != 0 {
		t.Error("a publish request reached the upstream registry via a write method")
	}
}

func TestOtherAPIRoutesArePassedThrough(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})
	resp := get(t, noRedirect(), "http://"+p.Addr()+"/api/packages/http/advisories")
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(got), "advisories") {
		t.Errorf("advisories passthrough: status %d body %q", resp.StatusCode, got)
	}
	if p.srv.Cache().Len() != 0 {
		t.Error("a passthrough response was cached")
	}
}

func TestSizeCapEvicts(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	budget := int64(2 * len(pub.archive))
	p := startProxy(t, Config{Upstream: pub.URL, MaxBytes: budget})
	base := "http://" + p.Addr()

	for i := range 12 {
		u := pkgcache.ArtifactURL(base, fmt.Sprintf("%s/api/archives/http-1.2.%d.tar.gz", pub.URL, i))
		resp := get(t, noRedirect(), u)
		if got := body(t, resp); len(got) != len(pub.archive) {
			t.Fatalf("iteration %d: status %d, %d bytes", i, resp.StatusCode, len(got))
		}
	}

	var total int64
	err := filepath.WalkDir(p.srv.Cache().Root(), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // best effort
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total > budget {
		t.Errorf("cache holds %d bytes, past its %d byte budget", total, budget)
	}
	if total == 0 {
		t.Error("eviction emptied the cache entirely")
	}
}

func TestHealthAndEnvVars(t *testing.T) {
	t.Parallel()
	pub := newFakePub(t)
	p := startProxy(t, Config{Upstream: pub.URL})

	if !p.Healthy() {
		t.Error("a running proxy reported unhealthy")
	}
	if p.Name() != "pub" {
		t.Errorf("Name = %q", p.Name())
	}

	env := p.EnvVars()
	if len(env) != 1 || !strings.HasPrefix(env[0], "PUB_HOSTED_URL=") {
		t.Fatalf("EnvVars = %v, want a single PUB_HOSTED_URL", env)
	}
	value := strings.TrimPrefix(env[0], "PUB_HOSTED_URL=")
	if strings.HasSuffix(value, "/") {
		t.Errorf("PUB_HOSTED_URL = %q, want no trailing slash: pub concatenates paths onto it", value)
	}
	// The advertised base must actually serve the API.
	resp := get(t, noRedirect(), value+"/api/packages/http")
	_ = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET PUB_HOSTED_URL/api/packages/http = %d, want 200", resp.StatusCode)
	}
}

// --- unit tests on the pure helpers ---------------------------------------

func TestRewriteListingTolerates(t *testing.T) {
	t.Parallel()
	if _, err := rewriteListing([]byte("not json"), "http://gw", defaultAllowedHosts); err == nil {
		t.Error("expected an error for unparseable metadata so the caller can serve it through unchanged")
	}
	for _, in := range []string{
		`{}`,
		`{"latest":5}`,
		`{"versions":"nope"}`,
		`{"versions":[5,null]}`,
		`{"versions":[{"archive_url":7}]}`,
		`{"latest":{"archive_url":""}}`,
	} {
		if _, err := rewriteListing([]byte(in), "http://gw", defaultAllowedHosts); err != nil {
			t.Errorf("rewriteListing(%s): %v", in, err)
		}
	}
}

func TestNameAndArchivePatterns(t *testing.T) {
	t.Parallel()
	validNames := []string{"http", "flutter_bloc", "a", "x1_y2"}
	invalidNames := []string{"", "Http", "1http", "a-b", "a.b", "a/b", "..", strings.Repeat("a", 100)}
	for _, s := range validNames {
		if !pubNameRe.MatchString(s) {
			t.Errorf("pubNameRe rejected valid name %q", s)
		}
	}
	for _, s := range invalidNames {
		if pubNameRe.MatchString(s) {
			t.Errorf("pubNameRe accepted invalid name %q", s)
		}
	}

	validFiles := []string{"http-1.2.0.tar.gz", "flutter_bloc-8.1.3+1.tar.gz"}
	invalidFiles := []string{"", "..", "../x.tar.gz", "x.tar.gz.exe", "x.zip", `..\x.tar.gz`, "a/b.tar.gz"}
	for _, s := range validFiles {
		if !archiveFileRe.MatchString(s) {
			t.Errorf("archiveFileRe rejected valid file %q", s)
		}
	}
	for _, s := range invalidFiles {
		if archiveFileRe.MatchString(s) {
			t.Errorf("archiveFileRe accepted invalid file %q", s)
		}
	}
}
