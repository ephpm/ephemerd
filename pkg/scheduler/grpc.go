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
	}, nil
}

func (c *controlServer) ListJobs(ctx context.Context, req *apiv1.ListJobsRequest) (*apiv1.ListJobsResponse, error) {
	c.sched.mu.Lock()
	jobs := make([]*apiv1.Job, 0, len(c.sched.running))
	for id, rj := range c.sched.running {
		jobs = append(jobs, c.toProto(id, rj))
	}
	c.sched.mu.Unlock()

	return &apiv1.ListJobsResponse{Jobs: jobs}, nil
}

func (c *controlServer) GetJob(ctx context.Context, req *apiv1.GetJobRequest) (*apiv1.Job, error) {
	c.sched.mu.Lock()
	rj, exists := c.sched.running[req.Id]
	if !exists {
		c.sched.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "job %d not found", req.Id)
	}
	job := c.toProto(req.Id, rj)
	c.sched.mu.Unlock()

	return job, nil
}

func (c *controlServer) KillJob(ctx context.Context, req *apiv1.KillJobRequest) (*apiv1.KillJobResponse, error) {
	c.sched.mu.Lock()
	rj, exists := c.sched.running[req.Id]
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
	rj, exists := c.sched.running[req.Id]
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

func (c *controlServer) toProto(jobID int64, rj *runningJob) *apiv1.Job {
	job := &apiv1.Job{
		Id:        jobID,
		Repo:      rj.repo,
		Image:     rj.image,
		RunnerId:  rj.runnerID,
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
