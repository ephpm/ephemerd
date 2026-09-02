package buildkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/buildkit/client"
	"google.golang.org/grpc"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCore builds a serverCore that owns a real (never-served) grpc.Server,
// which is all close() touches. Enough to exercise the lifecycle without a
// containerd.
func fakeCore() *serverCore {
	return &serverCore{
		grpcServ: grpc.NewServer(),
		stop:     make(chan struct{}),
	}
}

// TestCoreUnavailableErr pins the whole "there is no core" state machine.
// Each state has to produce a DIFFERENT operator instruction: "the server is
// closed" is a shutdown-ordering bug in the caller, while "the store is
// unavailable" means this node needs restarting before it will build again.
// Conflating them is what made the original build-dead node undiagnosable.
func TestCoreUnavailableErr(t *testing.T) {
	rebuildCause := errors.New("worker controller: containerd unreachable")

	tests := []struct {
		name       string
		hasCore    bool
		closed     bool
		rebuildErr error
		wantNil    bool
		wantIs     error
		wantSubstr string
	}{
		{
			name:    "live core is usable",
			hasCore: true,
			wantNil: true,
		},
		{
			// Close wins even if a rebuild also failed earlier: the server
			// is going away, and "restart the node" would be wrong advice.
			name:       "closed server reports closed, not unavailable",
			closed:     true,
			rebuildErr: rebuildCause,
			wantIs:     ErrServerClosed,
		},
		{
			name:       "failed rebuild names the original cause",
			rebuildErr: rebuildCause,
			wantIs:     ErrStoreUnavailable,
			wantSubstr: "containerd unreachable",
		},
		{
			// Defensive: a nil core with no recorded reason must still fail
			// fast rather than be handed to a dialer.
			name:   "no core and no reason still fails closed",
			wantIs: ErrStoreUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := coreUnavailableErr(tc.hasCore, tc.closed, tc.rebuildErr)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("coreUnavailableErr = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("coreUnavailableErr = nil, want an error")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not mention the original cause %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestServerCoreClose_Idempotent covers the panic that a failed Rebuild used
// to arm: s.core kept pointing at the core Rebuild had already closed, so
// the daemon's own Close closed it a second time and died on "close of
// closed channel" — on the shutdown path, where nothing recovers.
func TestServerCoreClose_Idempotent(t *testing.T) {
	c := fakeCore()
	c.close()
	c.close()
	c.close()

	select {
	case <-c.stop:
	default:
		t.Fatal("stop channel was not closed")
	}
}

// TestServerCoreClose_ConcurrentIsSafe: Rebuild and Close can race on the
// same core through different code paths.
func TestServerCoreClose_ConcurrentIsSafe(t *testing.T) {
	c := fakeCore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.close()
		}()
	}
	wg.Wait()
}

// TestServerClose_Idempotent: Close is documented as safe to call twice and
// the service shutdown path does exactly that (defer plus an explicit stop).
func TestServerClose_Idempotent(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir(), Log: discardLog()}, core: fakeCore()}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Client(context.Background()); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Client after Close = %v, want ErrServerClosed", err)
	}
}

// TestRebuild_FailurePathLeavesCoherentState is the regression test for the
// build-dead node.
//
// The ContainerdAddress is deliberately bogus, so newCore cannot succeed —
// neither the rebuild itself nor the one restore attempt. What must hold
// afterwards:
//   - Rebuild returns an error (it did before too),
//   - s.core is nil rather than a pointer to the core Rebuild just stopped,
//   - builds fail fast with ErrStoreUnavailable instead of dialing a dead
//     in-process gRPC server forever,
//   - the previous store is back on disk, not stranded in quarantine,
//   - and Close afterwards does not panic.
func TestRebuild_FailurePathLeavesCoherentState(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dataDir, "marker")
	if err := os.WriteFile(marker, []byte("previous store"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := fakeCore()
	s := &Server{
		cfg: Config{
			DataDir:           dataDir,
			ContainerdAddress: "unix:///definitely/not/here.sock",
			Log:               discardLog(),
		},
		core: old,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Rebuild(ctx); err == nil {
		t.Fatal("Rebuild succeeded against a bogus containerd; the test proves nothing")
	}

	if s.core != nil {
		t.Fatal("s.core is non-nil after a failed Rebuild — this is the build-dead-node bug: every later build would dial a stopped gRPC server")
	}
	if s.rebuildErr == nil {
		t.Error("rebuildErr not recorded; the operator error cannot name the cause")
	}

	if _, err := s.Client(ctx); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Client after a failed Rebuild = %v, want ErrStoreUnavailable", err)
	}
	if _, err := s.Build(ctx, client.SolveOpt{}, nil); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Build after a failed Rebuild = %v, want ErrStoreUnavailable", err)
	}
	if _, err := s.Prune(ctx, client.PruneInfo{}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Prune after a failed Rebuild = %v, want ErrStoreUnavailable", err)
	}

	// The old store must be back where it was, not left in quarantine.
	if b, err := os.ReadFile(marker); err != nil || string(b) != "previous store" {
		t.Errorf("previous store not restored: read %q, err %v", b, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), quarantineDirPrefix) {
			t.Errorf("quarantine %q left behind after a restore", e.Name())
		}
	}

	// Would have panicked with "close of closed channel" before the fix.
	if err := s.Close(); err != nil {
		t.Fatalf("Close after a failed Rebuild: %v", err)
	}
}

// TestRebuild_AfterCloseIsRejected: the heal ladder can fire concurrently
// with daemon shutdown. Rebuild must not resurrect a solver on a Server that
// is on its way out (which would leak a gRPC server and a bbolt handle).
func TestRebuild_AfterCloseIsRejected(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir(), Log: discardLog()}, core: fakeCore()}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Rebuild(context.Background()); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Rebuild after Close = %v, want ErrServerClosed", err)
	}
	if s.core != nil {
		t.Error("Rebuild after Close installed a core")
	}
}
