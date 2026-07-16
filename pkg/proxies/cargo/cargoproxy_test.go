package cargoproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyCachesCrateDownloads(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, "crate-bytes-for-"+r.URL.Path)
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Downstream: upstream.URL,
		Upstream:   upstream.URL, // unused in this test but must be set
		ListenAddr: "127.0.0.1:0",
		Cleanup:    true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	get := func(path string) string {
		t.Helper()
		resp, err := http.Get("http://" + p.Addr() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	const cratePath = "/dl/serde/1.0.0/download"
	first := get(cratePath)
	if !strings.Contains(first, "crate-bytes-for-") {
		t.Fatalf("unexpected body: %q", first)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 upstream hit after first request, got %d", got)
	}

	second := get(cratePath)
	if second != first {
		t.Fatalf("cache-hit body mismatch: %q vs %q", second, first)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected still 1 upstream hit (cache hit), got %d", got)
	}
}

func TestProxyIndexConfigRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config.json" {
			_, _ = io.WriteString(w, `{"dl":"https://static.crates.io/crates","api":"https://crates.io"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		Downstream: upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Cleanup:    true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	resp, err := http.Get("http://" + p.Addr() + "/index/config.json")
	if err != nil {
		t.Fatalf("GET config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)

	wantDL := `"dl": "http://` + p.Addr() + `/dl"`
	if !strings.Contains(body, wantDL) {
		t.Fatalf("config.json dl not rewritten; got %q", body)
	}
	if !strings.Contains(body, `"api":"https://crates.io"`) {
		t.Fatalf("config.json api field lost; got %q", body)
	}
}

func TestEnvVars(t *testing.T) {
	p := New(Config{ListenAddr: "10.88.0.1:8083"})
	env := p.EnvVars()

	want := map[string]bool{
		"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse":                                 false,
		"CARGO_REGISTRIES_EPHEMERD_CRATES_INDEX=sparse+http://10.88.0.1:8083/index/": false,
		"CARGO_SOURCE_CRATES_IO_REPLACE_WITH=ephemerd-crates":                        false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing env var: %q (got %v)", k, env)
		}
	}
}
