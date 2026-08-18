package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// nonNativeGOOS picks a GOOS that is not the one the test binary runs on, so
// Run skips the `--version` probe of the staged (fake) binary. Mirrors the
// trick the existing run_test.go cases use.
func nonNativeGOOS() string {
	if runtime.GOOS == "linux" {
		return "windows"
	}
	return "linux"
}

// hangingRelease serves an asset that writes a few bytes and then never
// writes again, without closing the connection — the shape of a blackholed
// transfer. Before the stall timeout existed this parked io.Copy forever with
// the scheduler cordoned, which is the only way a live daemon could stay
// drained on the old binary.
func hangingRelease(t *testing.T, target, goos, goarch string) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mux := http.NewServeMux()
	mux.HandleFunc("/"+AssetName(target, goos, goarch), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("PARTIAL"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// trickleRelease serves an asset one byte at a time forever. It never stalls,
// so only the whole-phase install budget can stop it.
func trickleRelease(t *testing.T, target, goos, goarch string, every time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+AssetName(target, goos, goarch), func(w http.ResponseWriter, r *http.Request) {
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRun_StalledDownloadUncordons is the regression for the one cordon leak
// the error/panic/restart-watchdog paths could not cover: an upgrade that
// neither succeeds nor fails. The node must end up serving jobs again on its
// old binary, not silently drained.
func TestRun_StalledDownloadUncordons(t *testing.T) {
	const target = "v0.2.3"
	goos, goarch := nonNativeGOOS(), "amd64"
	srv := hangingRelease(t, target, goos, goarch)
	install := setupInstall(t)
	drainer := &fakeDrainer{counts: []int{0}}

	start := time.Now()
	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.2.2",
		BaseURLOverride: srv.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		StallTimeout:    150 * time.Millisecond,
		Restart:         func() error { t.Error("restart must not be attempted after a stalled download"); return nil },
	}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run returned nil for a download that never completed")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %v, want it to name the stall", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s to give up; the stall timeout did not fire", elapsed)
	}
	if !drainer.wasCordoned() {
		t.Error("upgrade never cordoned, so this is not exercising the leak")
	}
	if got := drainer.uncordonCount(); got != 1 {
		t.Errorf("uncordoned %d times, want exactly 1 — a stalled upgrade must put the node back into service", got)
	}
	if got, _ := os.ReadFile(install); string(got) != "OLD-BINARY" {
		t.Errorf("install binary = %q, want the untouched old binary", got)
	}
}

// TestRun_InstallBudgetUncordons covers the backstop: a transfer that keeps
// dribbling bytes never trips the stall timeout, so the cordon has to be
// bounded by the phase budget instead.
func TestRun_InstallBudgetUncordons(t *testing.T) {
	const target = "v0.2.3"
	goos, goarch := nonNativeGOOS(), "amd64"
	srv := trickleRelease(t, target, goos, goarch, 5*time.Millisecond)
	install := setupInstall(t)
	drainer := &fakeDrainer{counts: []int{0}}

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.2.2",
		BaseURLOverride: srv.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		StallTimeout:    time.Minute, // never fires: bytes keep arriving
		InstallTimeout:  250 * time.Millisecond,
		Restart:         func() error { t.Error("restart must not be attempted"); return nil },
	}, nil)

	if err == nil {
		t.Fatal("Run returned nil despite exceeding the install budget")
	}
	if !strings.Contains(err.Error(), "install budget") {
		t.Errorf("error = %v, want it to name the install budget", err)
	}
	if got := drainer.uncordonCount(); got != 1 {
		t.Errorf("uncordoned %d times, want exactly 1", got)
	}
}

// TestRun_DrainTimeoutStillUncordons pins the behavior the incident review
// assumed but nothing asserted: jobs that never finish must not leave the
// cordon behind either.
func TestRun_DrainTimeoutStillUncordons(t *testing.T) {
	const target = "v0.2.3"
	goos, goarch := nonNativeGOOS(), "amd64"
	fr := newFakeRelease(t, target, goos, goarch, []byte("NEW"), false)
	drainer := &fakeDrainer{counts: []int{1}} // a job that never finishes

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.2.2",
		BaseURLOverride: fr.server.URL,
		Drainer:         drainer,
		DrainTimeout:    20 * time.Millisecond,
		DrainPoll:       time.Millisecond,
		InstallPath:     setupInstall(t),
		GOOS:            goos,
		GOARCH:          goarch,
	}, nil)
	if err == nil {
		t.Fatal("Run returned nil despite the drain never reaching idle")
	}
	if got := drainer.uncordonCount(); got != 1 {
		t.Errorf("uncordoned %d times, want exactly 1", got)
	}
}

// TestRun_ExpiredBudgetNeverSwaps guards the gap between what the watchdog
// logs and what the daemon does. Checksum verify, extract and probe take no
// context, so a budget that expires during them cannot interrupt the step —
// but it must still stop the upgrade at the next phase boundary. Otherwise
// the daemon logs "install phase exceeded its budget; aborting" and then goes
// on to swap the binary and restart, which is worse than either outcome alone.
func TestRun_ExpiredBudgetNeverSwaps(t *testing.T) {
	const target = "v0.2.3"
	// Native GOOS/GOARCH so Run takes the probe branch at all.
	goos, goarch := runtime.GOOS, runtime.GOARCH
	fr := newFakeRelease(t, target, goos, goarch, []byte("NEW-BINARY"), false)
	install := setupInstall(t)
	drainer := &fakeDrainer{counts: []int{0}}

	// Burn the budget inside the PROBE, which takes no context and so cannot
	// be interrupted. The download and verify both succeed; the only thing
	// that can stop the swap is the boundary check.
	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.2.2",
		BaseURLOverride: fr.server.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		InstallTimeout:  50 * time.Millisecond,
		Probe: func(string) (string, error) {
			time.Sleep(250 * time.Millisecond) // outlives the budget, uninterruptible
			return target, nil
		},
		Restart: func() error { t.Error("restart must not run after the budget expired"); return nil },
	}, nil)

	if err == nil {
		t.Fatal("Run returned nil despite an expired install budget")
	}
	if got, _ := os.ReadFile(install); string(got) != "OLD-BINARY" {
		t.Errorf("install binary = %q, want it untouched — the swap ran after the abort", got)
	}
	if _, statErr := os.Stat(install + ".old"); statErr == nil {
		t.Error("a .old backup exists, so the swap ran despite the abort")
	}
	if got := drainer.uncordonCount(); got != 1 {
		t.Errorf("uncordoned %d times, want exactly 1", got)
	}
}

func TestDownloadFile_StallTimeoutFires(t *testing.T) {
	srv := hangingRelease(t, "v0.0.1", "linux", "amd64")
	dest := filepath.Join(t.TempDir(), "asset")

	start := time.Now()
	err := downloadFile(context.Background(), http.DefaultClient,
		srv.URL+"/"+AssetName("v0.0.1", "linux", "amd64"), dest, 100*time.Millisecond, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("downloadFile returned nil for a stalled transfer")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %v, want it to name the stall", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("gave up after %s, want ~100ms", elapsed)
	}
}

// TestDownloadFile_SlowButProgressingSucceeds guards the other direction: a
// genuinely slow link must not be mistaken for a stall, because a ~1 GB asset
// over a bad connection is a legitimate upgrade, not a failure.
func TestDownloadFile_SlowButProgressingSucceeds(t *testing.T) {
	body := []byte("0123456789")
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		for _, b := range body {
			if _, err := w.Write([]byte{b}); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "asset")
	// Total transfer (~100ms) far exceeds the per-read stall budget (50ms),
	// so this only passes if the watchdog resets on every read.
	if err := downloadFile(context.Background(), http.DefaultClient, srv.URL+"/slow", dest, 50*time.Millisecond, nil); err != nil {
		t.Fatalf("downloadFile on a slow-but-live transfer: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
}
