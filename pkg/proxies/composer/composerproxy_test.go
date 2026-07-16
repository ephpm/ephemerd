package composerproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProxyCachesMetadata(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"packages":{"path":"`+r.URL.Path+`"}}`)
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
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

	const metaPath = "/p2/monolog/monolog.json"
	first := get(metaPath)
	if !strings.Contains(first, "monolog") {
		t.Fatalf("unexpected body: %q", first)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 upstream hit after first request, got %d", got)
	}

	second := get(metaPath)
	if second != first {
		t.Fatalf("cache-hit body mismatch: %q vs %q", second, first)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected still 1 upstream hit (cache hit), got %d", got)
	}
}

func TestPackagesJSONNotCached(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"metadata-url":"/p2/%package%.json"}`)
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Cleanup:    true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	for i := 0; i < 2; i++ {
		resp, err := http.Get("http://" + p.Addr() + "/packages.json")
		if err != nil {
			t.Fatalf("GET packages.json: %v", err)
		}
		_ = resp.Body.Close()
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected packages.json to pass through each time (2 hits), got %d", got)
	}
}

func TestEnvVars(t *testing.T) {
	p := New(Config{ListenAddr: "10.88.0.1:8084"})
	env := p.EnvVars()
	if len(env) != 1 || env[0] != "COMPOSER_REPO_PACKAGIST=http://10.88.0.1:8084" {
		t.Fatalf("unexpected env vars: %v", env)
	}
}
