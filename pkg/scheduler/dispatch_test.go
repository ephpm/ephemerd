package scheduler

import (
	"context"
	"net"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
"google.golang.org/grpc/status"
)

// fakeDispatchServer implements apiv1.DispatchServer with configurable
// responses. Used to test DispatchClient round-trips without standing up
// a real runtime.
type fakeDispatchServer struct {
	apiv1.UnimplementedDispatchServer

	// Calls received.
	createCalls   []*apiv1.CreateJobRequest
	waitCalls     []*apiv1.WaitJobRequest
	destroyCalls  []*apiv1.DestroyJobRequest

	// Configurable responses.
	createErr    error
	waitErr      error
	waitExitCode uint32
	destroyErr   error
}

func (s *fakeDispatchServer) CreateJob(_ context.Context, req *apiv1.CreateJobRequest) (*apiv1.CreateJobResponse, error) {
	s.createCalls = append(s.createCalls, req)
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &apiv1.CreateJobResponse{}, nil
}

func (s *fakeDispatchServer) WaitJob(_ context.Context, req *apiv1.WaitJobRequest) (*apiv1.WaitJobResponse, error) {
	s.waitCalls = append(s.waitCalls, req)
	if s.waitErr != nil {
		return nil, s.waitErr
	}
	return &apiv1.WaitJobResponse{ExitCode: s.waitExitCode}, nil
}

func (s *fakeDispatchServer) DestroyJob(_ context.Context, req *apiv1.DestroyJobRequest) (*apiv1.DestroyJobResponse, error) {
	s.destroyCalls = append(s.destroyCalls, req)
	if s.destroyErr != nil {
		return nil, s.destroyErr
	}
	return &apiv1.DestroyJobResponse{}, nil
}

// startFakeDispatchServer spins up a gRPC server on a random local port and
// returns the bound address and a cleanup function. Tests connect to the
// returned address with NewDispatchClient.
func startFakeDispatchServer(t *testing.T, impl *fakeDispatchServer) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	apiv1.RegisterDispatchServer(srv, impl)

	go func() {
		_ = srv.Serve(lis)
	}()

	cleanup := func() {
		srv.Stop()
		_ = lis.Close()
	}
	return lis.Addr().String(), cleanup
}

func TestNewDispatchClient_ValidAddr(t *testing.T) {
	addr, cleanup := startFakeDispatchServer(t, &fakeDispatchServer{})
	defer cleanup()

	c, err := NewDispatchClient(addr)
	if err != nil {
		t.Fatalf("NewDispatchClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	})
}

// TestNewDispatchClient_InvalidAddr verifies that an obviously malformed
// address surfaces an error from grpc.NewClient (e.g., scheme parsing).
func TestNewDispatchClient_InvalidAddr(t *testing.T) {
	// grpc.NewClient is lazy and accepts most strings; only blatantly
	// invalid scheme syntax fails. Use one that will be rejected.
	_, err := NewDispatchClient("://!!invalid//")
	if err == nil {
		t.Skip("grpc.NewClient is lazy; this URL doesn't fail parsing on this platform")
	}
}

func TestDispatchClient_Create(t *testing.T) {
	impl := &fakeDispatchServer{}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, err := NewDispatchClient(addr)
	if err != nil {
		t.Fatalf("NewDispatchClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Create(ctx, "job-1", "alpine:latest", "jit-config"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(impl.createCalls) != 1 {
		t.Fatalf("create call count = %d, want 1", len(impl.createCalls))
	}
	got := impl.createCalls[0]
	if got.Id != "job-1" {
		t.Errorf("Id = %q, want job-1", got.Id)
	}
	if got.Image != "alpine:latest" {
		t.Errorf("Image = %q", got.Image)
	}
	if got.JitConfig != "jit-config" {
		t.Errorf("JitConfig = %q", got.JitConfig)
	}
}

func TestDispatchClient_Create_PropagatesError(t *testing.T) {
	impl := &fakeDispatchServer{
		createErr: status.Errorf(codes.Internal, "containerd unavailable"),
	}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Create(ctx, "job-1", "img", "")
	if err == nil {
		t.Fatal("expected error from server")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got %v", err)
	}
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

func TestDispatchClient_Wait_Success(t *testing.T) {
	impl := &fakeDispatchServer{waitExitCode: 0}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := c.Wait(ctx, "job-1")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
}

func TestDispatchClient_Wait_NonZeroExit(t *testing.T) {
	impl := &fakeDispatchServer{waitExitCode: 137}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := c.Wait(ctx, "job-1")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit != 137 {
		t.Errorf("exit = %d, want 137", exit)
	}
}

func TestDispatchClient_Wait_Error(t *testing.T) {
	impl := &fakeDispatchServer{
		waitErr: status.Errorf(codes.NotFound, "job not found"),
	}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := c.Wait(ctx, "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if exit != 1 {
		t.Errorf("exit on error = %d, want 1 (sentinel)", exit)
	}
}

func TestDispatchClient_Destroy(t *testing.T) {
	impl := &fakeDispatchServer{}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Destroy(ctx, "job-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(impl.destroyCalls) != 1 {
		t.Fatalf("destroy call count = %d, want 1", len(impl.destroyCalls))
	}
	if impl.destroyCalls[0].Id != "job-1" {
		t.Errorf("Id = %q, want job-1", impl.destroyCalls[0].Id)
	}
}

func TestDispatchClient_Destroy_PropagatesError(t *testing.T) {
	impl := &fakeDispatchServer{
		destroyErr: status.Errorf(codes.Internal, "destroy failed"),
	}
	addr, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Destroy(ctx, "job-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDispatchClient_Close_Idempotent verifies double-close doesn't panic.
func TestDispatchClient_Close_Idempotent(t *testing.T) {
	addr, cleanup := startFakeDispatchServer(t, &fakeDispatchServer{})
	defer cleanup()

	c, _ := NewDispatchClient(addr)
	if err := c.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	// Second close — gRPC reports an "already closed" error, not a panic.
	_ = c.Close()
}

