package pipproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/proxies/pkgcache"
)

// --- harness ---------------------------------------------------------------

// fakeIndex is a PEP 503/691 index that also serves the files it points at,
// so the whole rewrite → download → cache path can be exercised end to end.
type fakeIndex struct {
	*httptest.Server
	indexHits atomic.Int64
	fileHits  atomic.Int64
	metaHits  atomic.Int64
	fail      atomic.Bool
	wheel     []byte
	metadata  []byte
}

const wheelName = "requests-2.31.0-py3-none-any.whl"

func newFakeIndex(t *testing.T) *fakeIndex {
	t.Helper()
	idx := &fakeIndex{
		wheel:    []byte(strings.Repeat("WHEELBYTES", 1024)),
		metadata: []byte("Metadata-Version: 2.1\nName: requests\nVersion: 2.31.0\n"),
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/packages/", func(w http.ResponseWriter, r *http.Request) {
		if idx.fail.Load() {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".metadata") {
			idx.metaHits.Add(1)
			_, _ = w.Write(idx.metadata)
			return
		}
		idx.fileHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(idx.wheel)
	})

	mux.HandleFunc("/simple/", func(w http.ResponseWriter, r *http.Request) {
		if idx.fail.Load() {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/simple/"), "/")
		if rest == "nosuchproject" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		idx.indexHits.Add(1)

		if rest == "" {
			// The ROOT index: links to project pages, not to files.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><body>`+
				`<a href="/simple/requests/">requests</a>`+
				`<a href="flask/">flask</a>`+
				`</body></html>`)
			return
		}

		if strings.Contains(r.Header.Get("Accept"), jsonAccept) {
			w.Header().Set("Content-Type", jsonAccept)
			_, _ = fmt.Fprintf(w, `{
			  "meta": {"api-version": "1.1"},
			  "name": "requests",
			  "versions": ["2.31.0"],
			  "files": [
			    {
			      "filename": %q,
			      "url": "%s/packages/aa/bb/%s",
			      "hashes": {"sha256": "cafebabe"},
			      "core-metadata": {"sha256": "d00dfeed"},
			      "yanked": false
			    },
			    {
			      "filename": "requests-9.9.9.tar.gz",
			      "url": "https://third-party.invalid/requests-9.9.9.tar.gz",
			      "hashes": {}
			    }
			  ]
			}`, wheelName, idx.Server.URL, wheelName)
			return
		}

		w.Header().Set("ETag", `"idx-v1"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><body>`+
			`<a href="%s/packages/aa/bb/%s#sha256=cafebabe" data-core-metadata="sha256=d00dfeed">%s</a>`+
			`<a href="../../packages/cc/dd/requests-2.30.0.tar.gz#sha256=beefcafe">requests-2.30.0.tar.gz</a>`+
			`<a href="https://third-party.invalid/requests-9.9.9.tar.gz">requests-9.9.9.tar.gz</a>`+
			`</body></html>`, idx.Server.URL, wheelName, wheelName)
	})

	idx.Server = httptest.NewServer(mux)
	t.Cleanup(idx.Close)
	return idx
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

func get(t *testing.T, client *http.Client, rawURL string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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

// firstArtifactURL pulls the first rewritten download link out of a project
// page, which is what pip would follow.
func firstArtifactURL(t *testing.T, page, base string) string {
	t.Helper()
	marker := base + pkgcache.ArtifactRoute + "/"
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no rewritten artifact URL in page:\n%s", page)
	}
	rest := page[i:]
	end := strings.IndexAny(rest, `"' <`)
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// --- tests -----------------------------------------------------------------

func TestIndexCacheHitAndMiss(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL, IndexTTL: time.Hour})
	base := "http://" + p.Addr()

	first := body(t, get(t, noRedirect(), base+"/simple/requests/", nil))
	if idx.indexHits.Load() != 1 {
		t.Fatalf("upstream index hits = %d, want 1", idx.indexHits.Load())
	}
	second := body(t, get(t, noRedirect(), base+"/simple/requests/", nil))
	if idx.indexHits.Load() != 1 {
		t.Errorf("a second request went upstream: hits = %d", idx.indexHits.Load())
	}
	if string(first) != string(second) {
		t.Error("the cached index page differs from the first response")
	}
}

// TestHTMLProjectPageIsRewritten covers the PEP 503 rendering, including the
// relative-link and non-allowlisted-host cases.
func TestHTMLProjectPageIsRewritten(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	page := string(body(t, get(t, noRedirect(), base+"/simple/requests/", nil)))

	if !strings.Contains(page, base+pkgcache.ArtifactRoute+"/") {
		t.Fatalf("no link was rewritten:\n%s", page)
	}
	// pip parses the wheel filename out of the last path segment.
	if !strings.Contains(page, "/"+wheelName+"#sha256=cafebabe") {
		t.Errorf("the filename and hash fragment were not preserved:\n%s", page)
	}
	// A relative link must be resolved against the page and rewritten too.
	if !strings.Contains(page, "requests-2.30.0.tar.gz#sha256=beefcafe") {
		t.Errorf("the relative sdist link was lost:\n%s", page)
	}
	if strings.Count(page, pkgcache.ArtifactRoute) != 2 {
		t.Errorf("expected exactly two rewritten links:\n%s", page)
	}
	// A file on a host outside the allowlist is left alone.
	if !strings.Contains(page, "https://third-party.invalid/requests-9.9.9.tar.gz") {
		t.Errorf("a non-allowlisted link was rewritten:\n%s", page)
	}
	// PEP 658 metadata advertising survives the rewrite.
	if !strings.Contains(page, `data-core-metadata="sha256=d00dfeed"`) {
		t.Errorf("the PEP 658 attribute was dropped:\n%s", page)
	}
}

func TestJSONProjectPageIsRewritten(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	raw := body(t, get(t, noRedirect(), base+"/simple/requests/", http.Header{"Accept": {jsonAccept}}))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("PEP 691 response is not JSON: %v\n%s", err, raw)
	}
	files := doc["files"].([]any)
	first := files[0].(map[string]any)
	if !strings.HasPrefix(first["url"].(string), base+pkgcache.ArtifactRoute+"/") {
		t.Errorf("files[0].url = %q, want it rewritten", first["url"])
	}
	// Hashes and unknown fields survive: pip verifies the bytes we serve
	// against them, which is what makes the rewrite safe.
	if first["hashes"].(map[string]any)["sha256"] != "cafebabe" {
		t.Error("the file hash was altered")
	}
	if first["yanked"] != false {
		t.Error("an unknown field was dropped")
	}
	if doc["meta"].(map[string]any)["api-version"] != "1.1" {
		t.Error("the meta block was dropped")
	}
	second := files[1].(map[string]any)
	if second["url"] != "https://third-party.invalid/requests-9.9.9.tar.gz" {
		t.Errorf("a non-allowlisted URL was rewritten to %q", second["url"])
	}
}

// TestRootIndexIsNotRewritten: the root index lists PROJECTS. Rewriting its
// links as artifacts would send pip to download project pages as wheels.
func TestRootIndexIsNotRewritten(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	page := string(body(t, get(t, noRedirect(), base+"/simple/", nil)))
	if strings.Contains(page, pkgcache.ArtifactRoute) {
		t.Errorf("the root index was rewritten as artifacts:\n%s", page)
	}
	if !strings.Contains(page, `href="/simple/requests/"`) {
		t.Errorf("the root index links were altered:\n%s", page)
	}
}

func TestWheelIsCachedOnce(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	page := string(body(t, get(t, noRedirect(), base+"/simple/requests/", nil)))
	wheelURL := firstArtifactURL(t, page, base)

	for i := range 3 {
		resp := get(t, noRedirect(), wheelURL, nil)
		got := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, resp.StatusCode)
		}
		if string(got) != string(idx.wheel) {
			t.Fatalf("attempt %d: wheel bytes differ", i)
		}
	}
	if idx.fileHits.Load() != 1 {
		t.Errorf("upstream file hits = %d, want 1", idx.fileHits.Load())
	}
}

// TestPEP658MetadataFetch pins the subtle part of the artifact route: pip
// appends ".metadata" to the URL WE advertised, and that must resolve to the
// upstream .metadata file — not to the whole wheel.
func TestPEP658MetadataFetch(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	page := string(body(t, get(t, noRedirect(), base+"/simple/requests/", nil)))
	wheelURL := firstArtifactURL(t, page, base)
	// Strip the fragment exactly as pip does before appending.
	wheelURL, _, _ = strings.Cut(wheelURL, "#")

	resp := get(t, noRedirect(), wheelURL+".metadata", nil)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(got) != string(idx.metadata) {
		t.Fatalf("got %d bytes, want the METADATA file (%d bytes)", len(got), len(idx.metadata))
	}
	if idx.metaHits.Load() != 1 || idx.fileHits.Load() != 0 {
		t.Errorf("meta hits = %d, file hits = %d: the wheel was fetched instead of its metadata",
			idx.metaHits.Load(), idx.fileHits.Load())
	}
}

func TestFailOpenRedirectsWhenUpstreamIsDown(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()
	idx.Close() // upstream gone, cache cold

	resp := get(t, noRedirect(), base+"/simple/requests/", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 to the origin index", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, idx.URL) {
		t.Errorf("Location = %q, want the origin index", loc)
	}
}

func TestFailOpenRedirectsArtifactToOrigin(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	page := string(body(t, get(t, noRedirect(), base+"/simple/requests/", nil)))
	wheelURL := firstArtifactURL(t, page, base)
	origin := idx.URL + "/packages/aa/bb/" + wheelName
	idx.fail.Store(true)

	resp := get(t, noRedirect(), wheelURL, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, origin) {
		t.Errorf("Location = %q, want %q", loc, origin)
	}
}

func TestFailOpenServesStaleIndex(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL, IndexTTL: -time.Second})
	base := "http://" + p.Addr()

	warm := body(t, get(t, noRedirect(), base+"/simple/requests/", nil))
	idx.fail.Store(true)

	resp := get(t, noRedirect(), base+"/simple/requests/", nil)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the stale cache", resp.StatusCode)
	}
	if string(got) != string(warm) {
		t.Error("the stale index page differs from the warm one")
	}
}

func TestUpstream404IsPassedThrough(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	resp := get(t, noRedirect(), "http://"+p.Addr()+"/simple/nosuchproject/", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	p := startProxy(t, Config{Upstream: idx.URL, CacheDir: cacheDir})
	base := "http://" + p.Addr()

	hostile := []string{
		"/simple/../../../../etc/passwd",
		"/simple/..%2f..%2f..%2fetc%2fpasswd",
		"/simple/%2e%2e/%2e%2e/escaped/",
		"/simple/requests/../../escaped/",
		`/simple/..\..\escaped/`,
		"/simple/C:/Windows/System32/config/SAM",
		"/../escaped",
		"/simple/a%00b/",
		pkgcache.ArtifactRoute + "/..%2f..%2fescaped/x.whl",
	}
	for _, path := range hostile {
		resp := get(t, noRedirect(), base+path, nil)
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
	p := startProxy(t, Config{}) // production defaults: PyPI upstream
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
		resp := get(t, noRedirect(), pkgcache.ArtifactURL(base, target), nil)
		got := body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("artifact fetch of %q returned 200", target)
		}
		if strings.Contains(string(got), "cloud-metadata-credentials") {
			t.Fatalf("the proxy relayed a non-allowlisted host: %q", got)
		}
	}
}

func TestWritesAreRejected(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})
	base := "http://" + p.Addr()

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		req, err := http.NewRequestWithContext(t.Context(), method, base+"/simple/requests/", strings.NewReader("x"))
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
	if idx.indexHits.Load() != 0 {
		t.Error("a write method reached the upstream index")
	}
}

func TestSizeCapEvicts(t *testing.T) {
	t.Parallel()
	idx := newFakeIndex(t)
	budget := int64(2 * len(idx.wheel))
	p := startProxy(t, Config{Upstream: idx.URL, MaxBytes: budget})
	base := "http://" + p.Addr()

	for i := range 12 {
		u := pkgcache.ArtifactURL(base, fmt.Sprintf("%s/packages/aa/bb/requests-2.31.%d-py3-none-any.whl", idx.URL, i))
		resp := get(t, noRedirect(), u, nil)
		if got := body(t, resp); len(got) != len(idx.wheel) {
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
	idx := newFakeIndex(t)
	p := startProxy(t, Config{Upstream: idx.URL})

	if !p.Healthy() {
		t.Error("a running proxy reported unhealthy")
	}
	if p.Name() != "pip" {
		t.Errorf("Name = %q", p.Name())
	}

	env := map[string]string{}
	for _, e := range p.EnvVars() {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	indexURL, ok := env["PIP_INDEX_URL"]
	if !ok {
		t.Fatalf("EnvVars = %v, want PIP_INDEX_URL", p.EnvVars())
	}
	u, err := url.Parse(indexURL)
	if err != nil {
		t.Fatalf("PIP_INDEX_URL = %q: %v", indexURL, err)
	}
	if !strings.HasSuffix(u.Path, "/simple/") {
		t.Errorf("PIP_INDEX_URL = %q, want it to end in /simple/", indexURL)
	}
	trusted, ok := env["PIP_TRUSTED_HOST"]
	if !ok {
		t.Fatalf("EnvVars = %v, want PIP_TRUSTED_HOST (the proxy speaks plain HTTP)", p.EnvVars())
	}
	// pip splits append-style env values on whitespace, so both the bare
	// host and the host:port form travel in one variable.
	fields := strings.Fields(trusted)
	if len(fields) != 2 || !strings.Contains(fields[1], ":") {
		t.Errorf("PIP_TRUSTED_HOST = %q, want '<host> <host>:<port>'", trusted)
	}
	if u.Hostname() != fields[0] {
		t.Errorf("PIP_TRUSTED_HOST host %q does not match PIP_INDEX_URL host %q", fields[0], u.Hostname())
	}

	// The advertised index URL must actually serve the index.
	resp := get(t, noRedirect(), indexURL, nil)
	_ = body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET PIP_INDEX_URL = %d, want 200", resp.StatusCode)
	}
}

// --- unit tests on the pure helpers ---------------------------------------

func TestRewriteFileURL(t *testing.T) {
	t.Parallel()
	page, err := url.Parse("https://pypi.org/simple/requests/")
	if err != nil {
		t.Fatal(err)
	}
	allow := defaultAllowedHosts
	base := "http://gw:8085"

	tests := []struct {
		name    string
		in      string
		wantOK  bool
		wantSub string
	}{
		{name: "absolute allowlisted", in: "https://files.pythonhosted.org/packages/a/b/x.whl#sha256=ab", wantOK: true, wantSub: "#sha256=ab"},
		{name: "relative", in: "../../packages/a/b/x.tar.gz", wantOK: true, wantSub: "/x.tar.gz"},
		{name: "root relative", in: "/packages/a/b/x.whl", wantOK: true, wantSub: "/x.whl"},
		{name: "third party host", in: "https://third-party.invalid/x.whl"},
		{name: "metadata endpoint", in: "http://169.254.169.254/latest/meta-data/"},
		{name: "non http scheme", in: "ftp://files.pythonhosted.org/x.whl"},
		{name: "unparseable", in: "http://a b c/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := rewriteFileURL(tc.in, page, base, allow)
			if ok != tc.wantOK {
				t.Fatalf("rewriteFileURL(%q) ok = %v, want %v (got %q)", tc.in, ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if !strings.HasPrefix(got, base+pkgcache.ArtifactRoute+"/") {
				t.Errorf("rewriteFileURL(%q) = %q, want an artifact URL", tc.in, got)
			}
			if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
				t.Errorf("rewriteFileURL(%q) = %q, want it to contain %q", tc.in, got, tc.wantSub)
			}
		})
	}
}

func TestRewriteSimpleHTMLHandlesQuotingAndEscapes(t *testing.T) {
	t.Parallel()
	page, err := url.Parse("https://pypi.org/simple/requests/")
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(`<a HREF = 'https://files.pythonhosted.org/packages/a/b/x.whl'>x</a>` +
		`<a href="https://files.pythonhosted.org/packages/a/b/y.whl?a=1&amp;b=2">y</a>` +
		`<a name="not-an-href">z</a>`)
	out := string(rewriteSimpleHTML(in, page, "http://gw:8085", defaultAllowedHosts))

	if strings.Count(out, pkgcache.ArtifactRoute) != 2 {
		t.Errorf("expected two rewrites, got:\n%s", out)
	}
	if !strings.Contains(out, `name="not-an-href"`) {
		t.Errorf("a non-href attribute was mangled:\n%s", out)
	}
	// The single-quoted form must stay single-quoted, spacing and all.
	if !strings.Contains(out, "HREF = 'http://gw:8085"+pkgcache.ArtifactRoute) {
		t.Errorf("quoting was not preserved:\n%s", out)
	}
	// The "&amp;" in the source URL must have been decoded before encoding
	// and round-tripped, not doubled into "&amp;amp;".
	if strings.Contains(out, "&amp;amp;") {
		t.Errorf("HTML entities were double-escaped:\n%s", out)
	}
}

func TestProjectNameValidation(t *testing.T) {
	t.Parallel()
	valid := []string{"requests", "Django", "zope.interface", "a-b_c.d", "n2"}
	invalid := []string{"", "..", ".hidden", "-leading", "a/b", "a b", "a\x00b", strings.Repeat("a", 200)}
	for _, s := range valid {
		if !projectNameRe.MatchString(s) {
			t.Errorf("projectNameRe rejected valid name %q", s)
		}
	}
	for _, s := range invalid {
		if projectNameRe.MatchString(s) {
			t.Errorf("projectNameRe accepted invalid name %q", s)
		}
	}
}
