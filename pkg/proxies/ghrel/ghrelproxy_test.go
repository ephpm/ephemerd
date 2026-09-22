package ghrelproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- harness ---------------------------------------------------------------

// fakeGitHub stands in for both halves of GitHub: the REST API that serves
// release metadata, and the host that serves the asset bytes. They are
// separate servers because the proxy points at them with separate config
// (APIUpstream / DownloadUpstream) and conflating them would hide a wiring
// mistake between the two.
type fakeGitHub struct {
	api      *httptest.Server
	dl       *httptest.Server
	metaHits atomic.Int64
	dlHits   atomic.Int64
	asset    []byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{asset: []byte(strings.Repeat("ASSETBYTES", 512))}

	dlMux := http.NewServeMux()
	dlMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gh.dlHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(gh.asset)
	})
	gh.dl = httptest.NewServer(dlMux)
	t.Cleanup(gh.dl.Close)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gh.metaHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"rel-v1"`)
		// Shaped like a real GitHub release: the asset carries several URL
		// fields and only browser_download_url is the byte source.
		_, _ = fmt.Fprintf(w, `{
		  "tag_name": "v1.2.3",
		  "html_url": "https://github.com/acme/tool/releases/tag/v1.2.3",
		  "body": "see %s/acme/tool/releases/download/v1.2.3/tool.tar.gz for the build",
		  "assets": [
		    {
		      "name": "tool.tar.gz",
		      "url": "%s/repos/acme/tool/releases/assets/42",
		      "browser_download_url": "%s/acme/tool/releases/download/v1.2.3/tool.tar.gz",
		      "size": 5120
		    }
		  ]
		}`, gh.dl.URL, gh.api.URL, gh.dl.URL)
	})
	gh.api = httptest.NewServer(apiMux)
	t.Cleanup(gh.api.Close)
	return gh
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

// result is everything these tests need from a response.
//
// get() reads and CLOSES the body before returning, so no *http.Response
// escapes this helper. That is not tidiness: bodyclose flags the CALL SITE of
// anything that returns a response it cannot see closed, so a helper handing
// one back can never satisfy the linter no matter how careful the caller is.
type result struct {
	code int
	data []byte
}

func get(t *testing.T, rawURL string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading GET %s: %v", rawURL, err)
	}
	return result{code: resp.StatusCode, data: b}
}

// --- the rewrite, which is the whole point ---------------------------------

// TestRewriteAssetURLsSingleRelease covers the shape GitHub returns from
// /releases/latest and /releases/tags/<tag>: one release object.
func TestRewriteAssetURLsSingleRelease(t *testing.T) {
	t.Parallel()
	const base = "http://gw:8087"
	in := []byte(`{
	  "tag_name": "v1",
	  "html_url": "https://github.com/acme/tool/releases/tag/v1",
	  "body": "grab https://github.com/acme/tool/releases/download/v1/tool.tgz",
	  "assets": [{
	    "name": "tool.tgz",
	    "url": "https://api.github.com/repos/acme/tool/releases/assets/7",
	    "browser_download_url": "https://github.com/acme/tool/releases/download/v1/tool.tgz"
	  }]
	}`)

	out, err := rewriteAssetURLs(in, base)
	if err != nil {
		t.Fatalf("rewriteAssetURLs: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	asset := doc["assets"].([]any)[0].(map[string]any)
	want := base + downloadPrefix + "acme/tool/releases/download/v1/tool.tgz"
	if got := asset["browser_download_url"]; got != want {
		t.Errorf("browser_download_url = %q, want %q", got, want)
	}

	// Everything else must survive byte-for-byte. A blind string replace
	// would have rewritten the body text and the API asset URL too, and the
	// API URL is what `gh` uses to fetch an asset from a private repo.
	if got := asset["url"]; got != "https://api.github.com/repos/acme/tool/releases/assets/7" {
		t.Errorf("asset.url was rewritten to %q", got)
	}
	if got := asset["name"]; got != "tool.tgz" {
		t.Errorf("asset.name = %q", got)
	}
	if got := doc["html_url"]; got != "https://github.com/acme/tool/releases/tag/v1" {
		t.Errorf("html_url was rewritten to %q", got)
	}
	if got := doc["body"].(string); !strings.Contains(got, "https://github.com/acme/tool/releases/download/v1/tool.tgz") {
		t.Errorf("release body was rewritten to %q", got)
	}
	if got := doc["tag_name"]; got != "v1" {
		t.Errorf("tag_name = %q", got)
	}
}

// TestRewriteAssetURLsReleaseArray covers the OTHER shape: /releases returns
// an array, and a rewriter that only understands an object silently sends
// every asset in a listing straight to the CDN.
func TestRewriteAssetURLsReleaseArray(t *testing.T) {
	t.Parallel()
	const base = "http://gw:8087"
	in := []byte(`[
	  {"tag_name":"v2","assets":[{"browser_download_url":"https://github.com/acme/tool/releases/download/v2/tool.tgz"}]},
	  {"tag_name":"v1","assets":[
	     {"browser_download_url":"https://github.com/acme/tool/releases/download/v1/tool.tgz"},
	     {"browser_download_url":"https://github.com/acme/tool/releases/download/v1/tool.sha256"}
	  ]}
	]`)

	out, err := rewriteAssetURLs(in, base)
	if err != nil {
		t.Fatalf("rewriteAssetURLs: %v", err)
	}
	var docs []map[string]any
	if err := json.Unmarshal(out, &docs); err != nil {
		t.Fatalf("an array of releases must stay an array: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d releases, want 2", len(docs))
	}

	var seen []string
	for _, rel := range docs {
		for _, a := range rel["assets"].([]any) {
			u := a.(map[string]any)["browser_download_url"].(string)
			if !strings.HasPrefix(u, base+downloadPrefix) {
				t.Errorf("browser_download_url = %q, want it pointed at the proxy", u)
			}
			seen = append(seen, u)
		}
	}
	if len(seen) != 3 {
		t.Errorf("rewrote %d asset URLs, want 3: %v", len(seen), seen)
	}
	if seen[0] != base+downloadPrefix+"acme/tool/releases/download/v2/tool.tgz" {
		t.Errorf("first rewritten URL = %q", seen[0])
	}
}

// TestRewriteAssetURLsLeavesOddShapesAlone: the API is not under our control,
// so a value that is not a usable URL must be passed through rather than
// guessed at, and nothing structurally odd may panic.
func TestRewriteAssetURLsLeavesOddShapesAlone(t *testing.T) {
	t.Parallel()
	const base = "http://gw:8087"

	out, err := rewriteAssetURLs([]byte(`{"assets":[
	  {"browser_download_url":"not-a-url"},
	  {"browser_download_url":""},
	  {"browser_download_url":42},
	  {"browser_download_url":"https://github.com/"}
	]}`), base)
	if err != nil {
		t.Fatalf("rewriteAssetURLs: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	assets := doc["assets"].([]any)
	if got := assets[0].(map[string]any)["browser_download_url"]; got != "not-a-url" {
		t.Errorf("a non-URL value was rewritten to %q", got)
	}
	if got := assets[1].(map[string]any)["browser_download_url"]; got != "" {
		t.Errorf("an empty value was rewritten to %q", got)
	}
	if got := assets[2].(map[string]any)["browser_download_url"]; got != float64(42) {
		t.Errorf("a non-string value was rewritten to %v", got)
	}
	if got := assets[3].(map[string]any)["browser_download_url"]; got != "https://github.com/" {
		t.Errorf("a pathless URL was rewritten to %q", got)
	}

	for _, in := range []string{`{"assets":"nope"}`, `{"assets":[5]}`, `[]`, `null`, `"scalar"`} {
		if _, err := rewriteAssetURLs([]byte(in), base); err != nil {
			t.Errorf("rewriteAssetURLs(%s): %v", in, err)
		}
	}
	if _, err := rewriteAssetURLs([]byte("not json"), base); err == nil {
		t.Error("unparseable metadata must be an error so the caller can fail loudly rather than serve unrewritten bytes")
	}
}

// --- end to end ------------------------------------------------------------

// TestMetadataIsCachedAndRewritten is the proxy doing its job: one upstream
// hit for repeated metadata reads, and the asset URLs in what it serves point
// back at itself.
func TestMetadataIsCachedAndRewritten(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL, MetadataTTL: time.Hour})
	base := "http://" + p.Addr()
	const path = "/repos/acme/tool/releases/latest"

	first := get(t, base+path).data
	second := get(t, base+path).data
	if gh.metaHits.Load() != 1 {
		t.Errorf("upstream metadata hits = %d, want 1", gh.metaHits.Load())
	}
	if string(first) != string(second) {
		t.Error("the cached metadata differs from the first response")
	}

	var doc map[string]any
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	asset := doc["assets"].([]any)[0].(map[string]any)
	want := base + downloadPrefix + "acme/tool/releases/download/v1.2.3/tool.tar.gz"
	if got := asset["browser_download_url"]; got != want {
		t.Fatalf("browser_download_url = %q, want %q", got, want)
	}
	if got := asset["size"]; got != float64(5120) {
		t.Errorf("asset.size = %v, want it untouched", got)
	}
}

// TestAssetIsFetchedThroughTheProxyAndCached follows the rewritten URL out of
// the metadata exactly as a build tool would.
func TestAssetIsFetchedThroughTheProxyAndCached(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL})
	base := "http://" + p.Addr()

	var doc map[string]any
	if err := json.Unmarshal(get(t, base+"/repos/acme/tool/releases/latest").data, &doc); err != nil {
		t.Fatal(err)
	}
	assetURL := doc["assets"].([]any)[0].(map[string]any)["browser_download_url"].(string)

	for i := range 3 {
		resp := get(t, assetURL)
		got := resp.data
		if resp.code != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, resp.code)
		}
		if string(got) != string(gh.asset) {
			t.Fatalf("attempt %d: asset bytes differ", i)
		}
	}
	if gh.dlHits.Load() != 1 {
		t.Errorf("upstream asset hits = %d, want 1: the bytes were not cached", gh.dlHits.Load())
	}
}

// TestImmutableAssetsStillServesTheRightBytes: the flag changes only how the
// entry is revalidated, never what a job receives.
func TestImmutableAssetsStillServesTheRightBytes(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{
		APIUpstream:      gh.api.URL,
		DownloadUpstream: gh.dl.URL,
		ImmutableAssets:  true,
	})
	base := "http://" + p.Addr()
	u := base + downloadPrefix + "acme/tool/releases/download/v1.2.3/tool.tar.gz"

	for range 2 {
		if got := get(t, u).data; string(got) != string(gh.asset) {
			t.Fatal("asset bytes differ")
		}
	}
	if gh.dlHits.Load() != 1 {
		t.Errorf("upstream asset hits = %d, want 1", gh.dlHits.Load())
	}
}

func TestUpstream404IsPassedThrough(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL})
	base := "http://" + p.Addr()

	for _, path := range []string{
		"/repos/acme/missing/releases/latest",
		downloadPrefix + "acme/missing/releases/download/v1/x.tgz",
	} {
		resp := get(t, base+path)
		if resp.code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: a nonexistent release is a real answer", path, resp.code)
		}
	}
}

func TestUnroutedPathsAre404(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL})
	base := "http://" + p.Addr()

	for _, path := range []string{"/", "/user", "/graphql", downloadPrefix} {
		resp := get(t, base+path)
		if resp.code == http.StatusOK {
			t.Errorf("GET %s returned 200; only /repos/ and %s are routed", path, downloadPrefix)
		}
	}
	if gh.metaHits.Load() != 0 {
		t.Error("an unrouted path reached the upstream API")
	}
}

// TestWritesAreRejected: this is a read-through cache, never a GitHub front
// end. Nothing that could mutate a release may be relayed.
func TestWritesAreRejected(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL})
	base := "http://" + p.Addr()

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequestWithContext(t.Context(), method, base+"/repos/acme/tool/releases", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, resp.StatusCode)
		}
	}
	if gh.metaHits.Load() != 0 {
		t.Error("a write method reached the upstream API")
	}
}

func TestHealthAndEnvVars(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL})

	if !p.Healthy() {
		t.Error("a running proxy reported unhealthy")
	}
	if p.Name() != "ghrel" {
		t.Errorf("Name = %q, want ghrel", p.Name())
	}

	env := p.EnvVars()
	if len(env) != 1 {
		t.Fatalf("EnvVars = %v, want exactly GHREL_PROXY", env)
	}
	k, v, _ := strings.Cut(env[0], "=")
	if k != "GHREL_PROXY" {
		t.Fatalf("EnvVars = %v, want GHREL_PROXY", env)
	}
	// It must be reachable: a base URL whose port is the one actually bound,
	// not the ":0" an operator (or a test) asked for.
	if v != "http://"+p.Addr() {
		t.Fatalf("GHREL_PROXY = %q, want the bound address http://%s", v, p.Addr())
	}
	resp := get(t, v+"/repos/acme/tool/releases/latest")
	if resp.code != http.StatusOK {
		t.Errorf("the advertised base URL answered %d", resp.code)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()
	p, err := New(Config{CacheDir: t.TempDir(), ListenAddr: "127.0.0.1:0", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.APIUpstream != DefaultAPIUpstream {
		t.Errorf("APIUpstream = %q, want %q", p.cfg.APIUpstream, DefaultAPIUpstream)
	}
	if p.cfg.DownloadUpstream != DefaultDownloadUpstream {
		t.Errorf("DownloadUpstream = %q, want %q", p.cfg.DownloadUpstream, DefaultDownloadUpstream)
	}
	if p.cfg.MetadataTTL != DefaultMetadataTTL {
		t.Errorf("MetadataTTL = %v, want %v", p.cfg.MetadataTTL, DefaultMetadataTTL)
	}
	if p.cfg.AssetTTL != DefaultAssetTTL {
		t.Errorf("AssetTTL = %v, want %v", p.cfg.AssetTTL, DefaultAssetTTL)
	}
	// A trailing slash on an override must not produce "//" in every
	// upstream URL built from it.
	p2, err := New(Config{CacheDir: t.TempDir(), ListenAddr: "127.0.0.1:0", APIUpstream: "https://ghe.test/api/v3/", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if p2.cfg.APIUpstream != "https://ghe.test/api/v3" {
		t.Errorf("APIUpstream = %q, want the trailing slash trimmed", p2.cfg.APIUpstream)
	}
	// A GitHub Enterprise upstream must be allowed to serve its own bytes
	// without the operator restating it in allowed_hosts.
	if !p2.allow.Allows("https://ghe.test/acme/tool/releases/download/v1/x.tgz") {
		t.Error("the configured upstream host is not on the allowlist")
	}
}

func TestQueryIsPartOfTheCacheKey(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL, MetadataTTL: time.Hour})
	base := "http://" + p.Addr()

	// Page 1 and page 2 of a release listing are different documents.
	_ = get(t, base+"/repos/acme/tool/releases?per_page=1&page=1").data
	_ = get(t, base+"/repos/acme/tool/releases?per_page=1&page=2").data
	if gh.metaHits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2: two queries shared a cache entry", gh.metaHits.Load())
	}
	_ = get(t, base+"/repos/acme/tool/releases?per_page=1&page=1").data
	if gh.metaHits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2: the first query was not cached", gh.metaHits.Load())
	}
}

// TestNestedMetadataPathsAreBothCached: the REST API nests documents —
// /repos/o/r/releases is one and /repos/o/r/releases/latest is another — and
// a path-shaped cache key would need "releases" to be a file and a directory
// at once. The loser is cached silently-not-at-all.
func TestNestedMetadataPathsAreBothCached(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	p := startProxy(t, Config{APIUpstream: gh.api.URL, DownloadUpstream: gh.dl.URL, MetadataTTL: time.Hour})
	base := "http://" + p.Addr()

	paths := []string{
		"/repos/acme/tool/releases",        // the parent, fetched first
		"/repos/acme/tool/releases/latest", // nested under it
	}
	for _, path := range paths {
		_ = get(t, base+path).data
	}
	if gh.metaHits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", gh.metaHits.Load())
	}
	for _, path := range paths {
		_ = get(t, base+path).data
	}
	if got := gh.metaHits.Load(); got != 2 {
		t.Errorf("upstream hits = %d after re-reading both documents, want 2: one of them is not being cached", got)
	}
}

// TestAPIAssetEndpointServesBytesNotJSON is the regression test for the bug
// that took out every php-sdk Linux build the day this proxy was first
// enabled.
//
// The asset-DOWNLOAD endpoint lives under /repos/ like the metadata endpoints,
// but returns BYTES. Routing it to serveMetadata runs a binary body through a
// JSON rewriter, which fails to parse and answers 502 -- and the client just
// sees a failed download, with nothing pointing at the proxy. spc fetches
// asset bytes from exactly this endpoint (it is how an asset is fetched from a
// private repo), so `type: ghrel` artifacts all went through it.
func TestAPIAssetEndpointServesBytesNotJSON(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Repeat("BINARY\x00\xff", 400))
	var hits atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer api.Close()

	p := startProxy(t, Config{APIUpstream: api.URL, DownloadUpstream: api.URL})
	base := "http://" + p.Addr()
	const path = "/repos/madler/zlib/releases/assets/12345"

	for i := range 2 {
		got := get(t, base+path)
		if got.code != http.StatusOK {
			t.Fatalf("attempt %d: status %d, want 200 (a 502 here is the proxy failing to parse bytes as JSON)", i, got.code)
		}
		if string(got.data) != string(payload) {
			t.Fatalf("attempt %d: body mismatch (%d bytes, want %d)", i, len(got.data), len(payload))
		}
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1: the asset was not cached", hits.Load())
	}
}

// The asset endpoint is CONTENT-NEGOTIATED: GitHub returns the asset's
// metadata as JSON unless the request asks for application/octet-stream. The
// upstream above ignores Accept and always returns bytes, which is why it kept
// passing while production served JSON — so this one behaves like GitHub.
//
// Regression for 2026-09-21: the proxy omitted Accept, cached 1.4 KB of
// metadata JSON under zlib-1.3.2.tar.gz's key, and served it to every build.
// It was spc's sha256 check, not anything here, that turned that into a
// visible failure rather than a corrupt PHP binary.
func TestAPIAssetSendsOctetStreamAccept(t *testing.T) {
	t.Parallel()
	payload := []byte(strings.Repeat("BINARY\x00\xff", 400))
	const metadataJSON = `{"url":"https://api.github.com/repos/madler/zlib/releases/assets/357391855","name":"zlib-1.3.2.tar.gz"}`

	var sawAccept atomic.Value
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAccept.Store(r.Header.Get("Accept"))
		if r.Header.Get("Accept") != "application/octet-stream" {
			// Exactly what GitHub does: a 200, with metadata, not the asset.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, metadataJSON)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer api.Close()

	p := startProxy(t, Config{APIUpstream: api.URL, DownloadUpstream: api.URL})
	got := get(t, "http://"+p.Addr()+"/repos/madler/zlib/releases/assets/357391855")

	if acc, _ := sawAccept.Load().(string); acc != "application/octet-stream" {
		t.Errorf("upstream saw Accept %q, want application/octet-stream", acc)
	}
	if got.code != http.StatusOK {
		t.Fatalf("status %d, want 200", got.code)
	}
	if string(got.data) != string(payload) {
		t.Fatalf("served %d bytes, want the %d-byte asset; body starts %.60q",
			len(got.data), len(payload), got.data)
	}
}

// Second line of defence, independent of Accept: if upstream answers an asset
// path with JSON anyway — a rate-limit body, an error document, an API change
// — that response must not be cached or served. A 200 carrying the wrong media
// type is the dangerous shape, because it looks like success everywhere except
// in the bytes.
func TestAPIAssetRefusesJSONResponse(t *testing.T) {
	t.Parallel()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer api.Close()

	p := startProxy(t, Config{APIUpstream: api.URL, DownloadUpstream: api.URL})
	got := get(t, "http://"+p.Addr()+"/repos/madler/zlib/releases/assets/357391855")

	if got.code == http.StatusOK {
		t.Fatalf("status 200 with a JSON body: the proxy served metadata as an artifact\n%.120q", got.data)
	}
	if strings.Contains(string(got.data), "rate limit") {
		t.Errorf("upstream JSON was passed through to the caller: %.120q", got.data)
	}
}

// TestIsAPIAssetPath pins the routing predicate directly: metadata paths must
// NOT be mistaken for assets, or every release lookup would be served as
// opaque bytes and never rewritten.
func TestIsAPIAssetPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path  string
		asset bool
	}{
		{"/repos/acme/tool/releases/assets/42", true},
		{"/repos/acme/tool/releases/assets/42/", true},
		{"/repos/acme/tool/releases/latest", false},
		{"/repos/acme/tool/releases", false},
		{"/repos/acme/tool/releases/tags/v1.0.0", false},
		{"/repos/acme/tool/releases/assets", false},
		{"/download/acme/tool/releases/download/v1/x.tgz", false},
	} {
		if got := isAPIAssetPath(tc.path); got != tc.asset {
			t.Errorf("isAPIAssetPath(%q) = %v, want %v", tc.path, got, tc.asset)
		}
	}
}
