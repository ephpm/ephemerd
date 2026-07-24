package scheduler

import (
	"context"
	"net"
	"testing"
	"time"

	apiv1 "github.com/ephpm/ephemerd/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startAuthedDispatchServer starts a real gRPC server guarded by the token
// interceptors and returns its address plus a cleanup. The fakeDispatchServer
// (from concurrent_test.go) backs the RPCs so a call that passes the
// interceptor gets a normal empty-success response.
func startAuthedDispatchServer(t *testing.T, token string) (string, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(newAuthUnaryInterceptor(token)),
		grpc.StreamInterceptor(newAuthStreamInterceptor(token)),
	)
	apiv1.RegisterDispatchServer(srv, &fakeDispatchServer{})
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("authed dispatch serve: %v", err)
		}
	}()

	return lis.Addr().String(), func() { srv.GracefulStop() }
}

func dialRaw(t *testing.T, addr, token string) *DispatchClient {
	t.Helper()
	c, err := NewDispatchClient(addr, token)
	if err != nil {
		t.Fatalf("NewDispatchClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	})
	return c
}

// TestDispatchAuth_ValidTokenAccepted verifies the happy path: a client that
// presents the matching token reaches the handler.
func TestDispatchAuth_ValidTokenAccepted(t *testing.T) {
	const token = "s3cr3t-token"
	addr, cleanup := startAuthedDispatchServer(t, token)
	defer cleanup()

	c := dialRaw(t, addr, token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Create(ctx, "job-1", "img", "jit", "github", "owner/repo"); err != nil {
		t.Fatalf("Create with valid token: %v", err)
	}
}

// TestDispatchAuth_WrongTokenRejected verifies a mismatched token is rejected
// with Unauthenticated and never reaches the handler.
func TestDispatchAuth_WrongTokenRejected(t *testing.T) {
	addr, cleanup := startAuthedDispatchServer(t, "correct-token")
	defer cleanup()

	c := dialRaw(t, addr, "wrong-token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Create(ctx, "job-1", "img", "jit", "github", "owner/repo")
	if err == nil {
		t.Fatal("expected Unauthenticated error for wrong token")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

// TestDispatchAuth_MissingTokenRejected verifies a client that sends NO token
// (the pre-fix behaviour of any process on the VM network) is rejected.
func TestDispatchAuth_MissingTokenRejected(t *testing.T) {
	addr, cleanup := startAuthedDispatchServer(t, "correct-token")
	defer cleanup()

	// Empty token => NewDispatchClient attaches no per-RPC credentials, exactly
	// like an attacker dialing the port with a stock gRPC client.
	c := dialRaw(t, addr, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Destroy(ctx, "job-1")
	if err == nil {
		t.Fatal("expected Unauthenticated error for missing token")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

// TestDispatchAuth_StreamMissingTokenRejected checks the streaming interceptor
// guards StreamContainerStats too, not just the unary RPCs.
func TestDispatchAuth_StreamMissingTokenRejected(t *testing.T) {
	addr, cleanup := startAuthedDispatchServer(t, "correct-token")
	defer cleanup()

	// Dial with a raw connection carrying no credentials.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Logf("conn close: %v", err)
		}
	}()
	client := apiv1.NewDispatchClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.StreamContainerStats(ctx, &apiv1.StreamContainerStatsRequest{})
	if err == nil {
		// The RPC is lazy; the auth error surfaces on first Recv.
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("expected Unauthenticated error opening stream without token")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}

// TestGenerateDispatchToken_UniqueHex verifies tokens are non-empty, hex, and
// distinct across calls.
func TestGenerateDispatchToken_UniqueHex(t *testing.T) {
	a, err := GenerateDispatchToken()
	if err != nil {
		t.Fatalf("GenerateDispatchToken: %v", err)
	}
	b, err := GenerateDispatchToken()
	if err != nil {
		t.Fatalf("GenerateDispatchToken: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("generated empty token")
	}
	if a == b {
		t.Fatal("expected distinct tokens across calls")
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Fatalf("token length = %d, want 64", len(a))
	}
}
