package npmproxy

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

type fakeRegistry struct {
	*httptest.Server
	packumentHits atomic.Int64
	tarballHits   atomic.Int64
	fail          atomic.Bool
	tarball       []byte
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	reg := &fakeRegistry{tarball: []byte(strings.Repeat("TARBALL", 1024))}
	mux := http.NewServeMux()
	mux.HandleFunc("/express/-/", func(w http.ResponseWriter, _ *http.Request) {
		reg.tarballHits.Add(1)
		if reg.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(reg.tarball)
	})
	mux.HandleFunc("/-/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"objects":[]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		reg.packumentHits.Add(1)
		if reg.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"pack-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
		  "name": "express",
		  "dist-tags": {"latest": "4.18.2"},
		  "versions": {
		    "4.18.2": {
		      "name": "express", "version": "4.18.2",
		      "dist": {
		        "tarball": "%s/express/-/express-4.18.2.tgz",
		        "integrity": "sha512-deadbeef",
		        "shasum": "abc123"
		      }
		    },
		    "4.18.1": {
		      "name": "express", "version": "4.18.1",
		      "dist": {"tarball": "https://third-party.invalid/express-4.18.1.tgz"}
		    }
		  }
		}`, reg.Server.URL)
	})
	reg.Server = httptest.NewServer(mux)
	t.Cleanup(reg.Close)
	return reg
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

// noRedirect is used wherever a test needs to SEE the fail-open redirect
// rather than transparently follow it.
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

// --- tests -----------------------------------------------------------------

func TestPackumentCacheHitAndMiss(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL, PackumentTTL: time.Hour})
	base := "http://" + p.Addr()

	first := body(t, get(t, noRedirect(), base+"/express", nil))
	if reg.packumentHits.Load() != 1 {
		t.Fatalf("upstream packument hits = %d, want 1", reg.packumentHits.Load())
	}
	second := body(t, get(t, noRedirect(), base+"/express", nil))
	if reg.packumentHits.Load() != 1 {
		t.Errorf("a second request went upstream: hits = %d", reg.packumentHits.Load())
	}
	if string(first) != string(second) {
		t.Error("the cached packument differs from the first response")
	}
}

// TestPackumentVariantsAreCachedSeparately: the abbreviated and full
// packuments are different documents, and serving one for the other breaks
// `npm view`.
func TestPackumentVariantsAreCachedSeparately(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL, PackumentTTL: time.Hour})
	base := "http://" + p.Addr()

	_ = body(t, get(t, noRedirect(), base+"/express", http.Header{"Accept": {abbreviatedAccept}}))
	_ = body(t, get(t, noRedirect(), base+"/express", http.Header{"Accept": {"application/json"}}))
	if reg.packumentHits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2: the two Accept variants share a cache entry", reg.packumentHits.Load())
	}
	// And each variant is then cached in its own right.
	_ = body(t, get(t, noRedirect(), base+"/express", http.Header{"Accept": {abbreviatedAccept}}))
	if reg.packumentHits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", reg.packumentHits.Load())
	}
}

// TestPackumentTarballsAreRewritten is the whole point of the npm cache: the
// bytes must come through us, and only for hosts we are allowed to relay.
func TestPackumentTarballsAreRewritten(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	base := "http://" + p.Addr()

	var doc map[string]any
	if err := json.Unmarshal(body(t, get(t, noRedirect(), base+"/express", nil)), &doc); err != nil {
		t.Fatal(err)
	}
	versions := doc["versions"].(map[string]any)

	got := versions["4.18.2"].(map[string]any)["dist"].(map[string]any)
	tarball := got["tarball"].(string)
	if !strings.HasPrefix(tarball, base+pkgcache.ArtifactRoute+"/") {
		t.Errorf("dist.tarball = %q, want it rewritten to this proxy", tarball)
	}
	// Integrity metadata must survive untouched — npm verifies the bytes we
	// serve against it, which is what makes the rewrite safe.
	if got["integrity"] != "sha512-deadbeef" || got["shasum"] != "abc123" {
		t.Errorf("integrity metadata was altered: %v", got)
	}

	// A tarball on a host outside the allowlist is deliberately left alone,
	// so the client fetches it directly rather than through a relay we do
	// not trust.
	other := versions["4.18.1"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)
	if other != "https://third-party.invalid/express-4.18.1.tgz" {
		t.Errorf("a non-allowlisted tarball URL was rewritten to %q", other)
	}
}

func TestTarballIsCachedOnce(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	base := "http://" + p.Addr()

	// Follow the rewritten URL out of the packument, exactly as npm would.
	var doc map[string]any
	if err := json.Unmarshal(body(t, get(t, noRedirect(), base+"/express", nil)), &doc); err != nil {
		t.Fatal(err)
	}
	tarballURL := doc["versions"].(map[string]any)["4.18.2"].(map[string]any)["dist"].(map[string]any)["tarball"].(string)

	for i := range 3 {
		resp := get(t, noRedirect(), tarballURL, nil)
		got := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status %d", i, resp.StatusCode)
		}
		if string(got) != string(reg.tarball) {
			t.Fatalf("attempt %d: tarball bytes differ", i)
		}
	}
	if reg.tarballHits.Load() != 1 {
		t.Errorf("upstream tarball hits = %d, want 1", reg.tarballHits.Load())
	}
}

// TestCanonicalTarballPathIsCached covers the other way a tarball can be
// requested: a lockfile that already records the conventional registry path.
func TestCanonicalTarballPathIsCached(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	base := "http://" + p.Addr()

	for range 2 {
		resp := get(t, noRedirect(), base+"/express/-/express-4.18.2.tgz", nil)
		if got := body(t, resp); string(got) != string(reg.tarball) {
			t.Fatalf("tarball bytes differ (status %d)", resp.StatusCode)
		}
	}
	if reg.tarballHits.Load() != 1 {
		t.Errorf("upstream tarball hits = %d, want 1", reg.tarballHits.Load())
	}
}

// TestFailOpenRedirectsWhenUpstreamIsDown is the hard requirement: a dead
// cache path must degrade to "normal speed", never "build fails".
func TestFailOpenRedirectsWhenUpstreamIsDown(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	base := "http://" + p.Addr()
	reg.Close() // upstream gone, cache cold

	resp := get(t, noRedirect(), base+"/express", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("packument status = %d, want 307 to the origin", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, reg.URL) {
		t.Errorf("Location = %q, want the origin registry", loc)
	}

	resp2 := get(t, noRedirect(), base+"/express/-/express-4.18.2.tgz", nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("tarball status = %d, want 307 to the origin", resp2.StatusCode)
	}
}

func TestFailOpenServesStalePackument(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	// A negative TTL forces revalidation on every request, so the stale
	// path is exercised deterministically.
	p := startProxy(t, Config{Upstream: reg.URL, PackumentTTL: -time.Second})
	base := "http://" + p.Addr()

	warm := body(t, get(t, noRedirect(), base+"/express", nil))
	reg.fail.Store(true)

	resp := get(t, noRedirect(), base+"/express", nil)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the stale cache", resp.StatusCode)
	}
	if string(got) != string(warm) {
		t.Error("the stale packument differs from the warm one")
	}
}

func TestUpstream404IsPassedThrough(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	resp := get(t, noRedirect(), "http://"+p.Addr()+"/missing-package", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: a nonexistent package is a real answer", resp.StatusCode)
	}
}

// TestPathTraversalIsRejected: nothing a client can put in a URL may create
// or read a file outside the cache root.
func TestPathTraversalIsRejected(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	p := startProxy(t, Config{Upstream: reg.URL, CacheDir: cacheDir})
	base := "http://" + p.Addr()

	hostile := []string{
		"/../../../../etc/passwd",
		"/..%2f..%2f..%2fetc%2fpasswd",
		"/%2e%2e/%2e%2e/escaped",
		"/express/-/../../../escaped.tgz",
		"/express/-/..%2f..%2fescaped.tgz",
		`/express/-/..\..\escaped.tgz`,
		"/./../escaped",
		"/@scope/../../escaped",
		"/@scope%2f..%2f..%2fescaped",
		"/C:/Windows/System32/config/SAM",
		pkgcache.ArtifactRoute + "/..%2f..%2fescaped/x.tgz",
	}
	for _, path := range hostile {
		u := base + path
		resp := get(t, noRedirect(), u, nil)
		_ = body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %q returned 200; traversal must never succeed", path)
		}
	}

	// And nothing landed outside the cache root.
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
	err = filepath.WalkDir(cacheDir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(cacheDir, path)
		if rerr != nil {
			return rerr
		}
		if strings.HasPrefix(rel, "..") {
			t.Errorf("cache entry %q escaped the root", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestArtifactRouteRefusesNonAllowlistedHosts is the SSRF fence: the
// encoded-URL scheme must not let a job use the daemon as an open relay.
func TestArtifactRouteRefusesNonAllowlistedHosts(t *testing.T) {
	t.Parallel()
	// Configured as it would be in production: the public registry upstream,
	// which is never contacted here. Anything local is therefore off the
	// allowlist, which is exactly the case this fence exists for.
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
		"file:///etc/passwd",
	} {
		u := pkgcache.ArtifactURL(base, target)
		resp := get(t, noRedirect(), u, nil)
		got := body(t, resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("artifact fetch of %q returned 200 (%q)", target, got)
		}
		if strings.Contains(string(got), "cloud-metadata-credentials") {
			t.Fatalf("the proxy relayed a non-allowlisted host: %q", got)
		}
	}
}

// TestWritesAreRejected: this is a read-through cache, never a registry
// front end. Nothing may be relayed upstream that could publish or mutate.
func TestWritesAreRejected(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	base := "http://" + p.Addr()

	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequestWithContext(t.Context(), method, base+"/express", strings.NewReader("{}"))
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
	if reg.packumentHits.Load() != 0 {
		t.Error("a write method reached the upstream registry")
	}
}

func TestSizeCapEvicts(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	// Room for roughly two tarballs.
	budget := int64(2 * len(reg.tarball))
	p := startProxy(t, Config{Upstream: reg.URL, MaxBytes: budget})
	base := "http://" + p.Addr()

	for i := range 12 {
		u := pkgcache.ArtifactURL(base, fmt.Sprintf("%s/express/-/express-4.18.%d.tgz", reg.URL, i))
		resp := get(t, noRedirect(), u, nil)
		if got := body(t, resp); len(got) != len(reg.tarball) {
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
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL, ListenAddr: "127.0.0.1:0"})

	if !p.Healthy() {
		t.Error("a running proxy reported unhealthy")
	}
	if p.Name() != "npm" {
		t.Errorf("Name = %q", p.Name())
	}

	env := p.EnvVars()
	var sawRegistry, sawYarn bool
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "npm_config_registry":
			sawRegistry = true
			if _, err := url.Parse(v); err != nil || !strings.HasSuffix(v, "/") {
				t.Errorf("npm_config_registry = %q, want a URL ending in /", v)
			}
		case "YARN_NPM_REGISTRY_SERVER":
			sawYarn = true
		}
	}
	if !sawRegistry || !sawYarn {
		t.Errorf("EnvVars = %v, want npm_config_registry and YARN_NPM_REGISTRY_SERVER", env)
	}
}

func TestPassthroughIsNotCached(t *testing.T) {
	t.Parallel()
	reg := newFakeRegistry(t)
	p := startProxy(t, Config{Upstream: reg.URL})
	resp := get(t, noRedirect(), "http://"+p.Addr()+"/-/v1/search?text=express", nil)
	if got := body(t, resp); !strings.Contains(string(got), "objects") {
		t.Errorf("search passthrough body = %q", got)
	}
	if p.srv.Cache().Len() != 0 {
		t.Error("a passthrough response was cached")
	}
}

// --- unit tests on the pure helpers ---------------------------------------

func TestParsePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		pkg     string
		file    string
		wantErr bool
	}{
		{path: "/express", pkg: "express"},
		{path: "/@types/node", pkg: "@types/node"},
		{path: "/express/-/express-4.18.2.tgz", pkg: "express", file: "express-4.18.2.tgz"},
		{path: "/@types/node/-/node-20.1.0.tgz", pkg: "@types/node", file: "node-20.1.0.tgz"},

		{path: "/", wantErr: true},
		{path: "/@types", wantErr: true},                    // a bare scope is not a package
		{path: "/a/b", wantErr: true},                       // unscoped two-segment
		{path: "/express/4.18.2", wantErr: true},            // single-version manifest
		{path: "/../etc/passwd", wantErr: true},             //
		{path: "/express/-/../evil.tgz", wantErr: true},     //
		{path: "/express/-/evil.exe", wantErr: true},        // not a tarball
		{path: "/express/-/x.tgz/extra", wantErr: true},     //
		{path: "/-/express/-/x.tgz", wantErr: true},         //
		{path: "/@sc ope/node/-/node-1.tgz", wantErr: true}, //
		{path: "/express/-/" + strings.Repeat("a", 300) + ".tgz", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			pkg, file, ok := parsePath(tc.path)
			if tc.wantErr {
				if ok {
					t.Fatalf("parsePath(%q) = (%q, %q, true), want rejection", tc.path, pkg, file)
				}
				return
			}
			if !ok {
				t.Fatalf("parsePath(%q) rejected a valid path", tc.path)
			}
			if pkg != tc.pkg || file != tc.file {
				t.Errorf("parsePath(%q) = (%q, %q), want (%q, %q)", tc.path, pkg, file, tc.pkg, tc.file)
			}
		})
	}
}

func TestEscapePackage(t *testing.T) {
	t.Parallel()
	if got := escapePackage("express"); got != "express" {
		t.Errorf("escapePackage(express) = %q", got)
	}
	if got := escapePackage("@types/node"); got != "@types%2fnode" {
		t.Errorf("escapePackage(@types/node) = %q", got)
	}
}

func TestRewritePackumentPreservesUnknownFields(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"x","_rev":"7-abc","custom":{"a":1},"versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/x/-/x-1.0.0.tgz"},"extra":true}}}`)
	out, err := rewritePackument(in, "http://gw:8084", defaultAllowedHosts)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["_rev"] != "7-abc" {
		t.Error("_rev was dropped")
	}
	if doc["custom"].(map[string]any)["a"].(float64) != 1 {
		t.Error("an unknown field was dropped")
	}
	v := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)
	if v["extra"] != true {
		t.Error("an unknown version field was dropped")
	}
	if !strings.HasPrefix(v["dist"].(map[string]any)["tarball"].(string), "http://gw:8084"+pkgcache.ArtifactRoute) {
		t.Error("dist.tarball was not rewritten")
	}
}

func TestRewritePackumentTolerartesGarbage(t *testing.T) {
	t.Parallel()
	if _, err := rewritePackument([]byte("not json"), "http://gw", defaultAllowedHosts); err == nil {
		t.Error("expected an error for unparseable metadata so the caller can serve it through unchanged")
	}
	// Structurally odd but valid JSON must not panic.
	for _, in := range []string{`{"versions":"nope"}`, `{"versions":{"1.0.0":5}}`, `{"versions":{"1.0.0":{"dist":7}}}`, `{"dist":{"tarball":5}}`} {
		if _, err := rewritePackument([]byte(in), "http://gw", defaultAllowedHosts); err != nil {
			t.Errorf("rewritePackument(%s): %v", in, err)
		}
	}
}
