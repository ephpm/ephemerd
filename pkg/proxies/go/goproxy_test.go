package goproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCacheMiss_FetchesFromUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("module-content"))
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	resp, err := http.Get("http://" + p.Addr() + "/example.com/mod/@v/v1.0.0.zip")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
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
		w.Write([]byte("from-upstream"))
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	url := "http://" + p.Addr() + "/example.com/mod/@v/v1.0.0.mod"

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("first GET error: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if fetchCount.Load() != 1 {
		t.Fatalf("expected 1 upstream fetch after first request, got %d", fetchCount.Load())
	}

	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("second GET error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

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
		w.Write([]byte("version-list"))
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	for _, path := range []string{"/@v/list", "/@latest"} {
		fetchCount.Store(0)
		url := "http://" + p.Addr() + "/example.com/mod" + path

		for range 3 {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET %s error: %v", path, err)
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
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
		w.Write([]byte("zip-data"))
	}))
	defer upstream.Close()

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

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
			io.ReadAll(resp.Body)
			resp.Body.Close()
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

	p := New(Config{
		CacheDir:   t.TempDir(),
		Upstream:   upstream.URL,
		ListenAddr: "127.0.0.1:0",
		Log:        testLogger(),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer p.Stop()

	resp, err := http.Get("http://" + p.Addr() + "/example.com/mod/@v/v9.9.9.zip")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	resp.Body.Close()

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
