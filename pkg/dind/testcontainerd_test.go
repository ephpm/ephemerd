//go:build !darwin

package dind

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/client"
	containerdpkg "github.com/ephpm/ephemerd/pkg/containerd"
)

// containerd's prometheus metrics live in a process-global registry, so
// any second containerdpkg.New() in the same `go test` binary panics with
// "duplicate metrics collector registration attempted". Tests that need a
// real embedded containerd share this single instance via a sync.Once.
//
// The instance is created lazily on first call and torn down via a
// shutdown hook registered with the *first* test that uses it (subsequent
// callers reuse, no extra teardown). DataDir is a per-process temp dir
// shared across all callers — tests should put their state in distinct
// namespaces, not distinct data dirs.

var (
	sharedCtrdOnce sync.Once
	sharedCtrd     *containerdpkg.Server
	sharedCtrdErr  error
	sharedDataDir  string
)

func sharedTestContainerd(t *testing.T) *client.Client {
	t.Helper()
	sharedCtrdOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ephemerd-shared-ctrd-*")
		if err != nil {
			sharedCtrdErr = err
			return
		}
		sharedDataDir = dir

		log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		ctrd, err := containerdpkg.New(containerdpkg.Config{
			DataDir:    dir,
			SocketPath: testSocketPath(t),
			Log:        log.With("component", "shared-test-containerd"),
		})
		if err != nil {
			sharedCtrdErr = err
			return
		}
		sharedCtrd = ctrd
	})

	if sharedCtrdErr != nil {
		t.Skipf("embedded containerd unavailable in this env: %v", sharedCtrdErr)
	}
	if sharedCtrd == nil {
		t.Skip("embedded containerd not initialized")
	}
	return sharedCtrd.Client()
}

// TestMain ensures the shared containerd (if any was started) is stopped
// before the test binary exits, so its bbolt meta.db is unlocked and the
// temp dir can be removed.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedCtrd != nil {
		sharedCtrd.Stop()
	}
	if sharedDataDir != "" {
		// Best-effort. On Windows containerd's meta.db can take a beat to
		// release after Stop() returns; if RemoveAll fails the temp dir
		// just gets cleaned by the OS later — not worth failing tests over.
		if err := os.RemoveAll(sharedDataDir); err != nil {
			// stderr because slog isn't worth wiring up here in TestMain.
			_, _ = os.Stderr.WriteString("test cleanup: remove " + sharedDataDir + ": " + err.Error() + "\n")
		}
	}
	os.Exit(code)
}
