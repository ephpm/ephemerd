package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/metrics"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// samplerEntry binds a container's sampler to the labels it should be
// reported under. Entries are accumulated by the dispatch server as
// containers come and go and walked on every StreamContainerStats tick.
type samplerEntry struct {
	id      string
	repo    string
	sampler metrics.Sampler
}

// DispatchServer implements the Dispatch gRPC service.
// It runs inside the WSL containerd-only worker and proxies
// Create/Wait/Destroy calls to the local Linux Runtime. It also serves
// StreamContainerStats so the host can scrape per-container resource
// metrics on its own /metrics endpoint without exposing a second listener
// inside the VM. See docs/arch/container-metrics.md.
type DispatchServer struct {
	apiv1.UnimplementedDispatchServer
	rt              *runtime.Runtime
	log             *slog.Logger
	mu              sync.Mutex
	envs            map[string]*runtime.RunnerEnv
	samplers        map[string]samplerEntry
	defaultInterval time.Duration
	// linuxCPULimit / linuxMemLimitBytes are the configured per-container
	// resource caps used to populate the static "limit" fields on each
	// sample (the kernel reports the same value, but we surface caller
	// intent rather than depending on the cgroup-side reporting being
	// non-zero). Set via NewDispatchServer; zero means unlimited.
	linuxCPULimit      uint64
	linuxMemLimitBytes uint64
}

// NewDispatchServer constructs a Dispatch service handler. defaultInterval
// is used when the StreamContainerStats client passes interval_seconds=0;
// linuxCPULimit / linuxMemLimitBytes are baked into each per-container
// sampler as the configured cap.
func NewDispatchServer(rt *runtime.Runtime, log *slog.Logger, defaultInterval time.Duration, linuxCPULimit, linuxMemLimitBytes uint64) *DispatchServer {
	if defaultInterval <= 0 {
		defaultInterval = 10 * time.Second
	}
	return &DispatchServer{
		rt:                 rt,
		log:                log,
		envs:               make(map[string]*runtime.RunnerEnv),
		samplers:           make(map[string]samplerEntry),
		defaultInterval:    defaultInterval,
		linuxCPULimit:      linuxCPULimit,
		linuxMemLimitBytes: linuxMemLimitBytes,
	}
}

// RegisterSampler is called by the in-VM runtime's OnTaskStarted hook
// to expose a container's sampler to StreamContainerStats subscribers.
func (s *DispatchServer) RegisterSampler(id, repo string, sampler metrics.Sampler) {
	if sampler == nil {
		return
	}
	s.mu.Lock()
	s.samplers[id] = samplerEntry{id: id, repo: repo, sampler: sampler}
	count := len(s.samplers)
	s.mu.Unlock()
	s.log.Info("dispatch: sampler registered", "id", id, "repo", repo, "total_active", count)
}

// UnregisterSampler removes a container's sampler from the stream set,
// called by the runtime's OnTaskDestroy hook.
func (s *DispatchServer) UnregisterSampler(id string) {
	s.mu.Lock()
	delete(s.samplers, id)
	count := len(s.samplers)
	s.mu.Unlock()
	s.log.Info("dispatch: sampler unregistered", "id", id, "total_active", count)
}

func (s *DispatchServer) CreateJob(ctx context.Context, req *apiv1.CreateJobRequest) (*apiv1.CreateJobResponse, error) {
	s.log.Info("dispatch: creating job", "id", req.Id, "image", req.Image)

	env, err := s.rt.Create(ctx, runtime.CreateConfig{
		ID:        req.Id,
		Image:     req.Image,
		JITConfig: req.JitConfig,
		Provider:  req.Provider,
		Repo:      req.Repo,
	})
	if err != nil {
		s.log.Error("dispatch: create failed", "id", req.Id, "error", err)
		return nil, status.Errorf(codes.Internal, "creating container: %v", err)
	}

	s.mu.Lock()
	s.envs[req.Id] = env
	s.mu.Unlock()

	s.log.Info("dispatch: job created", "id", req.Id)
	return &apiv1.CreateJobResponse{}, nil
}

func (s *DispatchServer) WaitJob(ctx context.Context, req *apiv1.WaitJobRequest) (*apiv1.WaitJobResponse, error) {
	s.mu.Lock()
	env, ok := s.envs[req.Id]
	s.mu.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %q not found", req.Id)
	}

	s.log.Info("dispatch: waiting for job", "id", req.Id)

	exitCode, err := s.rt.Wait(ctx, env)
	if err != nil {
		s.log.Error("dispatch: wait failed", "id", req.Id, "error", err)
		return &apiv1.WaitJobResponse{ExitCode: exitCode}, nil
	}

	s.log.Info("dispatch: job exited", "id", req.Id, "exit_code", exitCode)
	return &apiv1.WaitJobResponse{ExitCode: exitCode}, nil
}

func (s *DispatchServer) DestroyJob(ctx context.Context, req *apiv1.DestroyJobRequest) (*apiv1.DestroyJobResponse, error) {
	s.mu.Lock()
	env, ok := s.envs[req.Id]
	if ok {
		delete(s.envs, req.Id)
	}
	s.mu.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %q not found", req.Id)
	}

	s.log.Info("dispatch: destroying job", "id", req.Id)

	if err := s.rt.Destroy(ctx, env); err != nil {
		s.log.Error("dispatch: destroy failed", "id", req.Id, "error", err)
		return nil, status.Errorf(codes.Internal, "destroying container: %v", err)
	}

	s.log.Info("dispatch: job destroyed", "id", req.Id)
	return &apiv1.DestroyJobResponse{}, nil
}

// StreamContainerStats serves the long-lived sampling stream that the host
// uses to surface per-container resource series. The handler ticks at the
// client-requested cadence and sends one batch per tick covering every
// registered sampler. Returns when the client cancels the context, when
// the underlying connection drops, or when Send fails for any reason.
func (s *DispatchServer) StreamContainerStats(req *apiv1.StreamContainerStatsRequest, stream grpc.ServerStreamingServer[apiv1.ContainerStatsBatch]) error {
	interval := s.defaultInterval
	if req != nil && req.IntervalSeconds > 0 {
		interval = time.Duration(req.IntervalSeconds) * time.Second
	}
	s.log.Info("dispatch: container stats stream opened", "interval", interval)
	defer s.log.Info("dispatch: container stats stream closed")

	ctx := stream.Context()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		case now := <-t.C:
			batch := s.collectBatch(ctx, now)
			if err := stream.Send(batch); err != nil {
				// Network problem or client gone. Return to release the
				// goroutine; the host will reconnect.
				s.log.Debug("dispatch: container stats send failed", "error", err)
				return err
			}
		}
	}
}

func (s *DispatchServer) collectBatch(ctx context.Context, now time.Time) *apiv1.ContainerStatsBatch {
	s.mu.Lock()
	// Snapshot under the lock so a slow Sample call doesn't block
	// Register/Unregister/CreateJob/DestroyJob.
	snaps := make([]samplerEntry, 0, len(s.samplers))
	for _, e := range s.samplers {
		snaps = append(snaps, e)
	}
	s.mu.Unlock()

	out := &apiv1.ContainerStatsBatch{
		TimestampUnixNano: now.UnixNano(),
		Stats:             make([]*apiv1.ContainerStats, 0, len(snaps)),
	}
	for _, e := range snaps {
		stats, err := e.sampler.Sample(ctx)
		if err != nil {
			s.log.Debug("dispatch: sampler failed", "id", e.id, "error", err)
			continue
		}
		out.Stats = append(out.Stats, &apiv1.ContainerStats{
			Id:               e.id,
			Repo:             e.repo,
			CpuUsageNanos:    stats.CPUUsageNanos,
			MemoryBytes:      stats.MemoryBytes,
			MemoryAnonBytes:  stats.MemoryAnonBytes,
			CpuLimit:         stats.CPULimit,
			MemoryLimitBytes: stats.MemoryLimitBytes,
			NetworkRxBytes:   stats.NetworkRxBytes,
			NetworkTxBytes:   stats.NetworkTxBytes,
		})
	}
	return out
}

// DispatchServerConfig configures the in-VM dispatch gRPC server.
type DispatchServerConfig struct {
	Port               int
	Runtime            *runtime.Runtime
	Log                *slog.Logger
	StatsInterval      time.Duration // default 10s when zero
	LinuxCPULimit      uint64        // 0 = unlimited
	LinuxMemLimitBytes uint64        // 0 = unlimited

	// Token is the shared bearer token the server requires on every RPC. When
	// non-empty, unary + stream interceptors reject any call that does not
	// present a matching token (constant-time compare). Empty disables auth —
	// which should only happen in tests or misconfigured setups; production
	// callers plumb a token from the shared data dir (see
	// LoadOrCreateDispatchToken). The server logs loudly when it starts
	// unauthenticated so the footgun is visible.
	Token string

	// BindAddr is the interface the listener binds to. Empty defaults to
	// "0.0.0.0" so the Vz/Hyper-V host on the NAT side can reach it (a narrower
	// bind is only safe when the host address is known; see the comment on
	// StartDispatchServer). The token check protects the surface regardless of
	// bind, so 0.0.0.0 is defense-in-depth-behind-auth rather than the sole
	// control.
	BindAddr string
}

// StartDispatchServer starts the dispatch gRPC server on the given TCP port
// and returns the running server instance plus a cleanup function that
// gracefully stops it. The returned *DispatchServer exposes RegisterSampler
// / UnregisterSampler so the local runtime can plumb its OnTaskStarted /
// OnTaskDestroy hooks into the stats stream surface area.
//
// Binds to cfg.BindAddr (default 0.0.0.0) so the host (outside the VM) can
// reach it. WSL on Windows shares localhost with the host, so this used to be
// 127.0.0.1, but the same process is now invoked from inside an Apple Vz VM
// where the host lives on the NAT side and needs the listener exposed on the
// VM's external interface.
//
// The gRPC surface (CreateJob/WaitJob/DestroyJob/StreamContainerStats) exposes
// container lifecycle control with a caller-supplied image + JIT config, so it
// MUST NOT be reachable unauthenticated by anything sharing the VM's network
// (notably job containers). When cfg.Token is set, every RPC is gated by a
// constant-time bearer-token check via unary + stream interceptors. Operators
// must additionally firewall the dispatch port off from job containers (the
// worker installs bridge control-port rules for exactly this — see main.go
// controlPorts).
func StartDispatchServer(cfg DispatchServerConfig) (*DispatchServer, func()) {
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindAddr, cfg.Port))
	if err != nil {
		cfg.Log.Error("dispatch: failed to listen", "bind", bindAddr, "port", cfg.Port, "error", err)
		return nil, func() {}
	}

	var opts []grpc.ServerOption
	if cfg.Token != "" {
		opts = append(opts,
			grpc.UnaryInterceptor(newAuthUnaryInterceptor(cfg.Token)),
			grpc.StreamInterceptor(newAuthStreamInterceptor(cfg.Token)),
		)
		cfg.Log.Info("dispatch: token authentication enabled", "bind", bindAddr, "port", cfg.Port)
	} else {
		// No token means every process that can reach the port can spawn/kill
		// jobs. This is only acceptable in tests; warn so a misconfigured
		// production worker is visible in the logs.
		cfg.Log.Warn("dispatch: starting WITHOUT authentication — any process that can reach the port can create/destroy jobs",
			"bind", bindAddr, "port", cfg.Port)
	}

	srv := grpc.NewServer(opts...)
	ds := NewDispatchServer(cfg.Runtime, cfg.Log, cfg.StatsInterval, cfg.LinuxCPULimit, cfg.LinuxMemLimitBytes)
	apiv1.RegisterDispatchServer(srv, ds)

	go func() {
		cfg.Log.Info("dispatch server listening", "port", cfg.Port)
		if err := srv.Serve(lis); err != nil {
			cfg.Log.Error("dispatch server error", "error", err)
		}
	}()

	return ds, func() { srv.GracefulStop() }
}

// DispatchClient dispatches Linux jobs to the WSL worker via gRPC.
type DispatchClient struct {
	conn   *grpc.ClientConn
	client apiv1.DispatchClient
}

// NewDispatchClient connects to the dispatch gRPC server at the given address.
// When token is non-empty it is attached to every RPC via per-RPC credentials
// so the server-side interceptor accepts the call; an empty token sends no
// credential (used by tests against an unauthenticated fake server).
//
// The transport stays insecure (no TLS): the dispatch link is a host<->VM
// loopback/NAT hop, and the bearer token — not the transport — authenticates
// the caller.
func NewDispatchClient(addr, token string) (*DispatchClient, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(dispatchTokenCredentials{token: token}))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to dispatch server at %s: %w", addr, err)
	}
	return &DispatchClient{
		conn:   conn,
		client: apiv1.NewDispatchClient(conn),
	}, nil
}

// Create dispatches a container create to the WSL worker. provider + repo
// are passed through so the VM-side dind server can scope its per-repo
// image cache namespace to (provider, repo) and not leak private images
// across forges or repos.
func (d *DispatchClient) Create(ctx context.Context, id, image, jitConfig, provider, repo string) error {
	_, err := d.client.CreateJob(ctx, &apiv1.CreateJobRequest{
		Id:        id,
		Image:     image,
		JitConfig: jitConfig,
		Provider:  provider,
		Repo:      repo,
	})
	return err
}

// Wait blocks until the dispatched job exits and returns its exit code.
func (d *DispatchClient) Wait(ctx context.Context, id string) (uint32, error) {
	resp, err := d.client.WaitJob(ctx, &apiv1.WaitJobRequest{Id: id})
	if err != nil {
		return 1, err
	}
	return resp.ExitCode, nil
}

// Destroy tears down the dispatched job's container.
func (d *DispatchClient) Destroy(ctx context.Context, id string) error {
	_, err := d.client.DestroyJob(ctx, &apiv1.DestroyJobRequest{Id: id})
	return err
}

// ConsumeContainerStats opens the StreamContainerStats stream, asks the
// in-VM dispatch server for samples at the given interval, and feeds each
// batch into the host metrics registry under the supplied runtime label
// (typically metrics.RuntimeLinuxVM). The call returns when ctx is
// cancelled. On non-fatal stream errors the consumer reconnects with
// backoff so a transient network blip in the VM doesn't lose metrics for
// the rest of the daemon's lifetime.
//
// Series are deleted via metrics.DeleteContainerSeries when the consumer
// sees a container id stop appearing in batches (the in-VM
// UnregisterSampler removes it from the stream).
func (d *DispatchClient) ConsumeContainerStats(ctx context.Context, intervalSeconds uint32, runtimeLabel string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	const minBackoff = 1 * time.Second
	const maxBackoff = 30 * time.Second
	backoff := minBackoff
	// known maps id -> repo so we can construct the exact label tuple
	// for DeleteContainerSeries when a container disappears.
	known := make(map[string]string)
	thisRound := make(map[string]string)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		stream, err := d.client.StreamContainerStats(ctx, &apiv1.StreamContainerStatsRequest{IntervalSeconds: intervalSeconds})
		if err != nil {
			log.Warn("dispatch: opening container stats stream failed; retrying", "error", err, "backoff", backoff)
			if err := waitWithCtx(ctx, backoff); err != nil {
				return err
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		backoff = minBackoff
		log.Info("dispatch: container stats stream open", "interval_seconds", intervalSeconds, "runtime_label", runtimeLabel)

		streamErr := readStatsLoop(stream, runtimeLabel, known, thisRound)
		if streamErr == nil || errors.Is(streamErr, io.EOF) {
			log.Info("dispatch: container stats stream closed by server; reconnecting")
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			log.Warn("dispatch: container stats stream errored; reconnecting", "error", streamErr)
		}
		// Clear known set so the next stream's first batch re-establishes
		// the delete-on-disappear bookkeeping cleanly.
		for id, repo := range known {
			metrics.DeleteContainerSeries(id, repo, runtimeLabel)
			delete(known, id)
		}
	}
}

func readStatsLoop(stream grpc.ServerStreamingClient[apiv1.ContainerStatsBatch], runtimeLabel string, known, thisRound map[string]string) error {
	for {
		batch, err := stream.Recv()
		if err != nil {
			return err
		}
		// Reset thisRound, populate from batch.
		for id := range thisRound {
			delete(thisRound, id)
		}
		for _, s := range batch.GetStats() {
			id, repo := s.GetId(), s.GetRepo()
			thisRound[id] = repo
			metrics.RecordContainerStats(id, repo, runtimeLabel, metrics.ContainerStats{
				CPUUsageNanos:    s.GetCpuUsageNanos(),
				MemoryBytes:      s.GetMemoryBytes(),
				MemoryAnonBytes:  s.GetMemoryAnonBytes(),
				CPULimit:         s.GetCpuLimit(),
				MemoryLimitBytes: s.GetMemoryLimitBytes(),
				NetworkRxBytes:   s.GetNetworkRxBytes(),
				NetworkTxBytes:   s.GetNetworkTxBytes(),
			})
			known[id] = repo
		}
		// Anything in known but not in thisRound has been destroyed
		// in-VM since the previous batch; drop its series so the host's
		// cardinality stays bounded by the live container count.
		for id, repo := range known {
			if _, present := thisRound[id]; !present {
				metrics.DeleteContainerSeries(id, repo, runtimeLabel)
				delete(known, id)
			}
		}
	}
}

func waitWithCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// Close closes the gRPC connection.
func (d *DispatchClient) Close() error {
	return d.conn.Close()
}
