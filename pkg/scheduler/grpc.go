package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/upgrade"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// controlServer implements the gRPC Control service.
type controlServer struct {
	apiv1.UnimplementedControlServer
	sched *Scheduler
	log   *slog.Logger
}

// SocketPath returns the path to the gRPC control socket for a given data dir.
func SocketPath(dataDir string) string {
	return dataDir + string(os.PathSeparator) + "ephemerd.sock"
}

// startControlServer starts the gRPC control server on a unix socket.
// Returns a cleanup function that stops the server.
func (s *Scheduler) startControlServer() (func(), error) {
	socketPath := SocketPath(s.cfg.DataDir)

	// Remove stale socket from previous run (best-effort, may not exist)
	_ = os.Remove(socketPath)

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	// Restrict the control socket to privileged principals. The control API can
	// KillJob (DoS) and stream GetJobLogs (may contain job secrets), so it must
	// not be reachable by ordinary local users. On POSIX this is chmod 0600; on
	// Windows POSIX mode bits are a near-no-op for an AF_UNIX socket file, so
	// secureControlSocket applies a real NTFS ACL (SYSTEM + Administrators only).
	if err := secureControlSocket(socketPath); err != nil {
		if closeErr := lis.Close(); closeErr != nil {
			s.cfg.Log.Warn("closing control socket listener after ACL failure", "error", closeErr)
		}
		return nil, fmt.Errorf("securing control socket %s: %w", socketPath, err)
	}

	srv := grpc.NewServer()
	apiv1.RegisterControlServer(srv, &controlServer{
		sched: s,
		log:   s.cfg.Log,
	})

	go func() {
		if err := srv.Serve(lis); err != nil {
			s.cfg.Log.Error("grpc server error", "error", err)
		}
	}()

	s.cfg.Log.Info("control socket ready", "path", socketPath)

	cleanup := func() {
		srv.GracefulStop()
		_ = os.Remove(socketPath)
	}
	return cleanup, nil
}

func (c *controlServer) Status(ctx context.Context, req *apiv1.StatusRequest) (*apiv1.StatusResponse, error) {
	c.sched.mu.Lock()
	activeJobs := len(c.sched.running)
	draining := c.sched.draining
	c.sched.mu.Unlock()

	return &apiv1.StatusResponse{
		Status:        "ok",
		ActiveJobs:    int32(activeJobs),
		MaxConcurrent: int32(c.sched.cfg.MaxConcurrent),
		Draining:      draining,
		Uptime:        time.Since(c.sched.startTime).Truncate(time.Second).String(),
		Version:       c.sched.cfg.Version,
	}, nil
}

// Upgrade downloads a specific published release, verifies its checksum,
// drains running jobs, swaps the daemon's own binary, and restarts the
// service into it — all over the daemon's own outbound HTTPS. Progress is
// streamed; see pkg/upgrade for the sequence and per-OS swap strategy.
//
// The last message sent is UPGRADE_STATE_RESTARTING, emitted BEFORE the
// detached restart fires, so the caller (mayfly / the CLI) sees a clean
// stream end and then polls Status until the reported version matches the
// target. A running-daemon restart is a hand-off: this stream must not be
// mistaken for a failure just because it ends when the process goes down.
func (c *controlServer) Upgrade(req *apiv1.UpgradeRequest, stream grpc.ServerStreamingServer[apiv1.UpgradeProgress]) error {
	emit := func(p upgrade.Progress) {
		if err := stream.Send(progressToProto(p)); err != nil {
			c.log.Debug("upgrade: sending progress", "error", err)
		}
	}
	opts := upgrade.RunOptions{
		TargetVersion:   req.TargetVersion,
		CurrentVersion:  c.sched.cfg.Version,
		BaseURLOverride: req.UrlOverride,
		NoDrain:         req.NoDrain,
		Force:           req.Force,
		DrainTimeout:    time.Duration(req.DrainTimeoutSeconds) * time.Second,
		Drainer:         c.sched,
		Log:             c.log.With("component", "upgrade"),
	}
	c.log.Info("upgrade requested via grpc", "target", req.TargetVersion, "no_drain", req.NoDrain, "force", req.Force)
	if err := upgrade.Run(stream.Context(), opts, emit); err != nil {
		return status.Errorf(codes.Internal, "upgrade: %v", err)
	}
	return nil
}

// progressToProto maps an upgrade.Progress onto the wire enum/message.
func progressToProto(p upgrade.Progress) *apiv1.UpgradeProgress {
	return &apiv1.UpgradeProgress{
		State:           upgradeStateToProto(p.State),
		Message:         p.Message,
		CurrentVersion:  p.CurrentVersion,
		TargetVersion:   p.TargetVersion,
		ActiveJobs:      int32(p.ActiveJobs),
		BytesDownloaded: p.BytesDownloaded,
		BytesTotal:      p.BytesTotal,
	}
}

func upgradeStateToProto(s upgrade.State) apiv1.UpgradeState {
	switch s {
	case upgrade.StatePreflight:
		return apiv1.UpgradeState_UPGRADE_STATE_PREFLIGHT
	case upgrade.StateUpToDate:
		return apiv1.UpgradeState_UPGRADE_STATE_UP_TO_DATE
	case upgrade.StateDraining:
		return apiv1.UpgradeState_UPGRADE_STATE_DRAINING
	case upgrade.StateDownloading:
		return apiv1.UpgradeState_UPGRADE_STATE_DOWNLOADING
	case upgrade.StateVerifying:
		return apiv1.UpgradeState_UPGRADE_STATE_VERIFYING
	case upgrade.StateStaging:
		return apiv1.UpgradeState_UPGRADE_STATE_STAGING
	case upgrade.StateSwapping:
		return apiv1.UpgradeState_UPGRADE_STATE_SWAPPING
	case upgrade.StateRestarting:
		return apiv1.UpgradeState_UPGRADE_STATE_RESTARTING
	case upgrade.StateFailed:
		return apiv1.UpgradeState_UPGRADE_STATE_FAILED
	default:
		return apiv1.UpgradeState_UPGRADE_STATE_UNSPECIFIED
	}
}

// Cordon stops the scheduler from claiming new jobs without initiating
// shutdown. Running jobs continue undisturbed. Used by `ephemerd drain
// --wait`, which polls Status until ActiveJobs reaches zero.
func (c *controlServer) Cordon(ctx context.Context, req *apiv1.CordonRequest) (*apiv1.CordonResponse, error) {
	active := c.sched.Cordon()
	c.log.Info("scheduler cordoned via grpc", "active_jobs", active)
	return &apiv1.CordonResponse{ActiveJobs: int32(active)}, nil
}

// Uncordon resumes claiming after a Cordon (e.g. an aborted drain).
func (c *controlServer) Uncordon(ctx context.Context, req *apiv1.UncordonRequest) (*apiv1.UncordonResponse, error) {
	active := c.sched.Uncordon()
	c.log.Info("scheduler uncordoned via grpc", "active_jobs", active)
	return &apiv1.UncordonResponse{ActiveJobs: int32(active)}, nil
}

// findJob looks up a running job by its int64 ID. When multiple providers are
// active, different providers could theoretically have the same job ID, so this
// returns the first match. The gRPC proto will gain a provider field in Phase 2
// to disambiguate.
func (c *controlServer) findJob(id int64) (jobKey, *runningJob, bool) {
	for key, rj := range c.sched.running {
		if key.JobID == id {
			return key, rj, true
		}
	}
	return jobKey{}, nil, false
}

func (c *controlServer) ListJobs(ctx context.Context, req *apiv1.ListJobsRequest) (*apiv1.ListJobsResponse, error) {
	c.sched.mu.Lock()
	jobs := make([]*apiv1.Job, 0, len(c.sched.running))
	for key, rj := range c.sched.running {
		jobs = append(jobs, c.toProto(key, rj))
	}
	c.sched.mu.Unlock()

	return &apiv1.ListJobsResponse{Jobs: jobs}, nil
}

func (c *controlServer) GetJob(ctx context.Context, req *apiv1.GetJobRequest) (*apiv1.Job, error) {
	c.sched.mu.Lock()
	key, rj, exists := c.findJob(req.Id)
	if !exists {
		c.sched.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "job %d not found", req.Id)
	}
	job := c.toProto(key, rj)
	c.sched.mu.Unlock()

	return job, nil
}

func (c *controlServer) KillJob(ctx context.Context, req *apiv1.KillJobRequest) (*apiv1.KillJobResponse, error) {
	c.sched.mu.Lock()
	_, rj, exists := c.findJob(req.Id)
	if !exists {
		c.sched.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "job %d not found", req.Id)
	}
	c.sched.mu.Unlock()

	c.log.Info("killing job via grpc", "job_id", req.Id)
	rj.cancel()

	return &apiv1.KillJobResponse{}, nil
}

func (c *controlServer) GetJobLogs(req *apiv1.GetJobLogsRequest, stream grpc.ServerStreamingServer[apiv1.LogChunk]) error {
	c.sched.mu.Lock()
	_, rj, exists := c.findJob(req.Id)
	if !exists {
		c.sched.mu.Unlock()
		return status.Errorf(codes.NotFound, "job %d not found", req.Id)
	}
	if rj.env == nil {
		c.sched.mu.Unlock()
		return status.Errorf(codes.Unimplemented, "logs not available for dispatched jobs")
	}
	name := rj.env.ID
	c.sched.mu.Unlock()

	logPath := fmt.Sprintf("%s/logs/%s.log", c.sched.cfg.DataDir, name)
	f, err := os.Open(logPath)
	if err != nil {
		return status.Errorf(codes.NotFound, "log file not found: %v", err)
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&apiv1.LogChunk{Data: buf[:n]}); sendErr != nil {
				if closeErr := f.Close(); closeErr != nil {
					c.log.Debug("closing log file after send error", "error", closeErr)
				}
				return sendErr
			}
		}
		if readErr == io.EOF {
			return f.Close()
		}
		if readErr != nil {
			if closeErr := f.Close(); closeErr != nil {
				c.log.Debug("closing log file after read error", "error", closeErr)
			}
			return status.Errorf(codes.Internal, "reading log: %v", readErr)
		}
	}
}

func (c *controlServer) toProto(key jobKey, rj *runningJob) *apiv1.Job {
	var runnerID int64
	if rj.claim != nil {
		runnerID = rj.claim.RunnerID
	}
	job := &apiv1.Job{
		Id:        key.JobID,
		Repo:      rj.repo,
		Image:     rj.image,
		RunnerId:  runnerID,
		Status:    "running",
		StartedAt: rj.startedAt.Format(time.RFC3339),
		Uptime:    time.Since(rj.startedAt).Truncate(time.Second).String(),
	}
	if rj.dispatched != "" {
		job.Name = rj.dispatched
	} else if rj.env != nil {
		job.Name = rj.env.ID
		job.Pid = rj.env.Task.Pid()
	}
	return job
}
