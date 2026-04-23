package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"github.com/ephpm/ephemerd/pkg/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// dispatchServer implements the Dispatch gRPC service.
// It runs inside the WSL containerd-only worker and proxies
// Create/Wait/Destroy calls to the local Linux Runtime.
type dispatchServer struct {
	apiv1.UnimplementedDispatchServer
	rt   *runtime.Runtime
	log  *slog.Logger
	mu   sync.Mutex
	envs map[string]*runtime.RunnerEnv
}

func (s *dispatchServer) CreateJob(ctx context.Context, req *apiv1.CreateJobRequest) (*apiv1.CreateJobResponse, error) {
	s.log.Info("dispatch: creating job", "id", req.Id, "image", req.Image)

	env, err := s.rt.Create(ctx, runtime.CreateConfig{
		ID: req.Id, Image: req.Image, JITConfig: req.JitConfig,
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

func (s *dispatchServer) WaitJob(ctx context.Context, req *apiv1.WaitJobRequest) (*apiv1.WaitJobResponse, error) {
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

func (s *dispatchServer) DestroyJob(ctx context.Context, req *apiv1.DestroyJobRequest) (*apiv1.DestroyJobResponse, error) {
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

// StartDispatchServer starts the dispatch gRPC server on the given TCP port.
// Returns a cleanup function that gracefully stops the server.
//
// Binds to 0.0.0.0 so the host (outside the VM) can reach it. WSL on Windows
// shares localhost with the host, so this used to be 127.0.0.1, but the same
// process is now invoked from inside an Apple Vz VM where the host lives on
// the NAT side and needs the listener exposed on the VM's external interface.
func StartDispatchServer(port int, rt *runtime.Runtime, log *slog.Logger) func() {
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		log.Error("dispatch: failed to listen", "port", port, "error", err)
		return func() {}
	}

	srv := grpc.NewServer()
	apiv1.RegisterDispatchServer(srv, &dispatchServer{
		rt:   rt,
		log:  log,
		envs: make(map[string]*runtime.RunnerEnv),
	})

	go func() {
		log.Info("dispatch server listening", "port", port)
		if err := srv.Serve(lis); err != nil {
			log.Error("dispatch server error", "error", err)
		}
	}()

	return func() { srv.GracefulStop() }
}

// DispatchClient dispatches Linux jobs to the WSL worker via gRPC.
type DispatchClient struct {
	conn   *grpc.ClientConn
	client apiv1.DispatchClient
}

// NewDispatchClient connects to the dispatch gRPC server at the given address.
func NewDispatchClient(addr string) (*DispatchClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to dispatch server at %s: %w", addr, err)
	}
	return &DispatchClient{
		conn:   conn,
		client: apiv1.NewDispatchClient(conn),
	}, nil
}

// Create dispatches a container create to the WSL worker.
func (d *DispatchClient) Create(ctx context.Context, id, image, jitConfig string) error {
	_, err := d.client.CreateJob(ctx, &apiv1.CreateJobRequest{
		Id:        id,
		Image:     image,
		JitConfig: jitConfig,
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

// Close closes the gRPC connection.
func (d *DispatchClient) Close() error {
	return d.conn.Close()
}
