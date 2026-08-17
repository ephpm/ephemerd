package pkgcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// DefaultTimeout bounds one upstream request. Generous: a cold PyPI wheel or
// an npm tarball for a large toolchain can legitimately take minutes on a
// slow link, and timing it out would turn a slow build into a failed one.
const DefaultTimeout = 10 * time.Minute

// Fetcher is the pull-through read path shared by all three proxies.
type Fetcher struct {
	Cache  *Cache
	Client *http.Client
	Log    *slog.Logger
}

// NewFetcher wires a Fetcher with a sane HTTP client.
func NewFetcher(c *Cache, log *slog.Logger) *Fetcher {
	if log == nil {
		log = slog.Default()
	}
	return &Fetcher{
		Cache:  c,
		Client: &http.Client{Timeout: DefaultTimeout},
		Log:    log,
	}
}

// Request describes one pull-through fetch.
type Request struct {
	// Key is the cache key. Must survive Cache.KeyPath.
	Key string
	// URL is the upstream URL to fetch on a miss.
	URL string
	// Immutable marks content that, once fetched, is never refetched and
	// never revalidated: npm tarballs, PyPI wheels/sdists, pub archives.
	// Every one of those is addressed by a (name, version) that a registry
	// will not re-publish with different bytes.
	Immutable bool
	// TTL is how long a MUTABLE entry is served without contacting
	// upstream. After it, the entry is revalidated with a conditional GET,
	// so the steady-state cost of a stale-but-unchanged document is a 304.
	// Zero or negative means "revalidate every time".
	TTL time.Duration
	// Accept, when set, is forwarded upstream. Content negotiation is part
	// of the cache identity for pip (PEP 691 JSON vs PEP 503 HTML) and npm
	// (abbreviated vs full packument), so callers that vary on Accept MUST
	// also vary their Key.
	Accept string
	// DefaultContentType is used when upstream does not supply one.
	DefaultContentType string
}

// NotFoundError marks an upstream 404/410 so a caller can pass it through as
// a 404 rather than treating it as an outage. A nonexistent package version
// must stay distinguishable from a registry being down: the first is a real
// answer the job should see, the second is what fail-open exists for.
type NotFoundError struct{ URL string }

func (e *NotFoundError) Error() string { return "upstream 404 for " + e.URL }

// IsNotFound reports whether err is an upstream 404/410.
func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}

// Document fetches a whole document through the cache and returns its bytes.
// Intended for the small MUTABLE documents each proxy has to rewrite anyway
// (npm packuments, PEP 503 index pages, pub version listings).
//
// FAIL-OPEN, layer one: when upstream errors, times out, or answers 5xx and
// a cached copy exists, the STALE COPY IS SERVED with a warning. A registry
// outage then shows up as a slightly out-of-date index rather than a red CI
// job. Callers layer a second fail-open on top (a redirect to the origin)
// for the case where nothing is cached at all.
func (f *Fetcher) Document(ctx context.Context, req Request) ([]byte, Meta, error) {
	// Fast path: a fresh (or immutable) hit needs neither a lock nor a
	// network round trip.
	if body, meta, ok := f.Cache.Read(req.Key); ok {
		if decide(true, req.Immutable, meta.Fetched, time.Now(), req.TTL) == serveCached {
			return body, meta, nil
		}
	}

	mu := f.Cache.Lock(req.Key)
	mu.Lock()
	defer mu.Unlock()

	body, meta, cached := f.Cache.Read(req.Key)
	decision := decide(cached, req.Immutable, meta.Fetched, time.Now(), req.TTL)
	if decision == serveCached {
		return body, meta, nil
	}

	resp, err := f.do(ctx, req, decision == revalidate, meta)
	if err != nil {
		if cached {
			f.Log.Warn("upstream unreachable; serving stale cache", "url", req.URL, "error", err)
			return body, meta, nil
		}
		return nil, Meta{}, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			f.Log.Debug("closing upstream body", "url", req.URL, "error", cerr)
		}
	}()

	switch {
	case resp.StatusCode == http.StatusNotModified && cached:
		meta.Fetched = time.Now()
		f.Cache.refreshMeta(req.Key, meta)
		return body, meta, nil

	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, Meta{}, &NotFoundError{URL: req.URL}

	case resp.StatusCode != http.StatusOK:
		if cached {
			f.Log.Warn("upstream error; serving stale cache", "url", req.URL, "status", resp.StatusCode)
			return body, meta, nil
		}
		return nil, Meta{}, fmt.Errorf("fetching %s: upstream status %d", req.URL, resp.StatusCode)
	}

	fresh, err := io.ReadAll(resp.Body)
	if err != nil {
		if cached {
			f.Log.Warn("upstream read failed; serving stale cache", "url", req.URL, "error", err)
			return body, meta, nil
		}
		return nil, Meta{}, fmt.Errorf("reading %s: %w", req.URL, err)
	}

	newMeta := metaFrom(resp, req)
	// A cache-write failure must never fail the request: the bytes are
	// already in hand, the job just loses the caching benefit.
	if err := f.Cache.Write(req.Key, fresh, newMeta); err != nil {
		f.Log.Warn("caching document failed; serving anyway", "url", req.URL, "error", err)
	}
	return fresh, newMeta, nil
}

// ServeArtifact streams an IMMUTABLE artifact to the client, populating the
// cache on the way through.
//
// Nothing is written to w until the upstream response is known good, so a
// caller can still fail open with a redirect after this returns an error.
//
// The body is streamed, never buffered: a single PyPI wheel can be a
// gigabyte, and several jobs pull concurrently.
func (f *Fetcher) ServeArtifact(w http.ResponseWriter, r *http.Request, req Request) error {
	if file, meta, ok := f.Cache.Open(req.Key); ok {
		defer func() {
			if cerr := file.Close(); cerr != nil {
				f.Log.Debug("closing cached artifact", "key", req.Key, "error", cerr)
			}
		}()
		w.Header().Set("Content-Type", contentTypeOr(meta.ContentType, req.DefaultContentType))
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if meta.ETag != "" {
			w.Header().Set("ETag", meta.ETag)
		}
		f.Log.Debug("package cache hit", "key", req.Key, "url", req.URL)
		// ServeContent handles Range, If-Modified-Since and HEAD. The empty
		// name suppresses its content sniffing — the type is already set.
		http.ServeContent(w, r, "", meta.Fetched, file)
		return nil
	}

	mu := f.Cache.Lock(req.Key)
	mu.Lock()
	defer mu.Unlock()

	// Another goroutine may have filled it while we waited for the lock.
	if file, meta, ok := f.Cache.Open(req.Key); ok {
		defer func() {
			if cerr := file.Close(); cerr != nil {
				f.Log.Debug("closing cached artifact", "key", req.Key, "error", cerr)
			}
		}()
		w.Header().Set("Content-Type", contentTypeOr(meta.ContentType, req.DefaultContentType))
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, "", meta.Fetched, file)
		return nil
	}

	f.Log.Debug("package cache miss", "key", req.Key, "url", req.URL)
	resp, err := f.do(r.Context(), req, false, Meta{})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			f.Log.Debug("closing upstream body", "url", req.URL, "error", cerr)
		}
	}()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return &NotFoundError{URL: req.URL}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("fetching %s: upstream status %d", req.URL, resp.StatusCode)
	}

	meta := metaFrom(resp, req)
	w.Header().Set("Content-Type", contentTypeOr(meta.ContentType, req.DefaultContentType))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		// Do not spend the upstream bytes on a HEAD; the next GET caches it.
		return nil
	}

	cw, err := f.Cache.Writer(req.Key)
	if err != nil {
		// Cannot cache — still serve the job. Losing the cache entry is a
		// performance regression; failing the download is an outage.
		f.Log.Warn("cannot stage cache entry; streaming uncached", "key", req.Key, "error", err)
		if _, cerr := io.Copy(w, resp.Body); cerr != nil {
			f.Log.Debug("streaming uncached artifact", "url", req.URL, "error", cerr)
		}
		return nil
	}

	sink := &teeSink{file: cw, client: w, log: f.Log}
	if _, err := io.Copy(sink, resp.Body); err != nil {
		// The upstream body was truncated. A partial artifact must never be
		// installed: the next job would get a corrupt tarball forever.
		cw.Abort()
		f.Log.Warn("upstream artifact truncated; not caching", "url", req.URL, "error", err)
		return nil // headers are already out; the client sees a short body
	}
	if err := cw.Commit(meta); err != nil {
		f.Log.Warn("caching artifact failed; job was served anyway", "url", req.URL, "error", err)
	}
	return nil
}

// teeSink writes to the cache and the client, and keeps writing to the cache
// after the client goes away. A job that cancels a download half-way (a
// cancelled workflow, a `timeout`) would otherwise leave the cache cold for
// the next one.
type teeSink struct {
	file       io.Writer
	client     io.Writer
	log        *slog.Logger
	clientDead bool
}

func (t *teeSink) Write(p []byte) (int, error) {
	if _, err := t.file.Write(p); err != nil {
		return 0, err
	}
	if !t.clientDead {
		if _, err := t.client.Write(p); err != nil {
			t.clientDead = true
			t.log.Debug("client went away mid-artifact; still filling the cache", "error", err)
		}
	}
	return len(p), nil
}

// do issues one upstream request.
//
// Deliberately minimal: NO client headers are forwarded beyond Accept, and
// no Authorization, Cookie or npm auth token ever leaves this process. These
// proxies serve PUBLIC registry content only — forwarding credentials would
// turn a shared node-wide cache into a place where one job's token fetches
// another job's private package.
func (f *Fetcher) do(ctx context.Context, req Request, conditional bool, meta Meta) (*http.Response, error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("building upstream request for %s: %w", req.URL, err)
	}
	if req.Accept != "" {
		hr.Header.Set("Accept", req.Accept)
	}
	hr.Header.Set("User-Agent", "ephemerd-pkgcache/1")
	if conditional {
		if meta.ETag != "" {
			hr.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			hr.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}
	resp, err := f.Client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", req.URL, err)
	}
	return resp, nil
}

func metaFrom(resp *http.Response, req Request) Meta {
	return Meta{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  contentTypeOr(resp.Header.Get("Content-Type"), req.DefaultContentType),
		Fetched:      time.Now(),
		URL:          req.URL,
	}
}

func contentTypeOr(got, fallback string) string {
	if got == "" {
		return fallback
	}
	return got
}

// freshness is the cache decision for one request, kept as a pure value so
// the whole TTL/revalidation policy is table-testable without a server, a
// clock or a filesystem.
type freshness int

const (
	fetchFresh  freshness = iota // nothing usable on disk
	serveCached                  // within TTL, or immutable: no network at all
	revalidate                   // aged out: conditional GET
)

func (f freshness) String() string {
	switch f {
	case fetchFresh:
		return "fetch"
	case serveCached:
		return "hit"
	case revalidate:
		return "revalidate"
	default:
		return "unknown"
	}
}

// decide is the entire cache policy.
//
//   - IMMUTABLE content is content-addressed by its URL: once cached it is
//     never refetched and never revalidated, whatever its age.
//   - MUTABLE content is served from cache inside ttl and conditionally
//     revalidated after that, so a new package version never takes an hour
//     to become visible and an indefinitely stale index is impossible.
//
// A zero or negative ttl means "always revalidate" — the safe direction for
// mutable data.
func decide(cached, immutable bool, fetched, now time.Time, ttl time.Duration) freshness {
	if !cached {
		return fetchFresh
	}
	if immutable {
		return serveCached
	}
	if ttl > 0 && now.Sub(fetched) < ttl {
		return serveCached
	}
	return revalidate
}
