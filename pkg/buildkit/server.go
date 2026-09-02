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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// /etc/resolv.conf. Set this to networking.DefaultPublicDNS (public
	// resolvers reached over NAT egress) — the SAME set job containers get.
	// Do NOT use the bridge gateway: ephemerd runs no resolver there, so
	// pointing at it fails every lookup (the mistake #180 made and this once
	// repeated).
	//
	// REQUIRED for `RUN` steps to resolve names once CNIConfigPath moves
	// builds off the host netns. BuildKit's CNI provider attaches build steps
	// to the container subnet but does NOT apply the CNI conflist's DNS to
	// resolv.conf — that is the executor's job, and with no DNS set here the
	// executor falls back to the HOST's /etc/resolv.conf (systemd-resolved on
	// loopback), which the container's isolated netns cannot reach. Every
	// `RUN` that resolves a name (apt-get, apk, pip, ...) then fails with
	// "Temporary failure resolving". Job containers get their resolv.conf by a
	// separate bind-mount (withDNSMount); this field is the equivalent for
	// BuildKit build containers. Linux only; empty preserves BuildKit's
	// default (host resolv.conf) behaviour.
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

	// mu guards core, closed and rebuildErr. Readers (Build, Client, Prune)
	// hold it only long enough to take the pointer, never for the duration
	// of a solve: a Rebuild must not have to wait out a multi-hour build
	// before it can replace a store it already knows is corrupt.
	mu   sync.RWMutex
	core *serverCore
	// closed records that Close ran. Distinguishes an intentionally shut
	// down server from one left core-less by a failed Rebuild — the two
	// need very different operator messages.
	closed bool
	// rebuildErr is why the last Rebuild could not produce a working core.
	// Kept so every subsequent build can name the ORIGINAL cause instead of
	// a generic "unavailable"; cleared by a Rebuild that succeeds.
	rebuildErr error

	// dataGen counts the times Rebuild has moved or deleted the contents of
	// DataDir. Guarded by mu.
	//
	// It exists for exactly one decision: whether a solver init that
	// buildCore ABANDONED, and which finished afterwards, may still be
	// installed. A core is only valid for the store it opened. If DataDir has
	// been renamed to quarantine or cleared and restored in the meantime, the
	// late core's bbolt files are (on Linux) unlinked or (on Windows) a
	// different directory's — installing it would give the node a solver
	// writing into a store nothing else reads. Comparing the generation the
	// init started at with the current one is a cheap, exact test for that.
	dataGen uint64

	once sync.Once

	// newCoreFn is newCore, overridable in tests. The liveness property
	// buildCore guarantees is only observable against an init that hangs,
	// and the real one can only be made to hang by corrupting a bbolt lock.
	newCoreFn func(context.Context, Config) (*serverCore, error)
	// coreTimeoutOverride shortens buildCore's bound in tests. Zero uses
	// coreInitTimeout.
	coreTimeoutOverride time.Duration
}

// ErrServerClosed is returned by build paths after Close.
var ErrServerClosed = errors.New("buildkit: server is closed")

// ErrStoreUnavailable is returned by build paths when Rebuild tore the old
// solver down and could not stand a new one up.
//
// WHY THIS EXISTS. Rebuild MUST stop the old core before it can quarantine
// the data dir (see Rebuild), so a newCore failure leaves the Server with no
// solver at all. Before this error existed, s.core kept pointing at the
// already-stopped core: every later build dialed a dead in-process gRPC
// server and failed with an opaque transport error, forever, while the node
// stayed "healthy" in every health check and kept accepting jobs — a
// build-dead node that only a daemon restart cleared. Naming the state gives
// the operator the one instruction that actually works.
var ErrStoreUnavailable = errors.New("buildkit: shared build store is unavailable after a failed metadata-store rebuild; restart ephemerd on this node to recover")

// coreController is the slice of *control.Controller that serverCore's
// teardown needs. Close is the ONLY thing that releases the bbolt files under
// DataDir; see serverCore.close.
type coreController interface {
	Close() error
}

// serverCore is everything that is discarded and rebuilt when the BuildKit
// metadata store has to be reconstructed. Grouping it means Rebuild swaps one
// pointer rather than mutating half a dozen fields under a lock.
type serverCore struct {
	// controller is the *control.Controller, held as an interface for the
	// one method teardown depends on. The concrete type can only be
	// constructed against a live containerd, and the close contract below
	// (that the controller IS closed, after the gRPC server stops) is the
	// single most important property in this file — it must be testable
	// without one.
	controller coreController
	session    *session.Manager
	workers    *worker.Controller

	// bufnet is the in-process gRPC listener the Controller serves on.
	bufnet    *bufconn.Listener
	grpcServ  *grpc.Server
	grpcErrCh chan error

	// stop is closed on shutdown to signal graceful stop to the Controller.
	stop chan struct{}

	// log is the core's own logger, needed because close() reports teardown
	// errors and runs on paths (Rebuild, daemon shutdown) that have no other
	// way to surface them. May be nil in tests.
	log *slog.Logger

	// closeOnce makes close idempotent. Rebuild closes the OLD core and
	// Server.Close closes whatever core is current; a Rebuild that failed
	// used to leave both pointing at the same core, and the second
	// `close(c.stop)` panicked the daemon on shutdown ("close of closed
	// channel"). The nil-ing in Rebuild removes that aliasing, but a
	// teardown path must never be one refactor away from a panic.
	closeOnce sync.Once

	// abandoned records that close() gave up on a gRPC server that would not
	// stop, and therefore SKIPPED controller.Close(). The core's bbolt
	// handles under DataDir are still open and always will be. Set exactly
	// once, inside closeOnce, before close() returns; read by tests and by
	// closeAbandoned().
	abandoned atomic.Bool

	// graceTimeout / hardStopTimeout override coreCloseGraceTimeout and
	// coreCloseHardStopTimeout. Zero means "use the constant". Tests set
	// them: the escalation path they need to exercise is only reachable by
	// waiting the bound out, and a 20s+5s wait per assertion is not a test.
	graceTimeout    time.Duration
	hardStopTimeout time.Duration
}

// closeAbandoned reports that this core's teardown gave up with its gRPC
// server still running, so its bbolt handles under DataDir were deliberately
// leaked. Callers that are about to touch DataDir on disk can use it to
// explain a failure instead of guessing.
func (c *serverCore) closeAbandoned() bool {
	return c != nil && c.abandoned.Load()
}

// coreCloseGraceTimeout bounds how long close() waits for in-flight RPCs to
// drain before it stops the gRPC server the hard way.
//
// grpc.Server.GracefulStop blocks until every open stream ends, and a Solve
// stream lives as long as the build it is running — potentially hours. Two
// things make waiting that long unacceptable here. Rebuild calls close()
// while holding s.mu, so an unbounded wait blocks Client/Build/Prune/Close
// for the duration of a build the Rebuild exists to unbreak; and, worse, the
// bbolt handles below cannot be released until the server has stopped, so
// the quarantine rename that Rebuild is about to attempt would be guaranteed
// to fail. Dropping an in-flight build is the correct trade: we are here
// because builds on this node are already failing.
const coreCloseGraceTimeout = 20 * time.Second

// coreCloseHardStopTimeout bounds how long close() waits AFTER grpc.Server.Stop
// before it abandons the server entirely.
//
// WHY A SECOND BOUND IS NEEDED, i.e. why Stop() is not the escape hatch it
// looks like. Both GracefulStop and Stop funnel into grpc.Server.stop, whose
// last act is `s.handlersWG.Wait()` (grpc@v1.78.0 server.go:1962) — it waits
// for every RPC HANDLER GOROUTINE to return. Stop closes the listeners and the
// transports, which cancels each stream's context, but a handler that does not
// observe its context never returns and the WaitGroup never drains. The
// in-flight `GracefulStop()` therefore stays blocked even after `Stop()`, and
// the old code's unconditional `<-stopped` after Stop() was an unbounded wait
// wearing a bound's clothing. Reproduced directly: with a handler that ignores
// its stream ctx, GracefulStop was still blocked ten seconds after Stop().
//
// This is not hypothetical for BuildKit. A Windows RUN step whose containerd
// shim has died leaves containerdexecutor.runProcess parked in
// `p.Wait(context.Background())` / its `defer io.Wait()` — neither takes the
// stream ctx — so the Solve handler never returns and this core's gRPC server
// can never be stopped by any means short of process exit.
//
// Sized as a short grace after Stop() because Stop() DOES unblock the common
// case (handlers that respect cancellation return promptly once their
// transport dies); it only fails for the wedged-handler case, and for that
// case no amount of waiting helps.
const coreCloseHardStopTimeout = 5 * time.Second

// close tears down one core. Safe to call on a core whose gRPC server has
// already stopped, and safe to call more than once.
//
// ORDER MATTERS, and it mirrors buildkitd's own shutdown
// (buildkit@v0.25.1 cmd/buildkitd/main.go: `defer controller.Close()`
// registered before `server.GracefulStop()`, so the controller is closed
// after the server has stopped serving):
//
//  1. close(c.stop) — BuildKit's GracefulStop channel. The history queue
//     watches it and closes its pubsub when no build is active. A hint, not
//     a barrier.
//  2. GracefulStop (bounded, then Stop) — no RPC may still be touching the
//     stores when we close them.
//  3. controller.Close() — THE STEP THAT RELEASES THE bbolt FILES.
//
// Step 3 was missing, and serverCore.controller was a write-only field.
// control.Controller.Close (buildkit@v0.25.1 control/control.go:141) closes,
// in order, opt.HistoryDB (<dataDir>/history.db), opt.WorkerController (each
// worker's Close → MetadataStore.Close, i.e. <dataDir>/worker/metadata_v2.db),
// opt.CacheStore (<dataDir>/cache.db) and the llbsolver. Nothing else in
// ephemerd closed any of them, so every core we ever tore down left three
// exclusively-flock'd bbolt files open under DataDir for the daemon's
// lifetime. On Windows that made Rebuild's os.Rename(DataDir, quarantine)
// fail with "Access is denied" every single time, which fed the reinit path
// described in Server.reinitAfterFailedRebuild.
//
// GUARANTEE: close() ALWAYS returns, in at most graceTimeout+hardStopTimeout.
// It is called with s.mu held (Rebuild, Server.Close) and from inside
// closeOnce, so a call that does not return takes the whole daemon with it —
// no build, no prune, no shutdown. That is the same wedge class this branch
// exists to remove, and the guarantee is worth more than step 3.
//
// WHAT WE GIVE UP TO GET IT: see the escalation branch below.
func (c *serverCore) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.stop)

		if c.grpcServ != nil && !c.stopGRPC() {
			// The gRPC server could not be stopped, so an RPC handler is
			// still live and may still be reading and writing the very
			// bbolt files controller.Close() would close underneath it.
			// Closing them here is not "leaky but safe" — it is a
			// use-after-close of a bbolt DB from a running goroutine,
			// i.e. a panic or a corrupted store. So we do not.
			c.abandoned.Store(true)
			if c.log != nil {
				c.log.Error("buildkit: a solver RPC handler will not stop; ABANDONING this core's teardown with its bbolt handles (cache.db, history.db, worker/metadata_v2.db) INTENTIONALLY LEFT OPEN",
					"grace", c.grace(), "hard_stop_grace", c.hardStop(),
					"consequence", "the daemon stays live and can keep serving jobs, but this core's handles under the buildkit data dir are held until the process exits; on Windows the next metadata-store Rebuild's quarantine rename will fail with 'Access is denied' and the node needs an ephemerd restart to build again",
					"likely_cause", "a build step whose containerd shim died — containerdexecutor's process Wait does not take the stream context, so the Solve handler never returns")
			}
			return
		}

		if c.controller != nil {
			if err := c.controller.Close(); err != nil && c.log != nil {
				// Logged, not returned: every caller is on a teardown
				// path that has no better answer than to continue. What
				// matters operationally is that the attempt happened —
				// a failure here is the signal that a handle may still
				// be held.
				c.log.Warn("buildkit: closing the solver controller (bbolt handles may still be held)", "error", err)
			}
		}
	})
}

func (c *serverCore) grace() time.Duration {
	if c.graceTimeout > 0 {
		return c.graceTimeout
	}
	return coreCloseGraceTimeout
}

func (c *serverCore) hardStop() time.Duration {
	if c.hardStopTimeout > 0 {
		return c.hardStopTimeout
	}
	return coreCloseHardStopTimeout
}

// stopGRPC drains the in-process gRPC server, escalating GracefulStop → Stop,
// and reports whether the server actually stopped. False means a handler
// goroutine is wedged and the server will never stop; the caller must NOT
// touch anything that handler can still reach.
//
// The GracefulStop goroutine is deliberately left running when we give up: it
// is parked on handlersWG.Wait() and will exit if the handler ever returns.
// Killing it is not possible and waiting for it is the bug.
func (c *serverCore) stopGRPC() bool {
	stopped := make(chan struct{})
	go func() {
		c.grpcServ.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		return true
	case <-time.After(c.grace()):
	}

	// Stop() closes the listeners and every transport, which cancels each
	// in-flight stream's context. Handlers that respect cancellation return
	// almost immediately and the GracefulStop above then completes.
	//
	// Stop() runs on its own goroutine and is NOT waited on, for two
	// independent reasons:
	//
	//   - Stop() itself can block indefinitely. grpc.Server.stop takes s.mu
	//     and the parked GracefulStop holds s.mu for the whole of its
	//     handlersWG.Wait() once the connections have drained (server.go:1938
	//     `defer s.mu.Unlock()` + :1961). If the client hung up while the
	//     handler stayed wedged — exactly the shim-death shape — Stop()
	//     deadlocks on that mutex.
	//   - Even when Stop() returns, it does not make GracefulStop return:
	//     stop(false) skips handlersWG.Wait, stop(true) does not. So `stopped`
	//     closing is the only honest evidence that the server is down.
	go c.grpcServ.Stop()

	select {
	case <-stopped:
		return true
	case <-time.After(c.hardStop()):
		return false
	}
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

	// Everything constructed below this point owns an OS resource, and three
	// of them (the worker's metadata_v2.db, cache.db, history.db) are bbolt
	// files under DataDir held with an EXCLUSIVE flock. Returning from a
	// later step without closing an earlier one leaks that lock for the
	// daemon's lifetime — and a leaked lock under DataDir is precisely what
	// makes Rebuild's quarantine rename impossible on Windows and its
	// re-open impossible everywhere. So every early return from here on
	// unwinds what it has already built, newest first.
	//
	// This is live, not theoretical: a corrupt history.db is one of the
	// conditions that sends the heal ladder to Rebuild, and before this the
	// `history db` error path returned with cache.db and metadata_v2.db
	// still open.
	var built []io.Closer
	unwind := func() {
		for i := len(built) - 1; i >= 0; i-- {
			if cerr := built[i].Close(); cerr != nil {
				cfg.Log.Warn("buildkit: closing a partially built solver core", "error", cerr)
			}
		}
	}

	workerCtrl, err := newWorkerController(ctx, cfg, sessMgr)
	if err != nil {
		return nil, fmt.Errorf("buildkit: worker controller: %w", err)
	}
	built = append(built, workerCtrl)

	defaultWorker, err := workerCtrl.GetDefault()
	if err != nil {
		unwind()
		return nil, fmt.Errorf("buildkit: default worker: %w", err)
	}

	frontends := map[string]frontend.Frontend{
		"dockerfile.v0": forwarder.NewGatewayForwarder(workerCtrl.Infos(), builder.Build),
	}

	gwfe, err := gateway.NewGatewayFrontend(workerCtrl.Infos(), nil)
	if err != nil {
		unwind()
		return nil, fmt.Errorf("buildkit: gateway frontend: %w", err)
	}
	frontends["gateway.v0"] = gwfe

	cacheStore, err := bboltcachestorage.NewStore(filepath.Join(cfg.DataDir, "cache.db"))
	if err != nil {
		unwind()
		return nil, fmt.Errorf("buildkit: cache store: %w", err)
	}
	built = append(built, cacheStore)

	historyDB, err := boltutil.Open(filepath.Join(cfg.DataDir, "history.db"), 0o600, nil)
	if err != nil {
		unwind()
		return nil, fmt.Errorf("buildkit: history db: %w", err)
	}
	built = append(built, historyDB)

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
		unwind()
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

	// From here the core owns `built`: serverCore.close() releases all of it
	// through ctrl.Close(), which closes HistoryDB, the WorkerController and
	// the CacheStore. Do NOT also close them here.
	return &serverCore{
		controller: ctrl,
		session:    sessMgr,
		workers:    workerCtrl,
		bufnet:     bufnet,
		grpcServ:   grpcServ,
		grpcErrCh:  grpcErrCh,
		stop:       stop,
		log:        cfg.Log,
	}, nil
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
//
// FAILURE-PATH CONTRACT. The old core cannot be kept as a fallback: it holds
// open bbolt handles inside DataDir, and the quarantine rename cannot happen
// (on Windows, cannot happen AT ALL) while they are open, so stopping it is
// unavoidably the first step — and grpc.Server.GracefulStop is one-way, so a
// stopped core can never be handed back out. Rebuild therefore drops s.core
// to nil up front and only ever installs a core it has just verified. On a
// newCore failure it restores the quarantined store and makes ONE attempt to
// re-init against it (the common cause is a transient containerd blip, and
// the restored store is exactly what was serving builds a moment ago); if
// that also fails, s.core stays nil and every build path fails fast with
// ErrStoreUnavailable instead of dialing a dead gRPC server forever.
func (s *Server) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrServerClosed
	}

	old := s.core
	// Nil FIRST, under the same lock that closes it: from here until a new
	// core is installed there is no usable solver, and readers must be told
	// that rather than handed a pointer to a server being torn down.
	s.core = nil
	if old != nil {
		old.close()
		if old.closeAbandoned() {
			// The old core's gRPC server would not stop, so its bbolt
			// handles under DataDir are open FOREVER (see serverCore.close).
			// Everything below — the quarantine rename, the fresh newCore —
			// is going to fail because of it, on Windows certainly. Say so
			// up front, so the log reads as one diagnosis rather than three
			// unexplained rename errors.
			s.cfg.Log.Error("buildkit: the previous solver could not be stopped; its bbolt handles under the data dir are still open, so this rebuild will very likely fail — restart ephemerd on this node",
				"data_dir", s.cfg.DataDir)
		}
	}

	quarantine, err := quarantineDir(s.cfg.DataDir, time.Now())
	if err != nil {
		err = fmt.Errorf("buildkit: choose a quarantine path for %s: %w", s.cfg.DataDir, err)
		// The store has not been touched on disk — this failed before any
		// rename was attempted — so this branch is STRICTLY SAFER than the
		// rename-failure branch below, which does attempt a re-init. It
		// used to strand the node build-dead anyway. Symmetry, so the
		// safer failure is not the one with the worse outcome.
		s.reinitAfterFailedRebuild(ctx, err)
		return err
	}
	// From here on DataDir's contents are no longer what any already-running
	// solver init believes them to be. See dataGen.
	s.dataGen++
	if err := os.Rename(s.cfg.DataDir, quarantine); err != nil {
		err = fmt.Errorf("buildkit: quarantine metadata store %s: %w", s.cfg.DataDir, err)
		// The store is untouched on disk (the rename never happened), so
		// the old core's replacement can still come straight back up.
		s.reinitAfterFailedRebuild(ctx, err)
		return err
	}
	pruneOldQuarantines(filepath.Dir(s.cfg.DataDir), quarantine, s.cfg.Log)

	core, err := s.buildCore(ctx)
	if err != nil {
		// Restoring means clearing the half-built store first, then renaming
		// the quarantine back over it. Both steps can fail, and the reason
		// they fail is worth being precise about, because it decides what the
		// operator has to do.
		s.dataGen++
		if rerr := s.restoreQuarantinedStore(quarantine); rerr != nil {
			// THE STORE IS NOW ONLY IN QUARANTINE. s.core is nil, so the
			// node is build-dead until a restart, and a restart alone will
			// NOT bring the cache back — the directory has to be moved by
			// hand. Name both paths.
			s.cfg.Log.Error("buildkit: could not restore the quarantined metadata store — it is STRANDED in quarantine and this node cannot build until an operator moves it back",
				"quarantine", quarantine, "data_dir", s.cfg.DataDir, "error", rerr, "cause", err,
				"recovery", "stop ephemerd, move the quarantine directory back onto the data dir path, then start ephemerd")
			s.rebuildErr = err
			return fmt.Errorf("buildkit: rebuild solver: %w", err)
		}
		err = fmt.Errorf("buildkit: rebuild solver: %w", err)
		s.reinitAfterFailedRebuild(ctx, err)
		return err
	}
	s.core = core
	s.rebuildErr = nil

	s.cfg.Log.Warn("buildkit: metadata store rebuilt after a dangling-snapshot failure; the build cache is cold",
		"data_dir", s.cfg.DataDir, "quarantined_to", quarantine)
	return nil
}

// coreInitTimeout bounds how long any s.mu-holding path will WAIT for a
// solver core to come up. Generous enough that a healthy containerd
// re-attach — the case these paths exist to rescue — comfortably fits.
const coreInitTimeout = 45 * time.Second

// ErrCoreInitTimeout is returned by buildCore when a solver init did not
// finish inside coreInitTimeout and was abandoned.
var ErrCoreInitTimeout = errors.New("buildkit: solver init did not complete within the bound and was abandoned")

// buildCore constructs a solver core and GUARANTEES that the caller unblocks
// within coreInitTimeout, whatever state the store is in.
//
// WHY THE BOUND IS MANDATORY, NOT TIDY. Both callers run with s.mu HELD —
// Rebuild's write lock, which Client, Build, Prune and Close all need — and
// the work is newCore, which bbolt-opens three files under DataDir. BuildKit
// opens them with a nil *bolt.Options, so the flock timeout is 0, and bbolt
// reads 0 as "retry forever, 50ms apart" rather than "fail fast"
// (bbolt@v1.4.3 bolt_windows.go:flock — both the initial `if timeout != 0`
// and the deadline check `timeout != 0 && time.Since(t) > timeout-...` skip
// the give-up path when timeout is zero; cache/metadata/metadata.go:30 passes
// nil). So if ANYTHING still holds a lock under DataDir, a straight-line call
// never returns: the daemon can no longer build, prune, or even shut down. An
// unkillable process is strictly worse than the build-dead node these paths
// were added to prevent, and it is a REGRESSION against the pre-existing
// behaviour of simply returning an error.
//
// serverCore.close() now genuinely releases those handles, so in practice the
// lock is free. This bound is the second line of defence: liveness of the
// daemon must not be load-bearing on the correctness of a teardown path.
// Losing an init attempt costs the node its solver until a restart — exactly
// the outcome we already accept when newCore returns an error. Wedging costs
// the node everything.
//
// An abandoned init is reaped, and the reaper ADOPTS a late success when it
// still can (see adoptLateCore). coreInitTimeout is an engineering guess, not
// a measured number; discarding a working solver because the guess was 10%
// short would turn a slow init into a build-dead node, which is the outcome
// this whole file exists to prevent. Adopting removes the cost of the guess
// being wrong.
func (s *Server) buildCore(ctx context.Context) (*serverCore, error) {
	build := s.newCoreFn
	if build == nil {
		build = newCore
	}
	timeout := s.coreTimeoutOverride
	if timeout <= 0 {
		timeout = coreInitTimeout
	}
	// Read under the caller's write lock; compared again in adoptLateCore.
	gen := s.dataGen

	// Buffered: the worker must never block on a send after we have stopped
	// listening, or abandoning it would park a goroutine on a channel
	// forever instead of letting it finish and be reaped.
	done := make(chan coreInitResult, 1)

	// Cancelled only when we abandon — best effort, since the ctx-aware
	// steps (the containerd dial in newWorkerController) bail out on it even
	// though the bbolt opens will not. On success the new core keeps the
	// caller's ctx exactly as it did before.
	initCtx, cancelInit := context.WithCancel(ctx)
	kept := false
	defer func() {
		if !kept {
			cancelInit()
		}
	}()

	go func() {
		core, err := build(initCtx, s.cfg)
		done <- coreInitResult{core: core, err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		kept = true
		return r.core, nil
	case <-time.After(timeout):
		go s.adoptLateCore(done, gen, timeout)
		return nil, fmt.Errorf("%w after %s (something is still holding a lock under %s)", ErrCoreInitTimeout, timeout, s.cfg.DataDir)
	}
}

// coreInitResult is what an init goroutine reports back to buildCore. Named
// (rather than declared inside buildCore) so the reaper can be a method with
// its own doc comment instead of a closure nobody reads.
type coreInitResult struct {
	core *serverCore
	err  error
}

// adoptLateCore reaps an init that buildCore gave up on.
//
// If the init eventually produced a working core AND the store it opened is
// still the store on disk AND the Server has no core, the core is INSTALLED.
// Otherwise it is closed, so a late success can never leak the bbolt handles
// that made us give up in the first place.
//
// WHY INSTALL RATHER THAN ALWAYS DISCARD. The alternative — the previous
// behaviour — is that a node whose init took coreInitTimeout+1s is build-dead
// until someone restarts ephemerd, even though a perfectly good solver was
// sitting in this goroutine's hands. The bound exists to protect the DAEMON's
// liveness (nothing may block s.mu indefinitely), and it still does: by the
// time this runs, the caller has long since returned. Nothing about the bound
// requires throwing the result away.
//
// gen is Server.dataGen as it was when the init STARTED. A mismatch means
// Rebuild has moved or cleared DataDir since, so the late core is bound to a
// store that no longer exists at that path — see Server.dataGen.
func (s *Server) adoptLateCore(done <-chan coreInitResult, gen uint64, timeout time.Duration) {
	r := <-done
	if r.err != nil || r.core == nil {
		// Nothing was built, so there is nothing to close or install. The
		// error was already reported to the caller as ErrCoreInitTimeout.
		return
	}

	s.mu.Lock()
	adopt := s.core == nil && !s.closed && s.dataGen == gen
	if adopt {
		s.core = r.core
		s.rebuildErr = nil
	}
	closed, hadCore, curGen := s.closed, !adopt && s.core != nil, s.dataGen
	s.mu.Unlock()

	if adopt {
		s.cfg.Log.Warn("buildkit: a solver init that had been abandoned as too slow finished successfully and was installed; this node can build again WITHOUT a restart",
			"data_dir", s.cfg.DataDir, "abandoned_after", timeout)
		return
	}

	// Not adoptable. Close it — this is the leak-prevention half of the
	// reaper and it must happen on every non-adopting path.
	r.core.close()
	s.cfg.Log.Warn("buildkit: a solver init that had been abandoned as too slow finished, but its core could not be installed and was discarded",
		"data_dir", s.cfg.DataDir,
		"server_closed", closed,
		"another_core_installed", hadCore,
		"data_dir_changed_under_it", curGen != gen)
}

// restoreRetryBudget / restoreRetryInterval bound the retry Rebuild makes when
// it cannot put a quarantined store back.
//
// The realistic reason for the failure is a Windows sharing violation from an
// ABANDONED newCore that is still running and still holds a handle under
// DataDir (see buildCore). That handle is released the moment the init
// finishes and adoptLateCore closes its core — usually within a second or two
// of giving up. A short retry converts "the store is stranded and the node
// needs a hand-repair" into "the rebuild failed and the node carried on",
// which is a much better outcome for a couple of seconds spent under s.mu on
// a path that has already spent coreInitTimeout there.
const (
	restoreRetryBudget   = 5 * time.Second
	restoreRetryInterval = 250 * time.Millisecond
)

// restoreQuarantinedStore moves the quarantined store back onto DataDir.
// Caller holds s.mu.
//
// The RemoveAll is not optional: newCore MkdirAll's DataDir and may get as far
// as creating an empty cache.db/history.db before the step that failed, and a
// rename onto an existing directory fails outright on Windows and fails on
// Linux the moment the target is non-empty.
//
// It is also not obviously safe, which is why it is documented here rather
// than asserted in a comment as it used to be ("everything at this path was
// created by the newCore call that just failed"). That claim is false when the
// failure was ErrCoreInitTimeout: the abandoned init is STILL RUNNING and may
// still be creating files under DataDir. RemoveAll can therefore race it and
// the rename can lose to its open handles. Retrying is what makes that
// recoverable; the caller logs loudly if it is not.
func (s *Server) restoreQuarantinedStore(quarantine string) error {
	deadline := time.Now().Add(restoreRetryBudget)
	for attempt := 1; ; attempt++ {
		err := os.RemoveAll(s.cfg.DataDir)
		if err != nil {
			err = fmt.Errorf("clear the half-built store at %s: %w", s.cfg.DataDir, err)
		} else if rerr := os.Rename(quarantine, s.cfg.DataDir); rerr != nil {
			err = fmt.Errorf("rename %s back to %s: %w", quarantine, s.cfg.DataDir, rerr)
		} else {
			if attempt > 1 {
				s.cfg.Log.Warn("buildkit: restored the quarantined metadata store on a retry (something under the data dir was still open)",
					"data_dir", s.cfg.DataDir, "attempts", attempt)
			}
			return nil
		}
		if !time.Now().Add(restoreRetryInterval).Before(deadline) {
			return err
		}
		time.Sleep(restoreRetryInterval)
	}
}

// reinitAfterFailedRebuild makes one attempt to stand a core back up against
// the store now on disk, so a Rebuild that failed for a transient reason
// (containerd restarting under us is the observed one) does not cost the node
// its solver until the next daemon restart. Caller holds s.mu.
//
// cause is the error Rebuild is already returning; it is recorded either way
// so ErrStoreUnavailable can name the original failure rather than the
// second-order one. On success the node is degraded (the cache was NOT
// rebuilt, so the corruption that triggered this is still there and the heal
// ladder will fire again) but it is not build-dead, which is the difference
// that matters.
//
// GUARANTEE: always returns, in at most coreInitTimeout. See buildCore.
func (s *Server) reinitAfterFailedRebuild(ctx context.Context, cause error) {
	core, err := s.buildCore(ctx)
	if err != nil {
		s.rebuildErr = cause
		s.cfg.Log.Error("buildkit: rebuild failed and the solver could not be restarted against the restored store; builds on this node will fail until ephemerd restarts",
			"data_dir", s.cfg.DataDir, "cause", cause, "reinit_error", err)
		return
	}
	s.core = core
	s.rebuildErr = nil
	s.cfg.Log.Warn("buildkit: rebuild failed; the previous metadata store was restored and the solver restarted against it — the store is still suspect",
		"data_dir", s.cfg.DataDir, "cause", cause)
}

// coreUnavailableErr maps the Server's core state to the error a build path
// must return when there is no core to dial. Pure, so the state machine that
// used to be "silently return a stopped core" is table-testable.
//
// hasCore true means callers should proceed; it returns nil only in that
// case.
func coreUnavailableErr(hasCore, closed bool, rebuildErr error) error {
	if hasCore {
		return nil
	}
	if closed {
		return ErrServerClosed
	}
	if rebuildErr != nil {
		return fmt.Errorf("%w (last rebuild error: %v)", ErrStoreUnavailable, rebuildErr)
	}
	return ErrStoreUnavailable
}

// currentOrErr returns the live core, or the reason there isn't one. Every
// build path goes through this rather than nil-checking current(), so
// "no solver" always reaches the operator as a diagnosis instead of as a
// dial failure against a stopped server.
func (s *Server) currentOrErr() (*serverCore, error) {
	s.mu.RLock()
	core, closed, rerr := s.core, s.closed, s.rebuildErr
	s.mu.RUnlock()
	if err := coreUnavailableErr(core != nil, closed, rerr); err != nil {
		return nil, err
	}
	return core, nil
}

// Client returns a buildkit client.Client connected to the in-process
// Controller via bufconn. The returned Client is not safe for concurrent
// use across different callers — construct one per request/goroutine and
// Close it when done.
func (s *Server) Client(ctx context.Context) (*client.Client, error) {
	core, err := s.currentOrErr()
	if err != nil {
		return nil, err
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return core.bufnet.DialContext(ctx)
	}
	return client.New(ctx, "ephemerd-buildkit",
		client.WithContextDialer(dialer),
		client.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
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
// gRPC server, and releases worker resources. Safe to call multiple times —
// belt (s.once) and braces (serverCore.closeOnce), because a double
// `close(core.stop)` is a daemon panic on the shutdown path, where there is
// nothing left to catch it.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.core.close()
		s.core = nil
		s.closed = true
	})
	return nil
}

// quarantineDirPrefix names the directories Rebuild moves a corrupt BuildKit
// metadata store to. Kept adjacent to the store, not inside it, so the fresh
// store starts empty.
const quarantineDirPrefix = "_quarantine-buildkit-"

// quarantineDirCollisionLimit bounds the suffix search. Reaching it means the
// clock is not advancing AND a hundred quarantines already exist, which is a
// broken host, not a naming problem.
const quarantineDirCollisionLimit = 100

// quarantineDir picks the path to move a corrupt store to. The name is
// derived from the caller-supplied clock, so it is testable; it also consults
// the filesystem to guarantee the path does not already exist.
//
// WHY IT IS NOT JUST A UNIX TIMESTAMP. It was, and one-second granularity is
// not enough. Two heal keys can both escalate to Rebuild at once; s.mu
// serializes them but does nothing to spread them out in time, so both land
// in the same second and get the same name. os.Rename onto the existing
// directory then fails (always on Windows; on Linux as soon as the first
// quarantine is non-empty, which it always is), and a Rebuild that had
// nothing wrong with it is pushed into reinitAfterFailedRebuild. Nanoseconds
// make that collision essentially impossible; the suffix loop makes it
// actually impossible, including across a clock that jumps backwards.
func quarantineDir(dataDir string, now time.Time) (string, error) {
	base := filepath.Base(dataDir)
	parent := filepath.Dir(dataDir)
	if base == "." || base == string(filepath.Separator) || parent == dataDir {
		return "", fmt.Errorf("buildkit: refusing to quarantine implausible data dir %q", dataDir)
	}
	stamp := now.UTC()
	name := fmt.Sprintf("%s%d-%09d", quarantineDirPrefix, stamp.Unix(), stamp.Nanosecond())
	for i := 0; i < quarantineDirCollisionLimit; i++ {
		candidate := filepath.Join(parent, name)
		if i > 0 {
			candidate = filepath.Join(parent, fmt.Sprintf("%s-%d", name, i))
		}
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("buildkit: no free quarantine path beside %s after %d attempts", dataDir, quarantineDirCollisionLimit)
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
