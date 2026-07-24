package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// dispatchTokenMetadataKey is the gRPC metadata key the host client uses to
// present the shared dispatch bearer token to the in-VM server. Lower-case
// per the gRPC metadata convention (keys are normalized to lower-case).
const dispatchTokenMetadataKey = "x-ephemerd-dispatch-token"

// GenerateDispatchToken returns a fresh 256-bit hex dispatch token. The host
// mints one when config.toml carries no explicit [dispatch].token and persists
// it back into config.toml (see config.EnsureDispatchToken) so it rides into
// the Linux VM through the existing config-delivery channel (initrd on Windows,
// shared data dir on macOS) and the in-VM server reads the identical value.
func GenerateDispatchToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating dispatch token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newAuthUnaryInterceptor returns a server-side unary interceptor that rejects
// any call whose metadata does not carry the expected token. The comparison is
// constant-time to avoid leaking the token via timing.
func newAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	want := []byte(token)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkDispatchToken(ctx, want); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// newAuthStreamInterceptor is the streaming counterpart to
// newAuthUnaryInterceptor; it guards StreamContainerStats.
func newAuthStreamInterceptor(token string) grpc.StreamServerInterceptor {
	want := []byte(token)
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkDispatchToken(ss.Context(), want); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// checkDispatchToken validates the bearer token carried in the request
// metadata against want using a constant-time compare. A missing or mismatched
// token yields codes.Unauthenticated.
func checkDispatchToken(ctx context.Context, want []byte) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing dispatch credentials")
	}
	vals := md.Get(dispatchTokenMetadataKey)
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing dispatch token")
	}
	// subtle.ConstantTimeCompare returns 1 only when both length and content
	// match; it is safe against the empty-token case because want is non-empty
	// whenever the interceptor is installed.
	if subtle.ConstantTimeCompare([]byte(vals[0]), want) != 1 {
		return status.Error(codes.Unauthenticated, "invalid dispatch token")
	}
	return nil
}

// dispatchTokenCredentials is a grpc.PerRPCCredentials that attaches the shared
// dispatch token to every outbound RPC. RequireTransportSecurity is false
// because the dispatch link is a host<->VM loopback/NAT hop, not a public
// network; the token authenticates the caller, and narrowing the bind
// (F1) plus firewalling job containers off the port limits exposure.
type dispatchTokenCredentials struct {
	token string
}

func (c dispatchTokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{dispatchTokenMetadataKey: c.token}, nil
}

func (c dispatchTokenCredentials) RequireTransportSecurity() bool {
	return false
}
