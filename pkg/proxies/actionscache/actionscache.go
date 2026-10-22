// Package actionscache serves GitHub Actions' cache service (v2) from the
// LAN, backed by a writable OCI registry, so `actions/cache` and everything
// built on it (Swatinem/rust-cache, setup-* caching) stop moving
// gigabyte-sized archives over the WAN on every job.
//
// How a job reaches it. The runner exports ACTIONS_RESULTS_URL to every step
// from the job message, overwriting the container's environment, so ephemerd
// patches that name out of the job's own copy of Runner.Worker.dll (see
// pkg/runner.PatchResultsURL) and sets ACTIONS_RESULTS_URL to this server.
// The cache client then talks to us; GitHub's real address survives under a
// renamed variable and is what we relay everything that is not the cache to.
//
// Who a request is from. The cache client builds its twirp URLs with an
// absolute path ("/twirp/..."), which discards any path we could have put a
// per-job secret in, and it sends GitHub's own runtime token, which we cannot
// verify. So a request is attributed by its SOURCE ADDRESS: every container
// of a job joins that job's network under the job's id (networking.JobForIP),
// and job containers cannot spoof another's address on the bridge. The upload
// and download URLs we hand out are used verbatim, so those additionally carry
// an unguessable token.
//
// What a job may touch. Not decided here: a job is unbound until the
// scheduler binds it to a Scope derived from the forge's record of the job
// that ACTUALLY took the runner (GitHub hands same-label JIT runners to any
// queued job, so the job a runner was started for is not evidence). Until
// then every lookup misses and every save is refused.
//
// Failure is always a cache miss. Unbound, registry down, entry missing:
// the twirp answer is ok=false (which the client treats as "no cache" or
// "could not reserve", warning and moving on), never a 5xx, which it would
// retry five times and then log as an error.
package actionscache

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures the cache server.
type Config struct {
	// ListenAddr is the address to bind, normally the bridge gateway
	// ("10.88.0.1:8089").
	ListenAddr string
	// AdvertiseBase is the base URL job containers reach this server at
	// ("http://10.88.0.1:8089"). Upload and download URLs are built on it.
	AdvertiseBase string

	// Registry is the writable registry's base URL ("http://cache:5001").
	Registry         string
	RegistryUser     string
	RegistryPassword string

	// MaxBytesPerRepo caps each forge repo's total cache; the least
	// recently used entries are deleted past it. 0 disables eviction.
	MaxBytesPerRepo int64
	// MaxEntryBytes refuses a single save larger than this. Default 10 GiB
	// (GitHub's own per-repo limit).
	MaxEntryBytes int64

	// UpstreamResultsURL is GitHub's results service, where every request
	// that is not the cache (artifact uploads, …) is relayed unchanged.
	// Default https://results-receiver.actions.githubusercontent.com.
	UpstreamResultsURL string

	// JobForIP attributes a request to a job by its source address
	// (networking.Manager.JobForIP). Required.
	JobForIP func(ip string) string

	// HealthInterval is how often the registry is probed; negative disables
	// the loop (tests). Default 30s.
	HealthInterval time.Duration

	// HTTPClient talks to the registry. Default: a client without a total
	// timeout (blobs are large) but with dial/response-header bounds.
	HTTPClient *http.Client

	Log *slog.Logger
}

const (
	defaultUpstreamResultsURL = "https://results-receiver.actions.githubusercontent.com"
	defaultMaxEntryBytes      = 10 << 30
	defaultHealthInterval     = 30 * time.Second

	twirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"
	blobPrefix  = "/_ephemerd/actions-cache/"

	pendingTTL  = 2 * time.Hour
	downloadTTL = time.Hour
	sweepEvery  = 10 * time.Minute
)

// jobState is what the server knows about one runner.
type jobState struct {
	bound bool
	scope Scope
}

// pending is a save between CreateCacheEntry and FinalizeCacheEntryUpload.
type pending struct {
	runner  string
	scope   Scope
	key     string
	version string
	created time.Time

	mu     sync.Mutex
	blocks map[string]descriptor // Azure block id -> uploaded layer
	order  []string              // committed block list ("" = single-shot)
	size   int64
	failed error
}

// download is a granted restore: the entry snapshot it may stream.
type download struct {
	runner  string
	e       *entry
	expires time.Time
}

// Proxy is the cache server.
type Proxy struct {
	cfg      Config
	log      *slog.Logger
	reg      *registry
	ix       *index
	upstream *url.URL
	relay    *httputil.ReverseProxy

	healthy atomic.Bool

	mu        sync.Mutex
	jobs      map[string]*jobState // runner name -> state
	uploads   map[string]*pending  // upload token -> pending
	byKey     map[string]*pending  // runner+key+version -> pending
	downloads map[string]*download // download token -> grant

	srv    *http.Server
	ln     net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New validates cfg and builds the server. It does not listen until Start.
func New(cfg Config) (*Proxy, error) {
	if cfg.JobForIP == nil {
		return nil, errors.New("actionscache: JobForIP is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxEntryBytes <= 0 {
		cfg.MaxEntryBytes = defaultMaxEntryBytes
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = defaultHealthInterval
	}
	if cfg.UpstreamResultsURL == "" {
		cfg.UpstreamResultsURL = defaultUpstreamResultsURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: 2 * time.Minute,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		}}
	}
	cfg.AdvertiseBase = strings.TrimRight(cfg.AdvertiseBase, "/")
	if _, err := url.Parse(cfg.AdvertiseBase); err != nil || cfg.AdvertiseBase == "" {
		return nil, fmt.Errorf("actionscache: advertise base %q is not a URL", cfg.AdvertiseBase)
	}
	reg, err := newRegistry(cfg.Registry, cfg.RegistryUser, cfg.RegistryPassword, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("actionscache: %w", err)
	}
	up, err := url.Parse(strings.TrimRight(cfg.UpstreamResultsURL, "/"))
	if err != nil || up.Scheme == "" || up.Host == "" {
		return nil, fmt.Errorf("actionscache: upstream results URL %q is not an absolute URL", cfg.UpstreamResultsURL)
	}
	p := &Proxy{
		cfg:       cfg,
		log:       cfg.Log.With("component", "actions-cache"),
		reg:       reg,
		ix:        newIndex(reg),
		upstream:  up,
		jobs:      map[string]*jobState{},
		uploads:   map[string]*pending{},
		byKey:     map[string]*pending{},
		downloads: map[string]*download{},
	}
	p.relay = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
		},
		ErrorLog: slog.NewLogLogger(p.log.Handler(), slog.LevelWarn),
	}
	return p, nil
}

// Start probes the registry once, binds the listener and starts serving.
func (p *Proxy) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.probe(ctx)

	ln, err := net.Listen("tcp", p.cfg.ListenAddr)
	if err != nil {
		cancel()
		return fmt.Errorf("actionscache: listen %s: %w", p.cfg.ListenAddr, err)
	}
	p.ln = ln
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.log.Error("actions cache server stopped", "error", err)
		}
	}()
	if p.cfg.HealthInterval > 0 {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			t := time.NewTicker(p.cfg.HealthInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					p.probe(ctx)
					p.expire(time.Now())
				}
			}
		}()
	}
	p.log.Info("actions cache ready", "addr", ln.Addr().String(), "registry", p.reg.base.String(), "healthy", p.Healthy())
	return nil
}

// Stop shuts the server down.
func (p *Proxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	var err error
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = p.srv.Shutdown(ctx)
		cancel()
	}
	p.wg.Wait()
	return err
}

// Addr is the bound address (after Start).
func (p *Proxy) Addr() string {
	if p.ln == nil {
		return p.cfg.ListenAddr
	}
	return p.ln.Addr().String()
}

// ResultsURL is the ACTIONS_RESULTS_URL value for job containers.
func (p *Proxy) ResultsURL() string { return p.cfg.AdvertiseBase + "/" }

// Healthy reports whether the registry answered the last probe. The
// scheduler only turns the cache on for a new job while this is true.
func (p *Proxy) Healthy() bool { return p.healthy.Load() }

func (p *Proxy) probe(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := p.reg.ping(pctx)
	was := p.healthy.Swap(err == nil)
	switch {
	case err != nil && was:
		p.log.Warn("actions cache registry stopped answering; new jobs use GitHub's cache", "error", err)
	case err == nil && !was:
		p.log.Info("actions cache registry is answering")
	}
}

// Track registers a runner the cache is turned on for. Until Bind it is
// unbound: every lookup misses, every save is refused.
func (p *Proxy) Track(runner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.jobs[runner]; !ok {
		p.jobs[runner] = &jobState{}
	}
}

// Bind grants a tracked runner its scope. It is a no-op for an untracked
// runner (the cache was not turned on for it) and refuses to re-bind: a
// runner runs exactly one job, so a second, different binding is a bug or an
// attack, and the safe answer is to drop the runner's access entirely.
func (p *Proxy) Bind(runner string, s Scope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.jobs[runner]
	if !ok {
		return
	}
	if st.bound {
		if !sameScope(st.scope, s) {
			p.log.Warn("conflicting second cache binding for a runner; revoking its cache access", "runner", runner, "first", st.scope.Repo+"@"+st.scope.Write, "second", s.Repo+"@"+s.Write)
			st.scope = Scope{}
		}
		return
	}
	st.bound, st.scope = true, s
	p.log.Info("cache scope bound", "runner", runner, "repo", s.Repo, "write", s.Write, "reads", strings.Join(s.Reads, ","))
}

func sameScope(a, b Scope) bool {
	return a.Repo == b.Repo && a.Write == b.Write && strings.Join(a.Reads, "\n") == strings.Join(b.Reads, "\n")
}

// Forget drops a runner and everything it was granted. Call it when the
// runner's environment is destroyed.
func (p *Proxy) Forget(runner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.jobs, runner)
	for tok, u := range p.uploads {
		if u.runner == runner {
			delete(p.uploads, tok)
			delete(p.byKey, runner+"\x00"+u.key+"\x00"+u.version)
		}
	}
	for tok, d := range p.downloads {
		if d.runner == runner {
			delete(p.downloads, tok)
		}
	}
}

// expire drops grants nobody finished with.
func (p *Proxy) expire(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tok, u := range p.uploads {
		if now.Sub(u.created) > pendingTTL {
			delete(p.uploads, tok)
			delete(p.byKey, u.runner+"\x00"+u.key+"\x00"+u.version)
		}
	}
	for tok, d := range p.downloads {
		if now.After(d.expires) {
			delete(p.downloads, tok)
		}
	}
}

// caller identifies the job a request came from: its runner name and, if
// bound, its scope.
func (p *Proxy) caller(r *http.Request) (runner string, s Scope, bound bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", Scope{}, false
	}
	runner = p.cfg.JobForIP(host)
	if runner == "" {
		return "", Scope{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.jobs[runner]
	if !ok {
		return "", Scope{}, false
	}
	return runner, st.scope, st.bound && st.scope.Repo != ""
}

func token() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// ServeHTTP routes cache calls here and relays everything else to GitHub.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, twirpPrefix):
		p.serveTwirp(w, r, strings.TrimPrefix(r.URL.Path, twirpPrefix))
	case strings.HasPrefix(r.URL.Path, blobPrefix+"u/"):
		p.serveUpload(w, r, strings.TrimPrefix(r.URL.Path, blobPrefix+"u/"))
	case strings.HasPrefix(r.URL.Path, blobPrefix+"d/"):
		p.serveDownload(w, r, strings.TrimPrefix(r.URL.Path, blobPrefix+"d/"))
	default:
		p.serveRelay(w, r)
	}
}

// serveRelay passes a non-cache request (artifacts, …) to GitHub untouched,
// Authorization included. Only containers of a job the cache is on for may
// use it — this must not become an open relay on the bridge.
func (p *Proxy) serveRelay(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	runner := p.cfg.JobForIP(host)
	p.mu.Lock()
	_, tracked := p.jobs[runner]
	p.mu.Unlock()
	if runner == "" || !tracked {
		http.Error(w, "not a job this cache serves", http.StatusForbidden)
		return
	}
	p.relay.ServeHTTP(w, r)
}

// --- twirp ---------------------------------------------------------------

// The cache client (actions/toolkit packages/cache, generated
// results/api/v1/cache.ts) serializes requests with useProtoFieldName, so
// field names arrive snake_case and int64 values as JSON strings; it parses
// responses leniently (either naming, string or number). Every field below
// was checked against that generated code; none is guessed.

type twirpReq struct {
	Key         string   `json:"key"`
	Version     string   `json:"version"`
	RestoreKeys []string `json:"restore_keys"`
	SizeBytes   jsonInt  `json:"size_bytes"`
}

// jsonInt accepts an int64 as a JSON number or string (proto3 JSON emits
// strings).
type jsonInt int64

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*j = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*j = jsonInt(n)
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// twirpError answers in twirp's error envelope. Only used for requests that
// are malformed or for methods we do not serve; a 4xx is not retried.
func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": msg})
}

func (p *Proxy) serveTwirp(w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		twirpError(w, http.StatusBadRequest, "bad_route", "twirp methods are POST")
		return
	}
	var req twirpReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}
	runner, scope, bound := p.caller(r)
	switch method {
	case "GetCacheEntryDownloadURL":
		p.getDownloadURL(w, r, runner, scope, bound, req)
	case "CreateCacheEntry":
		p.createEntry(w, r, runner, scope, bound, req)
	case "FinalizeCacheEntryUpload":
		p.finalizeEntry(w, r, runner, scope, bound, req)
	default:
		twirpError(w, http.StatusNotFound, "bad_route", "method not served by ephemerd's LAN cache: "+method)
	}
}

func (p *Proxy) getDownloadURL(w http.ResponseWriter, r *http.Request, runner string, s Scope, bound bool, req twirpReq) {
	miss := map[string]any{"ok": false, "signed_download_url": "", "matched_key": ""}
	if !bound || !p.Healthy() {
		writeJSON(w, miss)
		return
	}
	e, err := p.ix.lookup(r.Context(), s, req.Key, req.RestoreKeys, req.Version)
	if err != nil {
		p.log.Warn("cache lookup failed; answering as a miss", "runner", runner, "error", err)
	}
	if e == nil {
		writeJSON(w, miss)
		return
	}
	tok := token()
	now := time.Now()
	p.mu.Lock()
	p.downloads[tok] = &download{runner: runner, e: e, expires: now.Add(downloadTTL)}
	p.mu.Unlock()
	p.touch(e, now)
	p.log.Info("cache hit", "runner", runner, "repo", s.Repo, "key", e.Key, "ref", e.Ref, "bytes", e.Size)
	writeJSON(w, map[string]any{
		"ok":                  true,
		"signed_download_url": p.cfg.AdvertiseBase + blobPrefix + "d/" + tok,
		"matched_key":         e.Key,
	})
}

func (p *Proxy) touch(e *entry, now time.Time) {
	s := p.ix.scope(e.repository)
	s.mu.Lock()
	e.lastUsed = now
	s.mu.Unlock()
}

func (p *Proxy) createEntry(w http.ResponseWriter, r *http.Request, runner string, s Scope, bound bool, req twirpReq) {
	refuse := func(msg string) {
		writeJSON(w, map[string]any{"ok": false, "signed_upload_url": "", "message": msg})
	}
	switch {
	case !bound:
		refuse("ephemerd's LAN cache has no scope for this job yet; not saving")
		return
	case s.Write == "":
		refuse("this job may only restore from the cache, not save to it")
		return
	case !p.Healthy():
		refuse("ephemerd's LAN cache registry is not answering; not saving")
		return
	case req.Key == "" || req.Version == "":
		refuse("key and version are required")
		return
	}
	exists, err := p.ix.exists(r.Context(), s.Repo, s.Write, req.Key, req.Version)
	if err != nil {
		refuse("checking for an existing entry failed: " + err.Error())
		return
	}
	if exists {
		refuse("an entry for this key already exists; cache entries are immutable")
		return
	}
	tok := token()
	u := &pending{runner: runner, scope: s, key: req.Key, version: req.Version, created: time.Now(), blocks: map[string]descriptor{}}
	bk := runner + "\x00" + req.Key + "\x00" + req.Version
	p.mu.Lock()
	if prev, ok := p.byKey[bk]; ok {
		for t, v := range p.uploads {
			if v == prev {
				delete(p.uploads, t)
			}
		}
	}
	p.uploads[tok] = u
	p.byKey[bk] = u
	p.mu.Unlock()
	writeJSON(w, map[string]any{
		"ok":                true,
		"signed_upload_url": p.cfg.AdvertiseBase + blobPrefix + "u/" + tok,
		"message":           "",
	})
}

func (p *Proxy) finalizeEntry(w http.ResponseWriter, r *http.Request, runner string, s Scope, bound bool, req twirpReq) {
	refuse := func(msg string) {
		writeJSON(w, map[string]any{"ok": false, "entry_id": "0", "message": msg})
	}
	if !bound || s.Write == "" {
		refuse("this job may not save to the cache")
		return
	}
	bk := runner + "\x00" + req.Key + "\x00" + req.Version
	p.mu.Lock()
	u, ok := p.byKey[bk]
	if ok {
		delete(p.byKey, bk)
		for t, v := range p.uploads {
			if v == u {
				delete(p.uploads, t)
			}
		}
	}
	p.mu.Unlock()
	if !ok {
		refuse("no upload in progress for this key")
		return
	}

	u.mu.Lock()
	layers, size, err := u.committed()
	u.mu.Unlock()
	if err != nil {
		refuse(err.Error())
		return
	}
	if int64(req.SizeBytes) != size {
		refuse(fmt.Sprintf("uploaded %d bytes but the client says the archive is %d", size, req.SizeBytes))
		return
	}

	e := &entry{
		Key: u.key, Version: u.version, Ref: u.scope.Write,
		Created: time.Now().UTC(), Size: size, Layers: layers,
		repository: scopeRepository(u.scope.Repo, u.scope.Write),
		tag:        entryTag(u.key, u.version),
	}
	cfgBytes := configBlob(e)
	cfgDigest, _, err := p.reg.uploadBlob(r.Context(), e.repository, strings.NewReader(string(cfgBytes)))
	if err != nil {
		refuse("storing entry config: " + err.Error())
		return
	}
	body, err := encodeManifest(e, descriptor{MediaType: configMediaType, Digest: cfgDigest, Size: int64(len(cfgBytes))})
	if err != nil {
		refuse(err.Error())
		return
	}
	digest, err := p.reg.putManifest(r.Context(), e.repository, e.tag, body)
	if err != nil {
		refuse("storing entry manifest: " + err.Error())
		return
	}
	e.digest, e.lastUsed = digest, e.Created
	p.ix.add(e)
	p.log.Info("cache saved", "runner", runner, "repo", u.scope.Repo, "ref", e.Ref, "key", e.Key, "bytes", e.Size, "blocks", len(layers))

	if evicted, err := p.ix.evict(r.Context(), u.scope.Repo, p.cfg.MaxBytesPerRepo, sweepEvery, time.Now()); err != nil {
		p.log.Warn("cache eviction failed", "repo", u.scope.Repo, "error", err)
	} else if len(evicted) > 0 {
		var freed int64
		for _, ev := range evicted {
			freed += ev.Size
		}
		p.log.Info("cache evicted least recently used entries", "repo", u.scope.Repo, "entries", len(evicted), "bytes", freed)
	}

	writeJSON(w, map[string]any{"ok": true, "entry_id": strconv.FormatInt(e.Created.UnixNano(), 10), "message": ""})
}

// committed returns the upload's layers in block-list order. Caller holds
// u.mu.
func (u *pending) committed() ([]descriptor, int64, error) {
	if u.failed != nil {
		return nil, 0, fmt.Errorf("upload failed: %w", u.failed)
	}
	if u.order == nil {
		return nil, 0, errors.New("upload was never committed")
	}
	layers := make([]descriptor, 0, len(u.order))
	var size int64
	for _, id := range u.order {
		d, ok := u.blocks[id]
		if !ok {
			return nil, 0, fmt.Errorf("block list names block %q that was never uploaded", id)
		}
		layers = append(layers, d)
		size += d.Size
	}
	return layers, size, nil
}

// --- Azure blob upload ---------------------------------------------------

// The client uploads with @azure/storage-blob's BlockBlobClient.uploadFile
// (toolkit uploadUtils.ts uploadCacheArchiveSDK): one Put Blob for archives
// up to 128 MiB, otherwise concurrent Put Block calls and a Put Block List.
// Each block becomes its own registry blob, so out-of-order concurrent blocks
// stream straight through without being reassembled first.

func (p *Proxy) serveUpload(w http.ResponseWriter, r *http.Request, tok string) {
	if r.Method != http.MethodPut {
		http.Error(w, "uploads are PUT", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	u, ok := p.uploads[tok]
	p.mu.Unlock()
	runner, _, _ := p.caller(r)
	if !ok || runner == "" || runner != u.runner {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	switch q.Get("comp") {
	case "block":
		id := q.Get("blockid")
		if id == "" {
			http.Error(w, "blockid is required", http.StatusBadRequest)
			return
		}
		d, err := p.storeBlock(r, u)
		if err != nil {
			p.uploadFailed(w, u, err)
			return
		}
		u.mu.Lock()
		u.blocks[id] = d
		u.mu.Unlock()
	case "blocklist":
		ids, err := parseBlockList(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.order = ids
		u.mu.Unlock()
	case "":
		// Put Blob: the whole archive in one request.
		d, err := p.storeBlock(r, u)
		if err != nil {
			p.uploadFailed(w, u, err)
			return
		}
		u.mu.Lock()
		u.blocks[""] = d
		u.order = []string{""}
		u.mu.Unlock()
	default:
		http.Error(w, "unsupported comp="+q.Get("comp"), http.StatusBadRequest)
		return
	}
	azureCreated(w)
}

// storeBlock streams one request body into the registry, enforcing the
// per-entry size limit across all of the upload's blocks.
func (p *Proxy) storeBlock(r *http.Request, u *pending) (descriptor, error) {
	u.mu.Lock()
	room := p.cfg.MaxEntryBytes - u.size
	u.mu.Unlock()
	if room <= 0 {
		return descriptor{}, fmt.Errorf("entry exceeds the %d byte limit", p.cfg.MaxEntryBytes)
	}
	lr := &io.LimitedReader{R: r.Body, N: room + 1}
	digest, n, err := p.reg.uploadBlob(r.Context(), scopeRepository(u.scope.Repo, u.scope.Write), lr)
	if err != nil {
		return descriptor{}, err
	}
	if n > room {
		return descriptor{}, fmt.Errorf("entry exceeds the %d byte limit", p.cfg.MaxEntryBytes)
	}
	u.mu.Lock()
	u.size += n
	u.mu.Unlock()
	return descriptor{MediaType: chunkMediaType, Digest: digest, Size: n}, nil
}

func (p *Proxy) uploadFailed(w http.ResponseWriter, u *pending, err error) {
	u.mu.Lock()
	if u.failed == nil {
		u.failed = err
	}
	u.mu.Unlock()
	p.log.Warn("cache upload failed; the job saves nothing", "runner", u.runner, "key", u.key, "error", err)
	// 400, not 5xx: the Azure SDK retries 5xx, and the upload is already
	// doomed — Finalize will refuse it.
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// azureCreated answers a Put Blob / Put Block / Put Block List the way the
// Azure SDK expects to see success.
func azureCreated(w http.ResponseWriter) {
	h := w.Header()
	h.Set("ETag", `"0x`+strings.ToUpper(token()[:16])+`"`)
	h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	h.Set("x-ms-request-id", token()[:32])
	h.Set("x-ms-version", "2024-11-04")
	h.Set("x-ms-request-server-encrypted", "false")
	w.WriteHeader(http.StatusCreated)
}

// parseBlockList reads a Put Block List body: <BlockList> with <Latest>,
// <Committed> or <Uncommitted> children, in commit order. The ids are base64
// as sent with Put Block and are compared verbatim.
func parseBlockList(r io.Reader) ([]string, error) {
	dec := xml.NewDecoder(r)
	var ids []string
	inList := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing block list: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "BlockList":
			inList = true
		case "Latest", "Committed", "Uncommitted":
			if !inList {
				return nil, errors.New("block id outside <BlockList>")
			}
			var id string
			if err := dec.DecodeElement(&id, &se); err != nil {
				return nil, fmt.Errorf("parsing block id: %w", err)
			}
			id = strings.TrimSpace(id)
			if _, err := base64.StdEncoding.DecodeString(id); err != nil {
				return nil, fmt.Errorf("block id %q is not base64", id)
			}
			ids = append(ids, id)
		}
	}
	if !inList {
		return nil, errors.New("no <BlockList> element")
	}
	if len(ids) == 0 {
		return nil, errors.New("empty block list")
	}
	return ids, nil
}

// --- download ------------------------------------------------------------

// serveDownload streams an entry's layers back as one archive. The client
// (downloadCacheHttpClient, used for every non-Azure host) does a plain GET
// and checks the bytes against Content-Length; a single byte range is also
// honoured for robustness.
func (p *Proxy) serveDownload(w http.ResponseWriter, r *http.Request, tok string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "downloads are GET", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	d, ok := p.downloads[tok]
	p.mu.Unlock()
	runner, _, _ := p.caller(r)
	if !ok || runner == "" || runner != d.runner || time.Now().After(d.expires) {
		http.Error(w, "unknown download", http.StatusNotFound)
		return
	}
	e := d.e

	start, end := int64(0), e.Size-1
	partial := false
	if rh := r.Header.Get("Range"); rh != "" && e.Size > 0 {
		s, en, ok := parseRange(rh, e.Size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(e.Size, 10))
			http.Error(w, "unsatisfiable range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, partial = s, en, true
	}
	length := end - start + 1
	if e.Size == 0 {
		length = 0
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, e.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead || length == 0 {
		return
	}
	if err := p.streamRange(r.Context(), w, e, start, length); err != nil {
		// Headers are gone; cutting the connection short makes the client's
		// Content-Length check fail, which it treats as a failed restore
		// (a miss), not a corrupt one.
		p.log.Warn("cache download aborted", "runner", runner, "key", e.Key, "error", err)
		panic(http.ErrAbortHandler)
	}
}

func (p *Proxy) streamRange(ctx context.Context, w io.Writer, e *entry, off, n int64) error {
	for _, l := range e.Layers {
		if n <= 0 {
			return nil
		}
		if off >= l.Size {
			off -= l.Size
			continue
		}
		take := l.Size - off
		if take > n {
			take = n
		}
		rc, err := p.reg.openBlob(ctx, e.repository, l.Digest, off, take)
		if err != nil {
			return err
		}
		copied, err := io.Copy(w, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		if copied != take {
			return fmt.Errorf("blob %s: got %d bytes, want %d", l.Digest, copied, take)
		}
		n -= take
		off = 0
	}
	if n > 0 {
		return fmt.Errorf("entry shorter than its recorded size by %d bytes", n)
	}
	return nil
}

// parseRange accepts one "bytes=a-b", "bytes=a-" or "bytes=-n" range.
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false
		}
		e := size - 1
		if b != "" {
			if e, err = strconv.ParseInt(b, 10, 64); err != nil || e < s {
				return 0, 0, false
			}
			if e >= size {
				e = size - 1
			}
		}
		return s, e, true
	}
}
