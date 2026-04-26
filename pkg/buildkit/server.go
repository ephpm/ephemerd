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
	"sync"

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

	// Log receives structured logging from the buildkit server.
	Log *slog.Logger
}

// Server hosts an in-process BuildKit Controller and the supporting objects
// (session manager, worker controller, caches) it needs. Callers interact
// with the controller through a buildkit client.Client obtained from the
// Client() method; the client dials an in-process bufconn listener that the
// Controller serves on, so no network socket is exposed.
type Server struct {
	cfg        Config
	controller *control.Controller
	session    *session.Manager
	workers    *worker.Controller

	// bufnet is the in-process gRPC listener the Controller serves on.
	bufnet    *bufconn.Listener
	grpcServ  *grpc.Server
	grpcErrCh chan error

	// stop is closed by Close to signal graceful shutdown to the Controller.
	stop chan struct{}
	once sync.Once
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

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("buildkit: create data dir: %w", err)
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
		HistoryConfig: &config.HistoryConfig{},
		GarbageCollect: defaultWorker.GarbageCollect,
		GracefulStop:  stop,
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

	return &Server{
		cfg:        cfg,
		controller: ctrl,
		session:    sessMgr,
		workers:    workerCtrl,
		bufnet:     bufnet,
		grpcServ:   grpcServ,
		grpcErrCh:  grpcErrCh,
		stop:       stop,
	}, nil
}

// Client returns a buildkit client.Client connected to the in-process
// Controller via bufconn. The returned Client is not safe for concurrent
// use across different callers — construct one per request/goroutine and
// Close it when done.
func (s *Server) Client(ctx context.Context) (*client.Client, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return s.bufnet.DialContext(ctx)
	}
	return client.New(ctx, "ephemerd-buildkit",
		client.WithContextDialer(dialer),
		client.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
}

// SessionManager exposes the session manager so callers (pkg/dind) can
// hijack incoming POST /session HTTP streams into session gRPC.
func (s *Server) SessionManager() *session.Manager {
	return s.session
}

// Close signals the Controller to shut down gracefully, stops the in-process
// gRPC server, and releases worker resources. Safe to call multiple times.
func (s *Server) Close() error {
	s.once.Do(func() {
		close(s.stop)
		s.grpcServ.GracefulStop()
	})
	return nil
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
