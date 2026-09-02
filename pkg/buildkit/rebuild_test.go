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
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/util/db/boltutil"
	"google.golang.org/grpc"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCore builds a serverCore that owns a real (never-served) grpc.Server.
// Enough to exercise the lifecycle without a containerd.
//
// NOTE: it deliberately has NO controller, so it cannot say anything about
// the handle-release contract. That is what
// TestServerCoreClose_ReleasesTheBoltHandles is for — this shape is precisely
// why the original failure-path test could not catch the deadlock.
func fakeCore() *serverCore {
	return &serverCore{
		grpcServ: grpc.NewServer(),
		stop:     make(chan struct{}),
		log:      discardLog(),
	}
}

// closerFn adapts a function to the coreController interface.
type closerFn func() error

func (f closerFn) Close() error { return f() }

// withinDeadline runs fn and fails the test if it has not returned by d.
// Every lifecycle assertion below uses it: the bug class being guarded here
// is "blocks forever holding s.mu", and a test that reproduces it by HANGING
// is useless — CI reports a timeout with no attribution instead of a failure
// naming the property that broke.
func withinDeadline(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v — the daemon is wedged (this is the deadlock, not a slow test)", what, d)
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

	var rebuildErr error
	withinDeadline(t, 60*time.Second, "Rebuild against a bogus containerd", func() {
		rebuildErr = s.Rebuild(ctx)
	})
	if rebuildErr == nil {
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

	// Would have panicked with "close of closed channel" before the fix —
	// and, after the reinit path was added, would have blocked on s.mu
	// forever. Both are failures here, neither is a hang.
	withinDeadline(t, 30*time.Second, "Close after a failed Rebuild", func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close after a failed Rebuild: %v", err)
		}
	})
}

// TestServerCoreClose_ClosesTheController is the direct regression test for
// the deadlock.
//
// serverCore.controller was assigned in newCore and then never read: close()
// did `close(c.stop)` + `GracefulStop()` and stopped. control.Controller.Close
// is the ONLY thing that closes BuildKit's HistoryDB, its WorkerController
// (hence the worker's metadata_v2.db) and its CacheStore, so every core we
// tore down leaked three bbolt handles for the daemon's lifetime.
func TestServerCoreClose_ClosesTheController(t *testing.T) {
	var closes int
	c := fakeCore()
	c.controller = closerFn(func() error { closes++; return nil })

	withinDeadline(t, 10*time.Second, "serverCore.close", c.close)

	if closes != 1 {
		t.Fatalf("controller.Close called %d times, want exactly 1 — without it the bbolt files under DataDir stay locked and Rebuild's quarantine rename can never succeed", closes)
	}

	// Idempotence must survive the addition: Rebuild closes the old core and
	// Server.Close closes the current one.
	c.close()
	c.close()
	if closes != 1 {
		t.Errorf("controller.Close called %d times after repeat close()s, want 1", closes)
	}
}

// TestServerCoreClose_SurvivesAControllerCloseError: teardown paths have
// nowhere to return an error to, so a failing controller must not panic or
// abort the rest of shutdown.
func TestServerCoreClose_SurvivesAControllerCloseError(t *testing.T) {
	c := fakeCore()
	c.controller = closerFn(func() error { return errors.New("history db already closed") })
	withinDeadline(t, 10*time.Second, "serverCore.close", c.close)
}

// TestServerCoreClose_ReleasesTheBoltHandles pins THE property the whole
// deadlock fix rests on: after close(), nothing under DataDir is still held
// open, so the directory can be renamed away and its bbolt files reopened.
//
// This is the test the old fakeCore()-based failure-path test structurally
// could not be. It uses real bbolt files, opened through the exact calls
// newCore and the worker make, and owned by the core's controller the way the
// real ones are.
//
// ALL THREE STORES, not just history.db. control.Controller.Close closes
// HistoryDB (<DataDir>/history.db), the WorkerController (hence
// <DataDir>/worker/metadata_v2.db) and the CacheStore (<DataDir>/cache.db) —
// and a partial fix that releases only some of them leaves the node in
// exactly the same unrecoverable state as no fix at all. A stub that opened
// one file could not tell the two apart; the worker's metadata_v2.db in
// particular is the handle the newWorkerController unwind bug leaked.
//
// Before the fix this test fails twice over: the rename fails outright on
// Windows ("Access is denied"), and the reopen blocks forever on the flock —
// which is exactly why the reopens are under a deadline rather than bare
// calls.
func TestServerCoreClose_ReleasesTheBoltHandles(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	workerDir := filepath.Join(dataDir, "worker")
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The same three opens the real core performs, on the same three paths.
	cacheDB, err := bboltcachestorage.NewStore(filepath.Join(dataDir, "cache.db"))
	if err != nil {
		t.Fatalf("open cache.db: %v", err)
	}
	historyDB, err := boltutil.Open(filepath.Join(dataDir, "history.db"), 0o600, nil)
	if err != nil {
		t.Fatalf("open history.db: %v", err)
	}
	workerDB, err := metadata.NewStore(filepath.Join(workerDir, "metadata_v2.db"))
	if err != nil {
		t.Fatalf("open worker/metadata_v2.db: %v", err)
	}

	c := fakeCore()
	// Mirrors control.Controller.Close's order: HistoryDB, WorkerController
	// (which closes the worker's metadata store), CacheStore.
	c.controller = closerFn(func() error {
		return errors.Join(historyDB.Close(), workerDB.Close(), cacheDB.Close())
	})
	withinDeadline(t, 10*time.Second, "serverCore.close", c.close)

	// 1. The Windows property: Rebuild's very first action after stopping
	//    the old core is os.Rename(DataDir, quarantine), and an open handle
	//    anywhere inside — at any depth — makes that impossible.
	quarantine := filepath.Join(parent, "quarantined")
	if err := os.Rename(dataDir, quarantine); err != nil {
		t.Fatalf("could not rename the data dir after close(): %v — a bbolt handle is still open somewhere under it, which is the deadlock's first domino", err)
	}

	// 2. The flock property, on every platform: every one of the three can be
	//    opened again. Under a deadline because the failure mode is an
	//    infinite 50ms retry loop inside bolt.Open, not an error return.
	for _, tc := range []struct {
		name string
		open func() error
	}{
		{"cache.db", func() error {
			s, err := bboltcachestorage.NewStore(filepath.Join(quarantine, "cache.db"))
			if err != nil {
				return err
			}
			return s.Close()
		}},
		{"history.db", func() error {
			db, err := boltutil.Open(filepath.Join(quarantine, "history.db"), 0o600, nil)
			if err != nil {
				return err
			}
			return db.Close()
		}},
		{"worker/metadata_v2.db", func() error {
			md, err := metadata.NewStore(filepath.Join(quarantine, "worker", "metadata_v2.db"))
			if err != nil {
				return err
			}
			return md.Close()
		}},
	} {
		reopened := make(chan error, 1)
		go func() { reopened <- tc.open() }()
		select {
		case err := <-reopened:
			if err != nil {
				t.Fatalf("reopening %s after close(): %v", tc.name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("reopening %s blocked — its flock was never released, so newCore would spin forever holding s.mu", tc.name)
		}
	}
}

// TestRebuild_ReinitCannotWedgeTheDaemon is the liveness regression test.
//
// The failure it guards: Rebuild takes s.mu, tears the old core down, and
// then calls newCore twice (once for the rebuild, once for the re-init
// against the restored store) STILL HOLDING s.mu. newCore bbolt-opens files
// with flock timeout 0, which bbolt treats as "retry forever". So a single
// leaked handle turned Rebuild into an infinite loop under the daemon's own
// lock: Client, Build, Prune and Close all block on it, forever. That is
// strictly worse than the pre-existing behaviour, which returned an error and
// left a node an operator could restart.
//
// Here newCore is replaced by one that never returns. Rebuild must still come
// back, and the daemon must still be able to shut down.
func TestRebuild_ReinitCannotWedgeTheDaemon(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Released at the end so the stuck inits do not outlive the test.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var inits int32
	s := &Server{
		cfg:                 Config{DataDir: dataDir, Log: discardLog()},
		core:                fakeCore(),
		coreTimeoutOverride: 50 * time.Millisecond,
		newCoreFn: func(ctx context.Context, _ Config) (*serverCore, error) {
			atomic.AddInt32(&inits, 1)
			// Stands in for bolt.Open spinning on a flock it will never
			// get: not cancellable, not ctx-aware, never returns.
			<-release
			return nil, errors.New("unreachable")
		},
	}

	var rebuildErr error
	withinDeadline(t, 10*time.Second, "Rebuild with a solver init that never returns", func() {
		rebuildErr = s.Rebuild(context.Background())
	})
	if rebuildErr == nil {
		t.Fatal("Rebuild reported success although the solver never came up")
	}
	if !errors.Is(rebuildErr, ErrCoreInitTimeout) {
		t.Errorf("Rebuild error = %v, want it to wrap ErrCoreInitTimeout so the log names the real problem", rebuildErr)
	}
	if got := atomic.LoadInt32(&inits); got < 1 {
		t.Errorf("newCore called %d times, want at least the rebuild attempt", got)
	}

	// The node is build-dead but diagnosable — the acceptable outcome.
	if s.core != nil {
		t.Error("a core was installed although no init ever completed")
	}
	if _, err := s.Client(context.Background()); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("Client = %v, want ErrStoreUnavailable", err)
	}

	// And, the part that matters most: the daemon can still stop. Before the
	// bound this blocked on s.mu forever, so the service could not be
	// restarted — the only recovery that works.
	withinDeadline(t, 10*time.Second, "Close after an abandoned re-init", func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// TestRebuild_QuarantinePathErrorStillAttemptsReinit: the branch where we
// could not even choose a quarantine path has NOT touched the store — it is
// strictly safer than the rename-failure branch below it, which does attempt
// a re-init. It used to strand the node build-dead anyway.
func TestRebuild_QuarantinePathErrorStillAttemptsReinit(t *testing.T) {
	// A filesystem root is the input quarantineDir refuses.
	s := &Server{
		cfg:  Config{DataDir: string(filepath.Separator), Log: discardLog()},
		core: fakeCore(),
		newCoreFn: func(context.Context, Config) (*serverCore, error) {
			return fakeCore(), nil
		},
	}

	err := s.Rebuild(context.Background())
	if err == nil {
		t.Fatal("Rebuild succeeded on an implausible data dir")
	}
	if s.core == nil {
		t.Fatal("no re-init was attempted after a quarantine-path error; the node is build-dead even though the store on disk was never touched")
	}
	if s.rebuildErr != nil {
		t.Errorf("rebuildErr = %v, want nil after a successful re-init", s.rebuildErr)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
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
