package buildkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewServer_RequiresDataDir(t *testing.T) {
	_, err := NewServer(context.Background(), Config{
		ContainerdAddress: "unix:///tmp/nonexistent.sock",
	})
	if err == nil {
		t.Fatal("expected error for missing DataDir, got nil")
	}
	if !strings.Contains(err.Error(), "DataDir") {
		t.Errorf("error should mention DataDir, got: %v", err)
	}
}

func TestNewServer_RequiresContainerdAddress(t *testing.T) {
	_, err := NewServer(context.Background(), Config{
		DataDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for missing ContainerdAddress, got nil")
	}
	if !strings.Contains(err.Error(), "ContainerdAddress") {
		t.Errorf("error should mention ContainerdAddress, got: %v", err)
	}
}

// TestNewServer_GracefulFailureOnMissingContainerd validates that the server
// constructor returns a real error (not a panic) when the containerd address
// doesn't resolve. This is the dominant failure mode during dev startup;
// it has to be diagnosable.
func TestNewServer_GracefulFailureOnMissingContainerd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := Config{
		DataDir:           t.TempDir(),
		ContainerdAddress: "unix:///definitely/not/here.sock",
	}

	_, err := NewServer(ctx, cfg)
	if err == nil {
		t.Fatal("expected error when containerd is unreachable, got nil")
	}
	// The exact error message comes from containerd client.New; we just
	// want to confirm it's a Go error (not a panic) and that it mentions
	// something buildkit/containerd-related.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Logf("got context deadline exceeded (acceptable): %v", err)
		return
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "containerd") && !strings.Contains(lower, "worker") && !strings.Contains(lower, "connect") && !strings.Contains(lower, "dial") {
		t.Errorf("error doesn't clearly point at containerd/connection; got: %v", err)
	}
}
