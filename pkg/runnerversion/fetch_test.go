package runnerversion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ephpm/ephemerd/pkg/diskspace"
)

// httpsTestServer gives a TLS server whose client is told to trust it,
// so the https-only rule can be exercised against a real listener.
func httpsTestServer(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

func plentyOfDisk(string) (diskspace.Usage, error) {
	return diskspace.Usage{TotalBytes: 100 << 30, FreeBytes: 50 << 30}, nil
}

func newFetcher(t *testing.T, client *http.Client) (*Fetcher, *Store) {
	t.Helper()
	s := newTestStore(t)
	return &Fetcher{Store: s, Client: client, DiskCheck: plentyOfDisk, Log: testLog()}, s
}

func TestFetchHappyPath(t *testing.T) {
	data, sha := fakeArchive(t, "echo fetched")
	var hits atomic.Int32
	srv, client := httpsTestServer(t, serveBytes(t, data, &hits))
	f, s := newFetcher(t, client)
	p, err := f.Fetch(context.Background(), srv.URL+"/"+ArchiveFilename("linux-x64", "2.338.2"), Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, sha+".tar.gz") || !s.Has(sha, "tar.gz") {
		t.Fatalf("blob path %s", p)
	}
	// Second fetch is served from the store.
	if _, err := f.Fetch(context.Background(), srv.URL+"/x", Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", hits.Load())
	}
	sc, err := s.Sidecar(sha)
	if err != nil || !strings.HasPrefix(sc.URL, srv.URL) {
		t.Fatalf("sidecar url: %v %+v", err, sc)
	}
}

func TestFetchChecksumMismatchLeavesNothing(t *testing.T) {
	data, _ := fakeArchive(t, "echo evil")
	srv, client := httpsTestServer(t, serveBytes(t, data, nil))
	f, s := newFetcher(t, client)
	_, err := f.Fetch(context.Background(), srv.URL+"/a", Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: zeroSHA})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if blobs := listBlobs(t, s); len(blobs) != 0 {
		t.Fatalf("blobs after mismatch: %v", blobs)
	}
}

func TestFetchRefusesOversizeAndPlainHTTP(t *testing.T) {
	data, sha := fakeArchive(t, "echo big")
	srv, client := httpsTestServer(t, serveBytes(t, data, nil))
	f, _ := newFetcher(t, client)
	f.MaxBytes = int64(len(data)) - 1
	_, err := f.Fetch(context.Background(), srv.URL+"/a", Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	f.MaxBytes = 0
	_, err = f.Fetch(context.Background(), "http://example.com/a", Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha})
	if !errors.Is(err, ErrFetch) {
		t.Fatalf("expected ErrFetch for http, got %v", err)
	}
}

func TestFetchRefusesLowDisk(t *testing.T) {
	data, sha := fakeArchive(t, "echo disk")
	var hits atomic.Int32
	srv, client := httpsTestServer(t, serveBytes(t, data, &hits))
	f, _ := newFetcher(t, client)
	f.DiskCheck = func(string) (diskspace.Usage, error) {
		return diskspace.Usage{TotalBytes: 10 << 30, FreeBytes: 1 << 30}, nil
	}
	_, err := f.Fetch(context.Background(), srv.URL+"/a", Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha})
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("expected ErrInsufficientDisk, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("must not touch the network when disk is low")
	}
}

func TestFetchSingleFlight(t *testing.T) {
	data, sha := fakeArchive(t, "echo once")
	var hits atomic.Int32
	release := make(chan struct{})
	srv, client := httpsTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		if _, err := w.Write(data); err != nil {
			t.Errorf("serving: %v", err)
		}
	}))
	f, _ := newFetcher(t, client)
	meta := Sidecar{Version: "2.338.2", Platform: "linux-x64", SHA256: sha}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.Fetch(context.Background(), srv.URL+"/a", meta)
		}(i)
	}
	// Let the goroutines pile up on the inflight call, then release.
	for f.inflightCount() == 0 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", hits.Load())
	}
}

func (f *Fetcher) inflightCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inflight)
}
