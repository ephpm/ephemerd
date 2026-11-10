package runnerversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/diskspace"
)

// Fetcher downloads a runner archive into the Store with every guard the
// design requires: https only, free-space check before the first byte, a
// size cap on the wire, hash-verify before commit (Store.Put), and
// single-flight per SHA so concurrent callers share one download.
type Fetcher struct {
	Store *Store
	// Client is the HTTP client; nil uses a 10-minute-timeout default.
	Client *http.Client
	// MaxBytes caps the archive size. 0 = DefaultMaxArchiveBytes.
	MaxBytes int64
	// MinFreeBytes refuses a fetch when the cache volume has less free.
	// 0 = DefaultMinFreeBytes.
	MinFreeBytes uint64
	// DiskCheck is diskspace.Check; replaceable for tests.
	DiskCheck func(path string) (diskspace.Usage, error)
	Log       *slog.Logger

	mu       sync.Mutex
	inflight map[string]*fetchCall
}

type fetchCall struct {
	done chan struct{}
	path string
	err  error
}

const (
	// DefaultMaxArchiveBytes is 512 MiB; the runner is ~150-200 MB.
	DefaultMaxArchiveBytes int64 = 512 << 20
	// DefaultMinFreeBytes is 5 GiB.
	DefaultMinFreeBytes uint64 = 5 << 30
)

var (
	// ErrInsufficientDisk means the cache volume is below MinFreeBytes.
	ErrInsufficientDisk = errors.New("insufficient free disk for runner archive")
	// ErrFetch wraps transport failures.
	ErrFetch = errors.New("runner archive fetch failed")
)

// Fetch downloads url and commits it as meta.SHA256. meta.Platform decides
// the extension. Returns the blob path. Already-present blobs return at
// once without touching the network.
func (f *Fetcher) Fetch(ctx context.Context, url string, meta Sidecar) (string, error) {
	if f.Store == nil {
		return "", errors.New("fetcher has no store")
	}
	if !sha256Re.MatchString(meta.SHA256) {
		return "", fmt.Errorf("%w: expected sha256 is not 64 hex chars", ErrChecksumMismatch)
	}
	if p := f.Store.blobPath(meta.SHA256, ArchiveExt(meta.Platform)); fileExists(p) {
		return p, nil
	}
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("%w: url must be https", ErrFetch)
	}

	f.mu.Lock()
	if f.inflight == nil {
		f.inflight = map[string]*fetchCall{}
	}
	if c, ok := f.inflight[meta.SHA256]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.path, c.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	c := &fetchCall{done: make(chan struct{})}
	f.inflight[meta.SHA256] = c
	f.mu.Unlock()

	c.path, c.err = f.fetch(ctx, url, meta)
	close(c.done)

	f.mu.Lock()
	delete(f.inflight, meta.SHA256)
	f.mu.Unlock()
	return c.path, c.err
}

func (f *Fetcher) fetch(ctx context.Context, url string, meta Sidecar) (string, error) {
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxArchiveBytes
	}
	minFree := f.MinFreeBytes
	if minFree == 0 {
		minFree = DefaultMinFreeBytes
	}
	check := f.DiskCheck
	if check == nil {
		check = diskspace.Check
	}
	usage, err := check(f.Store.Root())
	if err != nil {
		log.Warn("disk check before runner fetch failed; proceeding", "error", err)
	} else if usage.FreeBytes < minFree {
		return "", fmt.Errorf("%w: %s free at %s, need %s", ErrInsufficientDisk,
			fmtGiB(usage.FreeBytes), f.Store.Root(), fmtGiB(minFree))
	}

	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrFetch, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrFetch, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warn("closing fetch body", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: GET %s: %s", ErrFetch, url, resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return "", fmt.Errorf("%w: content-length %d > %d", ErrTooLarge, resp.ContentLength, maxBytes)
	}
	log.Info("fetching runner archive", "url", url, "sha256", meta.SHA256, "version", meta.Version)
	meta.URL = url
	path, err := f.Store.Put(resp.Body, meta, maxBytes)
	if err != nil {
		return "", err
	}
	return path, nil
}

func fmtGiB(b uint64) string {
	return fmt.Sprintf("%.1f GiB", diskspace.GiB(b))
}
