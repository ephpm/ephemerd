package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDrainer is a scripted Drainer: ActiveJobs returns each value in
// counts in turn (then the last forever), so a test can simulate jobs
// finishing over successive polls.
type fakeDrainer struct {
	mu         sync.Mutex
	counts     []int
	idx        int
	cordoned   int32
	uncordoned int32
}

func (f *fakeDrainer) Cordon() int   { atomic.AddInt32(&f.cordoned, 1); return f.ActiveJobs() }
func (f *fakeDrainer) Uncordon() int { atomic.AddInt32(&f.uncordoned, 1); return 0 }
func (f *fakeDrainer) ActiveJobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.counts[f.idx]
	if f.idx < len(f.counts)-1 {
		f.idx++
	}
	return v
}

// fakeRelease serves an archive + checksums.txt for a given target/os/arch.
type fakeRelease struct {
	server    *httptest.Server
	assetName string
	archive   []byte
	checksums string
	assetHits int32
	sumHits   int32
}

func newFakeRelease(t *testing.T, target, goos, goarch string, binContent []byte, corrupt bool) *fakeRelease {
	t.Helper()
	asset := AssetName(target, goos, goarch)
	var archive []byte
	if goos == "windows" {
		archive = makeZip(t, "ephemerd.exe", binContent)
	} else {
		archive = makeTarGz(t, "ephemerd", binContent)
	}
	sum := sha256.Sum256(archive)
	sumHex := hex.EncodeToString(sum[:])
	if corrupt {
		sumHex = "00" + sumHex[2:] // valid-length but wrong digest
	}
	checksums := fmt.Sprintf("%s  %s\n", sumHex, asset)

	fr := &fakeRelease{assetName: asset, archive: archive, checksums: checksums}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fr.assetHits, 1)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/"+checksumsName, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fr.sumHits, 1)
		_, _ = w.Write([]byte(checksums))
	})
	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

// collectEmit gathers progress states in order (thread-safe).
func collectEmit() (Emit, func() []State) {
	var mu sync.Mutex
	var states []State
	emit := func(p Progress) {
		mu.Lock()
		states = append(states, p.State)
		mu.Unlock()
	}
	return emit, func() []State {
		mu.Lock()
		defer mu.Unlock()
		out := make([]State, len(states))
		copy(out, states)
		return out
	}
}

func containsState(states []State, want State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// setupInstall creates a fake "installed" binary and returns its path.
func setupInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "ephemerd"
	if runtime.GOOS == "windows" {
		name = "ephemerd.exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun_HappyPath(t *testing.T) {
	// Force a non-native GOOS so the --version probe (which would try to
	// exec the fake binary) is skipped; the swap/restart logic is what we
	// assert here.
	goos, goarch := "linux", "amd64"
	if runtime.GOOS == "linux" {
		goos = "windows" // pick any non-native to skip the probe branch
	}
	const target = "v0.1.7"
	newBin := []byte("NEW-BINARY-" + target)
	fr := newFakeRelease(t, target, goos, goarch, newBin, false)
	install := setupInstall(t)

	var restarted int32
	drainer := &fakeDrainer{counts: []int{2, 1, 0}}
	emit, states := collectEmit()

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.1.6",
		BaseURLOverride: fr.server.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		Restart:         func() error { atomic.AddInt32(&restarted, 1); return nil },
		RestartDelay:    time.Millisecond,
		Log:             nil,
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Binary was swapped and the old one preserved.
	got, _ := os.ReadFile(install)
	if string(got) != string(newBin) {
		t.Errorf("install binary = %q, want new binary", got)
	}
	old, err := os.ReadFile(install + ".old")
	if err != nil || string(old) != "OLD-BINARY" {
		t.Errorf(".old backup = %q err=%v, want OLD-BINARY", old, err)
	}

	// Restart fired (after the short delay).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&restarted) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&restarted) != 1 {
		t.Errorf("restart called %d times, want 1", restarted)
	}

	st := states()
	for _, want := range []State{StateDraining, StateDownloading, StateVerifying, StateStaging, StateSwapping, StateRestarting} {
		if !containsState(st, want) {
			t.Errorf("missing progress state %v in %v", want, st)
		}
	}
	if atomic.LoadInt32(&drainer.cordoned) != 1 {
		t.Errorf("cordoned %d times, want 1", drainer.cordoned)
	}
	if atomic.LoadInt32(&drainer.uncordoned) != 0 {
		t.Errorf("uncordoned %d times on success, want 0", drainer.uncordoned)
	}
}

func TestRun_ChecksumMismatchAborts(t *testing.T) {
	goos, goarch := "linux", "amd64"
	if runtime.GOOS == "linux" {
		goos = "windows"
	}
	const target = "v0.1.7"
	fr := newFakeRelease(t, target, goos, goarch, []byte("NEW"), true /*corrupt checksum*/)
	install := setupInstall(t)

	var restarted int32
	drainer := &fakeDrainer{counts: []int{0}}
	emit, states := collectEmit()

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.1.6",
		BaseURLOverride: fr.server.URL,
		Drainer:         drainer,
		DrainPoll:       time.Millisecond,
		InstallPath:     install,
		GOOS:            goos,
		GOARCH:          goarch,
		Restart:         func() error { atomic.AddInt32(&restarted, 1); return nil },
		RestartDelay:    time.Millisecond,
	}, emit)
	if err == nil {
		t.Fatal("expected error on checksum mismatch")
	}

	// The live binary must be UNTOUCHED and no restart scheduled.
	got, _ := os.ReadFile(install)
	if string(got) != "OLD-BINARY" {
		t.Errorf("install binary was modified on checksum failure: %q", got)
	}
	if _, err := os.Stat(install + ".old"); err == nil {
		t.Error(".old backup created despite aborted swap")
	}
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&restarted) != 0 {
		t.Errorf("restart fired %d times on failure, want 0", restarted)
	}
	// Aborting after cordon must uncordon so the node keeps claiming.
	if atomic.LoadInt32(&drainer.uncordoned) != 1 {
		t.Errorf("uncordoned %d times, want 1 (degrade safely)", drainer.uncordoned)
	}
	if !containsState(states(), StateFailed) {
		t.Error("expected a FAILED progress state")
	}
}

func TestRun_UpToDateNoOp(t *testing.T) {
	install := setupInstall(t)
	var restarted int32
	drainer := &fakeDrainer{counts: []int{0}}
	emit, states := collectEmit()

	err := Run(context.Background(), RunOptions{
		TargetVersion:  "v0.1.7",
		CurrentVersion: "v0.1.7",
		Drainer:        drainer,
		InstallPath:    install,
		Restart:        func() error { atomic.AddInt32(&restarted, 1); return nil },
	}, emit)
	if err != nil {
		t.Fatalf("Run up-to-date: %v", err)
	}
	if !containsState(states(), StateUpToDate) {
		t.Errorf("expected UP_TO_DATE, got %v", states())
	}
	if atomic.LoadInt32(&drainer.cordoned) != 0 {
		t.Error("up-to-date no-op should not cordon")
	}
	if atomic.LoadInt32(&restarted) != 0 {
		t.Error("up-to-date no-op should not restart")
	}
	if got, _ := os.ReadFile(install); string(got) != "OLD-BINARY" {
		t.Error("up-to-date no-op modified the binary")
	}
}

func TestRun_RejectsNonTagWithoutOverride(t *testing.T) {
	err := Run(context.Background(), RunOptions{
		TargetVersion:  "latest",
		CurrentVersion: "v0.1.6",
		InstallPath:    setupInstall(t),
	}, nil)
	if err == nil {
		t.Fatal("expected error: 'latest' is not a release tag and no url override given")
	}
}

func TestRun_ProbeMismatchAborts(t *testing.T) {
	// Use the NATIVE goos/arch so the probe branch runs, and inject a probe
	// that reports the wrong version — the swap must be refused.
	const target = "v0.1.7"
	fr := newFakeRelease(t, target, runtime.GOOS, runtime.GOARCH, []byte("NEW"), false)
	install := setupInstall(t)
	var restarted int32

	err := Run(context.Background(), RunOptions{
		TargetVersion:   target,
		CurrentVersion:  "v0.1.6",
		BaseURLOverride: fr.server.URL,
		NoDrain:         true,
		InstallPath:     install,
		Probe:           func(string) (string, error) { return "v0.0.1", nil },
		Restart:         func() error { atomic.AddInt32(&restarted, 1); return nil },
		RestartDelay:    time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("expected error when staged binary probes to the wrong version")
	}
	if got, _ := os.ReadFile(install); string(got) != "OLD-BINARY" {
		t.Errorf("binary swapped despite probe mismatch: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&restarted) != 0 {
		t.Error("restart fired despite probe mismatch")
	}
}
