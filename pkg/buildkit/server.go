// Package buildkit wires an in-process BuildKit solver into ephemerd.
//
// The server type holds a *control.Controller configured with:
//   - a containerd worker pointed at ephemerd's embedded containerd
//   - the Dockerfile frontend plus the gateway.v0 frontend
//   - bbolt-backed cache and history stores under <dataDir>/buildkit
//
// Callers interact with the server through the Build method, which accepts a
// high-level BuildOpts describing a Docker-style build request and returns a
// progress stream. The Docker-API translation layer lives in pkg/dind.
package buildkit

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ephpm/ephemerd/pkg/networking"
	"github.com/moby/buildkit/cache/remotecache"
	inlineremotecache "github.com/moby/buildkit/cache/remotecache/inline"
	registryremotecache "github.com/moby/buildkit/cache/remotecache/registry"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/moby/buildkit/control"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/dockerfile/builder"
	"github.com/moby/buildkit/frontend/gateway"
	"github.com/moby/buildkit/frontend/gateway/forwarder"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/util/db/boltutil"
	"github.com/moby/buildkit/util/resolver"
	"github.com/moby/buildkit/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Config configures an embedded BuildKit Server.
type Config struct {
	// DataDir is where BuildKit stores its cache, history, and content.
	// Typically <ephemerd data dir>/buildkit.
	DataDir string

	// ContainerdAddress is the address of ephemerd's embedded containerd
	// gRPC endpoint. On Linux this is a unix socket path; on Windows it's
	// a named pipe (e.g. "npipe:////./pipe/ephemerd-containerd").
	ContainerdAddress string

	// ContainerdNamespace is the containerd namespace buildkit should use
	// for image and content storage. Defaults to "buildkit" if empty.
	ContainerdNamespace string

	// Snapshotter selects the containerd snapshotter. Defaults to "overlayfs"
	// on Linux and "windows" on Windows.
	Snapshotter string

	// Network manages container networking. Required on Windows so build
	// containers get an HCN NAT endpoint (otherwise RUN steps that hit the
	// internet exit immediately). Ignored on platforms where buildkit's
	// default network providers already work.
	Network *networking.Manager

	// CNIConfigPath points the Linux BuildKit worker at ephemerd's CNI
	// conflist so build `RUN` steps attach to the per-job CNI bridge — and
	// are therefore subject to the egress firewall (which jumps on the
	// container subnet) — instead of BuildKit's default fallback to the HOST
	// network namespace, which bypasses the firewall entirely. When set, the
	// worker uses network mode "cni" and fails init if the config is missing,
	// rather than silently degrading to host networking. Empty preserves
	// BuildKit's default ("auto") provider selection. Linux only; ignored on
	// Windows (which uses the HCN NAT path via Network) and unused on macOS.
	CNIConfigPath string

	// CNIBinDir is the directory holding the CNI plugin binaries (bridge,
	// host-local, portmap) the worker invokes for build-step networking.
	// Paired with CNIConfigPath. Linux only.
	CNIBinDir string

	// DNSNameservers is the resolver(s) written into each build container's
	// /etc/resolv.conf. Set this to the bridge gateway
	// (networking.Manager.GatewayIP(), e.g. "10.88.0.1"), where ephemerd's
	// resolver listens.
	//
	// REQUIRED for `RUN` steps to resolve names once CNIConfigPath moves
	// builds off the host netns. BuildKit's CNI provider attaches build steps
	// to the container subnet but does NOT apply the CNI conflist's DNS to
	// resolv.conf — that is the executor's job, and with no DNS set here the
	// executor falls back to the HOST's /etc/resolv.conf, which points at a
	// resolver the container's isolated netns cannot reach. Every `RUN` that
	// resolves a name (apt-get, apk, pip, ...) then fails with "Temporary
	// failure resolving". The CNI-conflist DNS added in #180 covers ephemerd's
	// OWN job containers (which get resolv.conf by a separate bind-mount), not
	// BuildKit build containers — this field is what covers those. Linux only;
	// empty preserves BuildKit's default (host resolv.conf) behaviour.
	DNSNameservers []string

	// GC bounds the on-disk build cache. The zero value produces NO prune
	// rules, which is how BuildKit reads "never garbage-collect" — see
	// GCConfig for what that cost us in production. Callers should pass a
	// configured policy.
	GC GCConfig

	// Log receives structured logging from the buildkit server.
	Log *slog.Logger
}

// Server hosts an in-process BuildKit Controller and the supporting objects
// (session manager, worker controller, caches) it needs. Callers interact
// with the controller through a buildkit client.Client obtained from the
// Client() method; the client dials an in-process bufconn listener that the
// Controller serves on, so no network socket is exposed.
type Server struct {
	cfg Config

	// healer remembers which dangling snapshots have already been repaired
	// and how hard, so a recurrence escalates instead of looping. See
	// heal.go.
	healer Healer

	// mu guards core. Readers (Build, Client, Prune) hold it only long
	// enough to take the pointer, never for the duration of a solve: a
	// Rebuild must not have to wait out a multi-hour build before it can
	// replace a store it already knows is corrupt.
	mu   sync.RWMutex
	core *serverCore

	once sync.Once
}

// serverCore is everything that is discarded and rebuilt when the BuildKit
// metadata store has to be reconstructed. Grouping it means Rebuild swaps one
// pointer rather than mutating half a dozen fields under a lock.
type serverCore struct {
	controller *control.Controller
	session    *session.Manager
	workers    *worker.Controller

	// bufnet is the in-process gRPC listener the Controller serves on.
	bufnet    *bufconn.Listener
	grpcServ  *grpc.Server
	grpcErrCh chan error

	// stop is closed on shutdown to signal graceful stop to the Controller.
	stop chan struct{}
}

// close tears down one core. Safe to call on a core whose gRPC server has
// already stopped.
func (c *serverCore) close() {
	if c == nil {
		return
	}
	close(c.stop)
	c.grpcServ.GracefulStop()
}

// NewServer constructs and initializes an embedded BuildKit server. The
// returned Server is ready to accept Build calls but does not expose a
// network listener; it is used in-process only.
func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("buildkit: DataDir is required")
	}
	if cfg.ContainerdAddress == "" {
		return nil, fmt.Errorf("buildkit: ContainerdAddress is required")
	}
	if cfg.ContainerdNamespace == "" {
		cfg.ContainerdNamespace = "buildkit"
	}
	if cfg.Snapshotter == "" {
		cfg.Snapshotter = defaultSnapshotter()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	core, err := newCore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, core: core}, nil
}

// newCore constructs the solver and everything under it against whatever is
// currently on disk at cfg.DataDir. Split out of NewServer so Rebuild can run
// it a second time after quarantining a corrupt metadata store.
func newCore(ctx context.Context, cfg Config) (*serverCore, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("buildkit: create data dir: %w", err)
	}

	if policy := cfg.GC.PruneInfo(); len(policy) == 0 {
		cfg.Log.Warn("buildkit: no GC policy configured — the build cache will grow without bound")
	} else {
		cfg.Log.Info("buildkit: cache GC policy active",
			"rules", len(policy),
			"reserved_bytes", cfg.GC.ReservedBytes,
			"max_used_bytes", cfg.GC.MaxUsedBytes,
			"min_free_bytes", cfg.GC.MinFreeBytes,
			"keep_duration", cfg.GC.KeepDuration)
	}

	sessMgr, err := session.NewManager()
	if err != nil {
		return nil, fmt.Errorf("buildkit: session manager: %w", err)
	}

	workerCtrl, err := newWorkerController(ctx, cfg, sessMgr)
	if err != nil {
		return nil, fmt.Errorf("buildkit: worker controller: %w", err)
	}

	defaultWorker, err := workerCtrl.GetDefault()
	if err != nil {
		return nil, fmt.Errorf("buildkit: default worker: %w", err)
	}

	frontends := map[string]frontend.Frontend{
		"dockerfile.v0": forwarder.NewGatewayForwarder(workerCtrl.Infos(), builder.Build),
	}

	gwfe, err := gateway.NewGatewayFrontend(workerCtrl.Infos(), nil)
	if err != nil {
		return nil, fmt.Errorf("buildkit: gateway frontend: %w", err)
	}
	frontends["gateway.v0"] = gwfe

	cacheStore, err := bboltcachestorage.NewStore(filepath.Join(cfg.DataDir, "cache.db"))
	if err != nil {
		return nil, fmt.Errorf("buildkit: cache store: %w", err)
	}

	historyDB, err := boltutil.Open(filepath.Join(cfg.DataDir, "history.db"), 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("buildkit: history db: %w", err)
	}

	// Registry resolver for cache import/export. Empty registries config
	// falls back to default behavior (anonymous pulls, docker config auth).
	resolverFn := resolver.NewRegistryConfig(nil)

	cacheExporters := map[string]remotecache.ResolveCacheExporterFunc{
		"registry": registryremotecache.ResolveCacheExporterFunc(sessMgr, resolverFn),
		"inline":   inlineremotecache.ResolveCacheExporterFunc(),
	}
	cacheImporters := map[string]remotecache.ResolveCacheImporterFunc{
		"registry": registryremotecache.ResolveCacheImporterFunc(sessMgr, defaultWorker.ContentStore(), resolverFn),
	}

	stop := make(chan struct{})

	ctrl, err := control.NewController(control.Opt{
		SessionManager:            sessMgr,
		WorkerController:          workerCtrl,
		Frontends:                 frontends,
		CacheManager:              solver.NewCacheManager(ctx, "local", cacheStore, worker.NewCacheResultStorage(workerCtrl)),
		ResolveCacheExporterFuncs: cacheExporters,
		ResolveCacheImporterFuncs: cacheImporters,
		// Entitlements left empty → security.insecure and network.host
		// are disabled, matching the arch doc's trust-boundary defaults.
		Entitlements: nil,
		HistoryDB:    historyDB,
		CacheStore:   cacheStore,
		LeaseManager: defaultWorker.LeaseManager(),
		ContentStore: defaultWorker.ContentStore(),
		// HistoryConfig nil → no history retention beyond Controller defaults
		HistoryConfig:  &config.HistoryConfig{},
		GarbageCollect: defaultWorker.GarbageCollect,
		GracefulStop:   stop,
	})
	if err != nil {
		return nil, fmt.Errorf("buildkit: controller: %w", err)
	}

	// Serve the Controller over an in-process bufconn listener so
	// client.Client can talk to it without a real socket.
	const bufSize = 1 << 20
	bufnet := bufconn.Listen(bufSize)
	grpcServ := grpc.NewServer()
	ctrl.Register(grpcServ)

	grpcErrCh := make(chan error, 1)
	go func() {
		if err := grpcServ.Serve(bufnet); err != nil {
			grpcErrCh <- err
		}
		close(grpcErrCh)
	}()

	return &serverCore{
		controller: ctrl,
		session:    sessMgr,
		workers:    workerCtrl,
		bufnet:     bufnet,
		grpcServ:   grpcServ,
		grpcErrCh:  grpcErrCh,
		stop:       stop,
	}, nil
}

// current returns the live core. Callers take the pointer under the read lock
// and then use it unlocked, so a concurrent Rebuild is never blocked by an
// in-flight solve.
func (s *Server) current() *serverCore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.core
}

// DefaultSnapshotter is the containerd snapshotter this platform's BuildKit
// worker uses when Config.Snapshotter is empty. Exported because the node's
// image ↔ snapshot repair pass needs the same name to build the
// `containerd.io/gc.ref.snapshot.<snapshotter>` label key, and it must agree
// with the solver even on nodes where BuildKit never started.
func DefaultSnapshotter() string { return defaultSnapshotter() }

// Healer exposes the per-daemon repair-escalation state. See heal.go.
func (s *Server) Healer() *Healer { return &s.healer }

// Snapshotter reports the containerd snapshotter the solver's worker uses.
// Callers repairing the image ↔ snapshot relationship need it to build the
// `containerd.io/gc.ref.snapshot.<snapshotter>` label key.
func (s *Server) Snapshotter() string { return s.cfg.Snapshotter }

// DataDir reports where BuildKit's cache metadata lives.
func (s *Server) DataDir() string { return s.cfg.DataDir }

// PruneAll drops every build-cache record not currently in use — the
// equivalent of `docker builder prune -af`, but executed against the SHARED
// host-side store rather than the namespaced view a job can reach.
//
// This is the cheap rung of the repair ladder: a stale record naming a
// snapshot containerd no longer has is unreferenced by definition (the build
// that would have referenced it just failed), so a full prune clears it.
func (s *Server) PruneAll(ctx context.Context) (int64, error) {
	return s.Prune(ctx, client.PruneInfo{All: true})
}

// Rebuild discards BuildKit's cache metadata store and reconstructs the
// solver against the live containerd.
//
// WHEN THIS IS THE RIGHT ANSWER. BuildKit's bbolt store is a DERIVED view of
// containerd: every record in it describes a snapshot and content that
// containerd owns. When the two disagree and a prune cannot reconcile them,
// the derived view is the one that is wrong, and there is no supported API to
// delete one bad record from it. Throwing the whole store away costs cache
// warmth — the next few builds re-pull and re-run their layers — and costs
// nothing else, because containerd still holds every blob and snapshot that is
// genuinely live. That is a far better trade than the status quo, which is
// every build on the node failing until someone logs in and does this by hand.
//
// The old store is MOVED, not deleted, so the corruption can still be
// examined; one previous quarantine is kept and older ones are removed, so a
// node that keeps tripping this cannot fill its disk with evidence.
//
// In-flight solves against the old controller fail when its gRPC server stops.
// They were failing anyway — that is why we are here.
func (s *Server) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.core
	if old != nil {
		old.close()
	}

	quarantine, err := quarantineDir(s.cfg.DataDir, time.Now())
	if err != nil {
		return err
	}
	if err := os.Rename(s.cfg.DataDir, quarantine); err != nil {
		return fmt.Errorf("buildkit: quarantine metadata store %s: %w", s.cfg.DataDir, err)
	}
	pruneOldQuarantines(filepath.Dir(s.cfg.DataDir), quarantine, s.cfg.Log)

	core, err := newCore(ctx, s.cfg)
	if err != nil {
		// Put the old store back: a broken cache is still better than no
		// solver at all, and the next build will retry the repair.
		if rerr := os.Rename(quarantine, s.cfg.DataDir); rerr != nil {
			s.cfg.Log.Error("buildkit: could not restore the quarantined metadata store",
				"quarantine", quarantine, "data_dir", s.cfg.DataDir, "error", rerr)
		}
		return fmt.Errorf("buildkit: rebuild solver: %w", err)
	}
	s.core = core

	s.cfg.Log.Warn("buildkit: metadata store rebuilt after a dangling-snapshot failure; the build cache is cold",
		"data_dir", s.cfg.DataDir, "quarantined_to", quarantine)
	return nil
}

// Client returns a buildkit client.Client connected to the in-process
// Controller via bufconn. The returned Client is not safe for concurrent
// use across different callers — construct one per request/goroutine and
// Close it when done.
func (s *Server) Client(ctx context.Context) (*client.Client, error) {
	core := s.current()
	if core == nil {
		return nil, fmt.Errorf("buildkit: server is closed")
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return core.bufnet.DialContext(ctx)
	}
	return client.New(ctx, "ephemerd-buildkit",
		client.WithContextDialer(dialer),
		client.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
}

// SessionManager exposes the session manager so callers (pkg/dind) can
// hijack incoming POST /session HTTP streams into session gRPC.
func (s *Server) SessionManager() *session.Manager {
	if core := s.current(); core != nil {
		return core.session
	}
	return nil
}

// ContainerdNamespace reports the containerd namespace build results and
// cache records land in. Exposed so per-job teardown (pkg/dind) can find
// and remove that job's build artifacts without hard-coding "buildkit".
func (s *Server) ContainerdNamespace() string {
	return s.cfg.ContainerdNamespace
}

// Prune runs BuildKit's cache prune through its own cache manager and
// returns the number of bytes released.
//
// It must go through BuildKit rather than deleting containerd records
// directly: BuildKit's bbolt cache DB keeps its own references to the
// snapshots backing each cache record, so containerd-level deletion leaves
// the snapshots pinned and reclaims nothing. (Confirmed the hard way on a
// production node — image records and leases were gone and the space did
// not come back until the snapshots were removed too.)
//
// rule is a single BuildKit prune rule. The zero rule prunes everything not
// currently in use (a full `docker builder prune`); passing
// GCConfig.PruneInfo()'s bounding rule performs the same bounded collection
// the worker does automatically, on demand.
//
// BuildKit's Prune RPC takes one rule per call, so callers wanting a
// multi-rule policy applied should call this once per rule.
func (s *Server) Prune(ctx context.Context, rule client.PruneInfo) (int64, error) {
	c, err := s.Client(ctx)
	if err != nil {
		return 0, fmt.Errorf("buildkit: dial in-process controller: %w", err)
	}
	defer func() {
		if cerr := c.Close(); cerr != nil {
			s.cfg.Log.Warn("closing buildkit client", "error", cerr)
		}
	}()

	ch := make(chan client.UsageInfo, 32)
	var released int64
	done := make(chan struct{})
	go func() {
		for ui := range ch {
			released += ui.Size
		}
		close(done)
	}()

	opts := []client.PruneOption{
		client.WithKeepOpt(rule.KeepDuration, rule.ReservedSpace, rule.MaxUsedSpace, rule.MinFreeSpace),
		pruneFilter(rule.Filter),
	}
	if rule.All {
		opts = append(opts, client.PruneAll)
	}

	err = c.Prune(ctx, ch, opts...)
	close(ch)
	<-done
	if err != nil {
		return released, fmt.Errorf("buildkit: prune: %w", err)
	}
	return released, nil
}

// pruneFilter sets the record-type filter on a prune request. The buildkit
// client package has no exported option for it (only WithKeepOpt and
// PruneAll), so we supply our own implementation of its PruneOption
// interface.
func pruneFilter(filter []string) client.PruneOption {
	return pruneOptionFunc(func(pi *client.PruneInfo) { pi.Filter = filter })
}

type pruneOptionFunc func(*client.PruneInfo)

func (f pruneOptionFunc) SetPruneOption(pi *client.PruneInfo) { f(pi) }

// Close signals the Controller to shut down gracefully, stops the in-process
// gRPC server, and releases worker resources. Safe to call multiple times.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.core.close()
		s.core = nil
	})
	return nil
}

// quarantineDirPrefix names the directories Rebuild moves a corrupt BuildKit
// metadata store to. Kept adjacent to the store, not inside it, so the fresh
// store starts empty.
const quarantineDirPrefix = "_quarantine-buildkit-"

// quarantineDir picks the path to move a corrupt store to. Pure apart from
// the caller-supplied clock, so the naming is testable.
func quarantineDir(dataDir string, now time.Time) (string, error) {
	base := filepath.Base(dataDir)
	parent := filepath.Dir(dataDir)
	if base == "." || base == string(filepath.Separator) || parent == dataDir {
		return "", fmt.Errorf("buildkit: refusing to quarantine implausible data dir %q", dataDir)
	}
	return filepath.Join(parent, fmt.Sprintf("%s%d", quarantineDirPrefix, now.UTC().Unix())), nil
}

// pruneOldQuarantines removes every quarantine directory in parent except
// keep. A node that trips this repeatedly must not accumulate copies of a
// broken store — the disk pressure that follows would be a worse bug than the
// one being repaired.
func pruneOldQuarantines(parent, keep string, log *slog.Logger) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), quarantineDirPrefix) {
			continue
		}
		path := filepath.Join(parent, e.Name())
		if path == keep {
			continue
		}
		if err := os.RemoveAll(path); err != nil && log != nil {
			log.Warn("buildkit: removing a previous quarantined store failed", "path", path, "error", err)
		}
	}
}

// Build performs a Docker-style build using the embedded BuildKit solver.
// Progress events are written to statusCh as they arrive; statusCh is
// closed by the underlying solve when the build terminates. Build itself
// blocks until the solve completes and returns the solve response.
//
// The caller constructs SolveOpt from Docker build options (this is the
// translation layer pkg/dind owns). def is nil when using a frontend
// like dockerfile.v0 — the frontend loads the definition from the build
// context supplied via SolveOpt.LocalMounts.
func (s *Server) Build(ctx context.Context, opt client.SolveOpt, statusCh chan *client.SolveStatus) (*client.SolveResponse, error) {
	c, err := s.Client(ctx)
	if err != nil {
		return nil, fmt.Errorf("buildkit: dial in-process controller: %w", err)
	}
	defer func() {
		if cerr := c.Close(); cerr != nil {
			s.cfg.Log.Warn("closing buildkit client", "error", cerr)
		}
	}()

	// def=nil — frontend loads LLB from the build context. Callers that
	// drive LLB directly (for buildx-style clients) would pass def here.
	return c.Solve(ctx, nil, opt, statusCh)
}
