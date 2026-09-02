package buildkit

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// wedgedCore returns a serverCore whose gRPC server is REALLY serving and has
// a REALLY in-flight RPC handler that ignores its stream context — the shape
// of a BuildKit Solve whose build step is stuck in containerdexecutor's
// `p.Wait(context.Background())` after its containerd shim died.
//
// Nothing about this core can be stopped: grpc.Server.stop ends in
// handlersWG.Wait(), which this handler will never let complete.
func wedgedCore(t *testing.T) *serverCore {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()

	started := make(chan struct{})
	var startOnce sync.Once
	// Released only at the very end of the test, so the handler is wedged for
	// the whole of every assertion below.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "ephemerd.test.Wedge",
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Hang",
			ServerStreams: true,
			ClientStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				startOnce.Do(func() { close(started) })
				// THE POINT OF THE TEST: stream.Context() is never
				// consulted, so cancelling it (which is all Stop() can
				// do) changes nothing.
				<-release
				return nil
			},
		}},
	}, nil)

	serving := make(chan struct{})
	go func() {
		defer close(serving)
		_ = srv.Serve(lis)
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial the in-process server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	streamCtx, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(cancelStream)
	if _, err := conn.NewStream(streamCtx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true},
		"/ephemerd.test.Wedge/Hang"); err != nil {
		t.Fatalf("open the wedging stream: %v", err)
	}

	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the ctx-ignoring handler never ran; this test would prove nothing")
	}

	return &serverCore{
		grpcServ: srv,
		bufnet:   lis,
		stop:     make(chan struct{}),
		log:      discardLog(),
		// Short so the escalation ladder is exercised in a test, not in 25s.
		graceTimeout:    150 * time.Millisecond,
		hardStopTimeout: 150 * time.Millisecond,
	}
}

// TestServerCoreClose_ReturnsWhenAnRPCHandlerWillNotStop is the regression
// test for the second wedge: the "bounded" GracefulStop that was not bounded.
//
// The old escalation was:
//
//	case <-time.After(coreCloseGraceTimeout):
//	    c.grpcServ.Stop()
//	    <-stopped        // <-- unbounded
//
// Stop() cancels every stream's context but then joins the SAME
// handlersWG.Wait() the GracefulStop is parked on, so a handler that ignores
// its context keeps GracefulStop blocked forever and `<-stopped` never
// returns. close() runs with s.mu held (Rebuild, Server.Close) and inside
// closeOnce, so that one stuck build step took out builds, prunes and
// shutdown for the whole daemon — the exact wedge class this branch removes,
// relocated into the code that removes it.
//
// close() must now RETURN. Under a deadline, so a regression fails with this
// message instead of hanging CI.
func TestServerCoreClose_ReturnsWhenAnRPCHandlerWillNotStop(t *testing.T) {
	c := wedgedCore(t)

	var controllerCloses int32
	c.controller = closerFn(func() error {
		atomic.AddInt32(&controllerCloses, 1)
		return nil
	})

	withinDeadline(t, 30*time.Second, "serverCore.close with an RPC handler that ignores its context", c.close)

	if !c.closeAbandoned() {
		t.Fatal("close() returned without recording that it abandoned the teardown; the leaked bbolt handles would be invisible to the operator")
	}

	// The deliberate leak. controller.Close closes cache.db, history.db and
	// worker/metadata_v2.db; doing that while a Solve handler is still running
	// against them is a use-after-close of a live bbolt DB, not a tidy-up.
	if got := atomic.LoadInt32(&controllerCloses); got != 0 {
		t.Fatalf("controller.Close ran %d times although an RPC handler is still live — that closes bbolt out from under a running goroutine", got)
	}
}

// TestServerClose_ReturnsWhenAnRPCHandlerWillNotStop: the whole point of
// bounding close() is that the DAEMON stays alive. Server.Close takes s.mu, so
// if close() blocks the service can never stop and the SCM kills it mid
// teardown.
func TestServerClose_ReturnsWhenAnRPCHandlerWillNotStop(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir(), Log: discardLog()}, core: wedgedCore(t)}

	withinDeadline(t, 30*time.Second, "Server.Close with a wedged RPC handler", func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// And the lock really is free afterwards.
	withinDeadline(t, 10*time.Second, "Client after Close", func() {
		if _, err := s.Client(context.Background()); !errors.Is(err, ErrServerClosed) {
			t.Errorf("Client after Close = %v, want ErrServerClosed", err)
		}
	})
}

// TestRebuild_ReturnsWhenTheOldCoreCannotBeStopped: Rebuild closes the old
// core while holding the write lock. A close() that blocks there is the worst
// case of all — the heal ladder calls Rebuild, so a single stuck build step
// would wedge the daemon through the very path meant to repair it.
//
// Rebuild is expected to FAIL here (there is no containerd), but it must fail
// promptly rather than hang, and the daemon must still shut down.
func TestRebuild_ReturnsWhenTheOldCoreCannotBeStopped(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "buildkit")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		cfg:                 Config{DataDir: dataDir, Log: discardLog()},
		core:                wedgedCore(t),
		coreTimeoutOverride: 100 * time.Millisecond,
		newCoreFn: func(context.Context, Config) (*serverCore, error) {
			return nil, errors.New("containerd unreachable")
		},
	}

	withinDeadline(t, 60*time.Second, "Rebuild whose old core cannot be stopped", func() {
		if err := s.Rebuild(context.Background()); err == nil {
			t.Error("Rebuild reported success with no containerd")
		}
	})

	withinDeadline(t, 30*time.Second, "Close after a Rebuild that abandoned its old core", func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// TestServerCoreClose_StillClosesTheControllerWhenTheServerStops guards the
// other direction: the escalation must not become an excuse to skip
// controller.Close on the NORMAL path. A never-served grpc.Server stops
// immediately, so the controller has to be closed.
func TestServerCoreClose_StillClosesTheControllerWhenTheServerStops(t *testing.T) {
	c := fakeCore()
	c.graceTimeout = 150 * time.Millisecond
	c.hardStopTimeout = 150 * time.Millisecond

	var closes int32
	c.controller = closerFn(func() error { atomic.AddInt32(&closes, 1); return nil })

	withinDeadline(t, 10*time.Second, "serverCore.close", c.close)

	if got := atomic.LoadInt32(&closes); got != 1 {
		t.Fatalf("controller.Close called %d times, want 1", got)
	}
	if c.closeAbandoned() {
		t.Error("a clean teardown was reported as abandoned")
	}
}

// TestBuildCore_AdoptsALateCore pins the reaper's new behaviour: an init that
// finished just after the bound expired produces a WORKING solver, and
// throwing it away leaves the node build-dead until a restart for no reason.
// coreInitTimeout is an unmeasured guess; adopting is what makes the guess
// cheap to be wrong about.
func TestBuildCore_AdoptsALateCore(t *testing.T) {
	release := make(chan struct{})
	late := fakeCore()

	s := &Server{
		cfg:                 Config{DataDir: t.TempDir(), Log: discardLog()},
		coreTimeoutOverride: 50 * time.Millisecond,
		newCoreFn: func(context.Context, Config) (*serverCore, error) {
			<-release
			return late, nil
		},
	}

	var err error
	withinDeadline(t, 10*time.Second, "buildCore with a slow init", func() {
		// Both real callers hold the write lock; hold it here too so the
		// dataGen read is as race-free in the test as it is in production.
		s.mu.Lock()
		_, err = s.buildCore(context.Background())
		s.mu.Unlock()
	})
	if !errors.Is(err, ErrCoreInitTimeout) {
		t.Fatalf("buildCore = %v, want ErrCoreInitTimeout", err)
	}

	close(release)

	if got := waitForCore(t, s, 10*time.Second); got != late {
		t.Fatalf("late core was not installed (s.core = %p, want %p) — the node would stay build-dead until a restart despite a working solver existing", got, late)
	}
	if s.rebuildErr != nil {
		t.Errorf("rebuildErr = %v, want it cleared by the adoption", s.rebuildErr)
	}
}

// TestBuildCore_DiscardsALateCoreWhoseStoreMoved is the safety half of the
// adoption. If Rebuild has renamed or cleared DataDir since the init started,
// the late core is bound to a store that is no longer at that path — on Linux
// its bbolt files are unlinked. Installing it would give the node a solver
// writing into nothing. It must be CLOSED instead, which is also what stops it
// leaking the handles that made us abandon it.
func TestBuildCore_DiscardsALateCoreWhoseStoreMoved(t *testing.T) {
	release := make(chan struct{})
	late := fakeCore()

	s := &Server{
		cfg:                 Config{DataDir: t.TempDir(), Log: discardLog()},
		coreTimeoutOverride: 50 * time.Millisecond,
		newCoreFn: func(context.Context, Config) (*serverCore, error) {
			<-release
			return late, nil
		},
	}

	var err error
	withinDeadline(t, 10*time.Second, "buildCore with a slow init", func() {
		s.mu.Lock()
		_, err = s.buildCore(context.Background())
		s.mu.Unlock()
	})
	if !errors.Is(err, ErrCoreInitTimeout) {
		t.Fatalf("buildCore = %v, want ErrCoreInitTimeout", err)
	}

	// Stands in for Rebuild's quarantine rename / restore.
	s.mu.Lock()
	s.dataGen++
	s.mu.Unlock()

	close(release)

	// The core must be closed (close(c.stop) is the observable), and never
	// installed.
	select {
	case <-late.stop:
	case <-time.After(10 * time.Second):
		t.Fatal("the discarded late core was never closed — its bbolt handles under the data dir leak for the daemon's lifetime")
	}
	s.mu.RLock()
	installed := s.core
	s.mu.RUnlock()
	if installed != nil {
		t.Fatal("a late core was installed although DataDir had been moved under it")
	}
}

// TestBuildCore_DiscardsALateCoreAfterClose: the heal ladder can be mid-init
// when the daemon shuts down. Installing a core into a closed Server would
// leak a gRPC server and three bbolt handles past shutdown.
func TestBuildCore_DiscardsALateCoreAfterClose(t *testing.T) {
	release := make(chan struct{})
	late := fakeCore()

	s := &Server{
		cfg:                 Config{DataDir: t.TempDir(), Log: discardLog()},
		coreTimeoutOverride: 50 * time.Millisecond,
		newCoreFn: func(context.Context, Config) (*serverCore, error) {
			<-release
			return late, nil
		},
	}

	var err error
	withinDeadline(t, 10*time.Second, "buildCore with a slow init", func() {
		s.mu.Lock()
		_, err = s.buildCore(context.Background())
		s.mu.Unlock()
	})
	if !errors.Is(err, ErrCoreInitTimeout) {
		t.Fatalf("buildCore = %v, want ErrCoreInitTimeout", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)

	select {
	case <-late.stop:
	case <-time.After(10 * time.Second):
		t.Fatal("a late core arriving after Close was not closed")
	}
	s.mu.RLock()
	installed := s.core
	s.mu.RUnlock()
	if installed != nil {
		t.Fatal("a late core was installed into a closed Server")
	}
}

// waitForCore polls for an installed core. Deadline-guarded: the failure being
// tested for is "it never arrives", and a bare block would report a CI timeout
// with no attribution.
func waitForCore(t *testing.T, s *Server, d time.Duration) *serverCore {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		s.mu.RLock()
		c := s.core
		s.mu.RUnlock()
		if c != nil {
			return c
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}
