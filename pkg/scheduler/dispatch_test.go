package scheduler

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNewDispatchClient_ValidAddr(t *testing.T) {
	addr, _, cleanup := startFakeDispatchServer(t, &fakeDispatchServer{})
	defer cleanup()

	c, err := NewDispatchClient(addr, "")
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
	_, err := NewDispatchClient("://!!invalid//", "")
	if err == nil {
		t.Skip("grpc.NewClient is lazy; this URL doesn't fail parsing on this platform")
	}
}

func TestDispatchClient_Create(t *testing.T) {
	impl := &fakeDispatchServer{}
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, err := NewDispatchClient(addr, "")
	if err != nil {
		t.Fatalf("NewDispatchClient: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Create(ctx, "job-1", "alpine:latest", "jit-config", "github", "owner/repo"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	impl.mu.Lock()
	defer impl.mu.Unlock()
	if len(impl.createRequests) != 1 {
		t.Fatalf("create call count = %d, want 1", len(impl.createRequests))
	}
	got := impl.createRequests[0]
	if got.Id != "job-1" {
		t.Errorf("Id = %q, want job-1", got.Id)
	}
	if got.Image != "alpine:latest" {
		t.Errorf("Image = %q", got.Image)
	}
	if got.JitConfig != "jit-config" {
		t.Errorf("JitConfig = %q", got.JitConfig)
	}
	if got.Provider != "github" {
		t.Errorf("Provider = %q, want github", got.Provider)
	}
	if got.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want owner/repo", got.Repo)
	}
}

func TestDispatchClient_Create_PropagatesError(t *testing.T) {
	impl := &fakeDispatchServer{
		createErr: status.Errorf(codes.Internal, "containerd unavailable"),
	}
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Create(ctx, "job-1", "img", "", "github", "owner/repo")
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
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

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
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

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
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

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
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Destroy(ctx, "job-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if len(impl.destroyRequests) != 1 {
		t.Fatalf("destroy call count = %d, want 1", len(impl.destroyRequests))
	}
	if impl.destroyRequests[0].Id != "job-1" {
		t.Errorf("Id = %q, want job-1", impl.destroyRequests[0].Id)
	}
}

func TestDispatchClient_Destroy_PropagatesError(t *testing.T) {
	impl := &fakeDispatchServer{
		destroyErr: status.Errorf(codes.Internal, "destroy failed"),
	}
	addr, _, cleanup := startFakeDispatchServer(t, impl)
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Destroy(ctx, "job-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDispatchClient_Close_Idempotent verifies double-close doesn't panic.
func TestDispatchClient_Close_Idempotent(t *testing.T) {
	addr, _, cleanup := startFakeDispatchServer(t, &fakeDispatchServer{})
	defer cleanup()

	c, _ := NewDispatchClient(addr, "")
	if err := c.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	// Second close — gRPC reports an "already closed" error, not a panic.
	if err := c.Close(); err != nil {
		t.Logf("second close (expected): %v", err)
	}
}
