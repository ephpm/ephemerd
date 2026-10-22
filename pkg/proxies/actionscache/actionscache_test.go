package actionscache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// harness wires a Proxy to a fake registry. Every request in a test comes
// from 127.0.0.1, so ip maps it to whichever runner the test says.
type harness struct {
	t      *testing.T
	reg    *fakeRegistry
	p      *Proxy
	srv    *httptest.Server
	mu     sync.Mutex
	runner string // runner 127.0.0.1 is attributed to
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, runner: "runner-1"}
	h.reg, _ = newFakeRegistry(t)
	regSrv := httptest.NewServer(h.reg)
	t.Cleanup(regSrv.Close)

	cfg := Config{
		ListenAddr:       "127.0.0.1:0",
		AdvertiseBase:    "http://placeholder",
		Registry:         regSrv.URL,
		RegistryUser:     "cache",
		RegistryPassword: "s3cret",
		HealthInterval:   -1,
		JobForIP: func(ip string) string {
			h.mu.Lock()
			defer h.mu.Unlock()
			if ip == "127.0.0.1" {
				return h.runner
			}
			return ""
		},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	h.srv = httptest.NewServer(p)
	t.Cleanup(h.srv.Close)
	p.cfg.AdvertiseBase = h.srv.URL
	p.probe(context.Background())
	if !p.Healthy() {
		t.Fatal("proxy not healthy against the fake registry")
	}
	return h
}

func (h *harness) as(runner string) {
	h.mu.Lock()
	h.runner = runner
	h.mu.Unlock()
}

func (h *harness) twirp(method string, req map[string]any) map[string]any {
	h.t.Helper()
	b, _ := json.Marshal(req)
	resp, err := http.Post(h.srv.URL+twirpPrefix+method, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("%s: decoding response (status %s): %v", method, resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("%s: status %s, body %v (cache failures must be ok=false, not an HTTP error)", method, resp.Status, out)
	}
	return out
}

func put(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// save runs the client's whole save flow; blocks > 1 uses Put Block in
// REVERSE order (the SDK uploads concurrently) and a Put Block List.
func (h *harness) save(key, version string, data []byte, blocks int) map[string]any {
	h.t.Helper()
	cr := h.twirp("CreateCacheEntry", map[string]any{"key": key, "version": version})
	if cr["ok"] != true {
		return cr
	}
	up := cr["signed_upload_url"].(string)
	if blocks <= 1 {
		if code := put(h.t, up, data); code != http.StatusCreated {
			h.t.Fatalf("Put Blob: %d", code)
		}
	} else {
		size := (len(data) + blocks - 1) / blocks
		var ids []string
		for i := 0; i < blocks; i++ {
			ids = append(ids, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%06d", i))))
		}
		for i := blocks - 1; i >= 0; i-- {
			lo, hi := i*size, (i+1)*size
			if hi > len(data) {
				hi = len(data)
			}
			if code := put(h.t, up+"?comp=block&blockid="+ids[i], data[lo:hi]); code != http.StatusCreated {
				h.t.Fatalf("Put Block %d: %d", i, code)
			}
		}
		var xml strings.Builder
		xml.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
		for _, id := range ids {
			xml.WriteString("<Latest>" + id + "</Latest>")
		}
		xml.WriteString("</BlockList>")
		if code := put(h.t, up+"?comp=blocklist", []byte(xml.String())); code != http.StatusCreated {
			h.t.Fatalf("Put Block List: %d", code)
		}
	}
	return h.twirp("FinalizeCacheEntryUpload", map[string]any{
		"key": key, "version": version, "size_bytes": strconv.Itoa(len(data)),
	})
}

// restore runs the client's restore flow and returns (matched key, bytes),
// or ("", nil) on a miss.
func (h *harness) restore(key string, restoreKeys []string, version string) (string, []byte) {
	h.t.Helper()
	rk := []any{}
	for _, k := range restoreKeys {
		rk = append(rk, k)
	}
	r := h.twirp("GetCacheEntryDownloadURL", map[string]any{"key": key, "restore_keys": rk, "version": version})
	if r["ok"] != true {
		return "", nil
	}
	resp, err := http.Get(r["signed_download_url"].(string))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("download: %s", resp.Status)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(data)) {
		h.t.Fatalf("Content-Length %s but %d bytes arrived; the client rejects that as incomplete", cl, len(data))
	}
	return r["matched_key"].(string), data
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/7)
	}
	return b
}

var mainScope = Scope{Repo: "ephpm/ephpm", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}}

func TestSaveThenRestore_SingleShot(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)

	data := payload(3000)
	if fr := h.save("cargo-linux-abc", "v1", data, 1); fr["ok"] != true {
		t.Fatalf("finalize: %v", fr)
	}
	key, got := h.restore("cargo-linux-abc", nil, "v1")
	if key != "cargo-linux-abc" || !bytes.Equal(got, data) {
		t.Fatalf("restore = %q (%d bytes), want the saved entry back byte for byte", key, len(got))
	}
}

func TestSaveThenRestore_ConcurrentBlocksOutOfOrder(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)

	data := payload(10_007) // not a multiple of the block size
	if fr := h.save("k", "v", data, 5); fr["ok"] != true {
		t.Fatalf("finalize: %v", fr)
	}
	_, got := h.restore("k", nil, "v")
	if !bytes.Equal(got, data) {
		t.Fatal("blocks uploaded in reverse came back in the wrong order; the block list must decide order")
	}
	// Each block streamed through as its own blob (5 blocks + 1 config).
	if h.reg.patches != 6 {
		t.Errorf("registry saw %d blob uploads, want 6 (one per block plus the config)", h.reg.patches)
	}
}

func TestRestore_RangeAcrossBlocks(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	data := payload(1000)
	h.save("k", "v", data, 4)

	r := h.twirp("GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v"})
	req, _ := http.NewRequest(http.MethodGet, r["signed_download_url"].(string), nil)
	req.Header.Set("Range", "bytes=240-760")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, data[240:761]) {
		t.Fatalf("range 240-760 = %s, %d bytes; want 206 and the exact slice", resp.Status, len(got))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 240-760/1000" {
		t.Errorf("Content-Range = %q", cr)
	}
}

func TestRestoreKeys_PrefixNewestWinsAndVersionMustMatch(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)

	h.save("cargo-linux-aaa", "v1", []byte("old"), 1)
	time.Sleep(5 * time.Millisecond)
	h.save("cargo-linux-bbb", "v1", []byte("new"), 1)
	h.save("cargo-linux-ccc", "v2", []byte("other version"), 1)

	key, got := h.restore("cargo-linux-zzz", []string{"cargo-linux-"}, "v1")
	if key != "cargo-linux-bbb" || string(got) != "new" {
		t.Fatalf("prefix restore = %q %q; want the NEWEST v1 entry", key, got)
	}
	if key, _ := h.restore("cargo-linux-ccc", nil, "v1"); key != "" {
		t.Fatalf("exact key with a different version matched %q; versions must match", key)
	}
	if key, _ := h.restore("nope", []string{"zzz-"}, "v1"); key != "" {
		t.Fatalf("no match should miss, got %q", key)
	}
}

func TestScope_PRReadsMainButCannotWriteIt(t *testing.T) {
	h := newHarness(t)
	h.p.Track("main-runner")
	h.p.Bind("main-runner", mainScope)
	h.as("main-runner")
	h.save("deps-1", "v", []byte("from main"), 1)

	pr := Scope{Repo: "ephpm/ephpm", Write: "refs/pull/7/merge", Reads: []string{"refs/pull/7/merge", "refs/heads/main"}}
	h.p.Track("pr-runner")
	h.p.Bind("pr-runner", pr)
	h.as("pr-runner")

	if _, got := h.restore("deps-1", nil, "v"); string(got) != "from main" {
		t.Fatalf("PR restore of main's entry = %q; a PR must read the default branch", got)
	}
	// The PR saves a NEW key: it lands in the PR's scope only.
	if fr := h.save("deps-2", "v", []byte("from pr"), 1); fr["ok"] != true {
		t.Fatalf("PR save into its own scope: %v", fr)
	}
	h.as("main-runner")
	if key, _ := h.restore("deps-2", nil, "v"); key != "" {
		t.Fatal("main restored an entry a PR saved; PR writes must never be visible to main")
	}
	if n := h.reg.tagCount(scopeRepository("ephpm/ephpm", "refs/heads/main")); n != 1 {
		t.Fatalf("main's scope holds %d entries, want exactly main's own 1", n)
	}
}

func TestScope_ReadOnlyJobCannotSave(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", Scope{Repo: "ephpm/ephpm", Reads: []string{"refs/heads/main"}})
	if cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); cr["ok"] != false {
		t.Fatalf("read-only job reserved an entry: %v", cr)
	}
}

func TestScope_ReposAreIsolated(t *testing.T) {
	h := newHarness(t)
	h.p.Track("a")
	h.p.Bind("a", Scope{Repo: "org/foo.bar", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}})
	h.as("a")
	h.save("k", "v", []byte("foo.bar"), 1)

	// "foo-bar" squashes to the same path component; the hash must keep the
	// namespaces apart.
	h.p.Track("b")
	h.p.Bind("b", Scope{Repo: "org/foo-bar", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}})
	h.as("b")
	if key, _ := h.restore("k", nil, "v"); key != "" {
		t.Fatal("a different repo restored another repo's entry")
	}
}

func TestUnboundOrUnknownJobGetsMissesAndNoSaves(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	h.save("k", "v", []byte("x"), 1)

	h.p.Track("fresh") // tracked, not yet bound by in_progress
	h.as("fresh")
	if key, _ := h.restore("k", nil, "v"); key != "" {
		t.Fatal("an unbound runner got a hit")
	}
	if cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k2", "version": "v"}); cr["ok"] != false {
		t.Fatal("an unbound runner reserved an entry")
	}
	h.as("stranger") // not tracked at all
	if key, _ := h.restore("k", nil, "v"); key != "" {
		t.Fatal("an untracked source got a hit")
	}
}

func TestSignedURLsAreBoundToTheirJob(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	h.save("k", "v", []byte("secret build"), 1)
	r := h.twirp("GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v"})
	url := r["signed_download_url"].(string)

	h.p.Track("other")
	h.p.Bind("other", Scope{Repo: "evil/repo", Write: "refs/heads/main", Reads: []string{"refs/heads/main"}})
	h.as("other")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another job fetched a download URL it was not granted: %s", resp.Status)
	}
}

func TestConflictingRebindRevokes(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	h.p.Bind("runner-1", Scope{Repo: "ephpm/ephpm", Write: "refs/pull/9/merge", Reads: []string{"refs/pull/9/merge"}})
	if cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); cr["ok"] != false {
		t.Fatal("a runner bound twice to different scopes kept write access")
	}
}

func TestEntriesAreImmutable(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	h.save("k", "v", []byte("first"), 1)
	if cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"}); cr["ok"] != false {
		t.Fatal("a second save of an existing key was accepted")
	}
}

func TestFinalizeRejectsSizeMismatch(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"})
	_ = put(t, cr["signed_upload_url"].(string), []byte("12345"))
	fr := h.twirp("FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v", "size_bytes": 99})
	if fr["ok"] != false {
		t.Fatal("finalize accepted a size that does not match what was uploaded")
	}
}

func TestEntrySizeLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxEntryBytes = 100 })
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"})
	if code := put(t, cr["signed_upload_url"].(string), payload(101)); code >= 500 || code < 400 {
		t.Fatalf("oversize upload answered %d; want a non-retryable 4xx", code)
	}
	if fr := h.twirp("FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": "v", "size_bytes": 101}); fr["ok"] != false {
		t.Fatal("an oversize entry was finalized")
	}
}

func TestRegistryDownIsAMiss(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	h.save("k", "v", []byte("x"), 1)

	h.reg.setDown(true)
	h.p.probe(context.Background())
	if h.p.Healthy() {
		t.Fatal("still healthy with the registry down")
	}
	if key, _ := h.restore("k", nil, "v"); key != "" {
		t.Fatal("got a hit with the registry down")
	}
	if cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k2", "version": "v"}); cr["ok"] != false {
		t.Fatal("reserved an entry with the registry down")
	}
}

func TestEvictionDropsLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxBytesPerRepo = 250 })
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)

	h.save("a", "v", payload(100), 1)
	time.Sleep(5 * time.Millisecond)
	h.save("b", "v", payload(100), 1)
	time.Sleep(5 * time.Millisecond)
	h.restore("a", nil, "v") // a is now the most recently USED
	time.Sleep(5 * time.Millisecond)
	h.save("c", "v", payload(100), 1) // 300 > 250: one must go

	if key, _ := h.restore("b", nil, "v"); key != "" {
		t.Fatal("b (least recently used) survived eviction")
	}
	for _, k := range []string{"a", "c"} {
		if key, _ := h.restore(k, nil, "v"); key != k {
			t.Fatalf("%s was evicted; only the least recently used entry should go", k)
		}
	}
}

func TestForgetRevokesEverything(t *testing.T) {
	h := newHarness(t)
	h.p.Track("runner-1")
	h.p.Bind("runner-1", mainScope)
	cr := h.twirp("CreateCacheEntry", map[string]any{"key": "k", "version": "v"})
	h.p.Forget("runner-1")
	if code := put(t, cr["signed_upload_url"].(string), []byte("x")); code != http.StatusNotFound {
		t.Fatalf("upload after Forget answered %d; want 404", code)
	}
}

func TestRelayPassesNonCacheCallsToGitHub(t *testing.T) {
	var gotAuth, gotPath, gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotHost = r.Header.Get("Authorization"), r.URL.Path, r.Host
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	h := newHarness(t, func(c *Config) { c.UpstreamResultsURL = upstream.URL })
	h.p.Track("runner-1")

	const art = "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact"
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+art, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer job-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || gotPath != art || gotAuth != "Bearer job-token" {
		t.Fatalf("relay: status %s path %q auth %q; artifacts must reach GitHub unchanged", resp.Status, gotPath, gotAuth)
	}
	if !strings.Contains(upstream.URL, gotHost) {
		t.Errorf("relay sent Host %q, want the upstream's", gotHost)
	}

	h.as("stranger")
	resp, err = http.Post(h.srv.URL+art, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an untracked source used the relay: %s; it must not be an open relay", resp.Status)
	}
}

func TestTwirpAcceptsNumericAndStringInt64(t *testing.T) {
	for _, raw := range []string{`{"size_bytes":"12"}`, `{"size_bytes":12}`} {
		var r twirpReq
		if err := json.Unmarshal([]byte(raw), &r); err != nil || r.SizeBytes != 12 {
			t.Errorf("%s -> %d, %v", raw, r.SizeBytes, err)
		}
	}
}

func TestParseBlockList(t *testing.T) {
	ids, err := parseBlockList(strings.NewReader(`<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>QQ==</Latest><Committed>Qg==</Committed><Uncommitted>Qw==</Uncommitted></BlockList>`))
	if err != nil || strings.Join(ids, ",") != "QQ==,Qg==,Qw==" {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	for _, bad := range []string{``, `<BlockList></BlockList>`, `<BlockList><Latest>not base64!</Latest></BlockList>`, `<Latest>QQ==</Latest>`} {
		if _, err := parseBlockList(strings.NewReader(bad)); err == nil {
			t.Errorf("parseBlockList(%q) accepted a bad block list", bad)
		}
	}
}

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		h    string
		s, e int64
		ok   bool
	}{
		{"bytes=0-9", 0, 9, true},
		{"bytes=5-", 5, 99, true},
		{"bytes=-10", 90, 99, true},
		{"bytes=90-200", 90, 99, true},
		{"bytes=100-", 0, 0, false},
		{"bytes=5-2", 0, 0, false},
		{"bytes=0-1,4-5", 0, 0, false},
		{"items=0-1", 0, 0, false},
	} {
		s, e, ok := parseRange(tc.h, 100)
		if ok != tc.ok || (ok && (s != tc.s || e != tc.e)) {
			t.Errorf("parseRange(%q) = %d,%d,%v want %d,%d,%v", tc.h, s, e, ok, tc.s, tc.e, tc.ok)
		}
	}
}

func TestNaming(t *testing.T) {
	ns := repoNamespace("EPHPM/ephpm")
	if ns != repoNamespace("ephpm/ephpm") {
		t.Error("repo namespace is case-sensitive; GitHub repo names are not")
	}
	if repoNamespace("org/foo.bar") == repoNamespace("org/foo-bar") {
		t.Error("two distinct repos share a namespace")
	}
	if scopeRepository("o/r", "refs/heads/main") == scopeRepository("o/r", "refs/pull/1/merge") {
		t.Error("two refs share a scope repository")
	}
	if entryTag("k", "v1") == entryTag("k", "v2") {
		t.Error("version is not part of the tag")
	}
	if tag := entryTag(strings.Repeat("x", 512), "v"); len(tag) > 128 {
		t.Errorf("tag %d chars; registry tags are at most 128", len(tag))
	}
	for _, s := range []string{ns, scopeRepository("Weird__Org/..repo..", "refs/heads/a/b")} {
		for _, comp := range strings.Split(s, "/") {
			if comp == "" || strings.Trim(comp, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" || strings.HasPrefix(comp, "-") || strings.HasSuffix(comp, "-") || strings.Contains(comp, "--") {
				t.Errorf("%q has an invalid registry path component %q", s, comp)
			}
		}
	}
}
