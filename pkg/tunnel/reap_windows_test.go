//go:build windows

package tunnel

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stagePowershellCopy copies powershell.exe into a temp dir so the test has a
// binary it can match by PATH without touching the real cloudflared, and
// without killing the system's own powershell processes. The reaper matches on
// full image path, which is exactly what makes that safe.
func stagePowershellCopy(t *testing.T, name string) string {
	t.Helper()
	src, err := exec.LookPath("powershell")
	if err != nil {
		t.Skipf("powershell not on PATH: %v", err)
	}
	in, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("cannot read powershell: %v", err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, in, 0o755); err != nil {
		t.Fatalf("staging copy: %v", err)
	}
	return dst
}

// spawnSleeper starts a child of THIS process — used only to prove the reaper
// spares its own child.
func spawnSleeper(t *testing.T, exe string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(exe, "-NoProfile", "-Command", "Start-Sleep -Seconds 120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn %s: %v", exe, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

// spawnOrphan starts exe via a launcher that immediately exits, so the
// surviving process's parent is DEAD — which is what a real stray is. Spawning
// it directly would make it our own child, and the reaper deliberately spares
// those, so the obvious test setup silently tests nothing.
func spawnOrphan(t *testing.T, exe string) int {
	t.Helper()
	launch := exec.Command("powershell", "-NoProfile", "-Command",
		"(Start-Process -FilePath '"+exe+"' -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 120' -PassThru).Id")
	out, err := launch.Output()
	if err != nil {
		t.Fatalf("launching orphan: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid); err != nil || pid == 0 {
		t.Fatalf("could not parse orphan pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = killPID(uint32(pid)) })

	// The launcher must be gone, or the orphan still has a live parent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := parentPID(uint32(pid)); p == 0 || p != uint32(launch.Process.Pid) {
			break
		}
		if _, ok := processImagePath(uint32(launch.Process.Pid)); !ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if parentPID(uint32(pid)) == uint32(os.Getpid()) {
		t.Fatalf("orphan %d is parented to the test process; setup is wrong", pid)
	}
	return pid
}

func waitGone(t *testing.T, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, ok := processImagePath(uint32(pid)); !ok {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// A stray from a previous ephemerd must die. This is the case that leaves 25
// orphans holding the tunnel's edge connections when it does not work.
func TestReapKillsStrayByImagePath(t *testing.T) {
	exe := stagePowershellCopy(t, "cloudflared.exe")
	stray := spawnOrphan(t, exe)

	reapStrayCloudflared(exe, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !waitGone(t, stray, 10*time.Second) {
		t.Fatalf("stray pid %d survived the reap", stray)
	}
}

// Matching is by IMAGE PATH, not process name. An operator running their own
// cloudflared for an unrelated tunnel must not be killed by ephemerd starting
// up — that would be a far worse bug than the leak this fixes.
func TestReapSparesUnrelatedCloudflared(t *testing.T) {
	ours := stagePowershellCopy(t, "cloudflared.exe")
	theirs := stagePowershellCopy(t, "cloudflared.exe") // same NAME, different dir
	if ours == theirs {
		t.Fatal("test staged both copies at the same path")
	}

	outsider := spawnOrphan(t, theirs)
	victim := spawnOrphan(t, ours)

	reapStrayCloudflared(ours, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !waitGone(t, victim, 10*time.Second) {
		t.Errorf("our own stray pid %d survived", victim)
	}
	if _, ok := processImagePath(uint32(outsider)); !ok {
		t.Errorf("killed an unrelated cloudflared at %s — matching is not path-scoped", theirs)
	}
}

// Our own live child must survive a sweep. start() calls the reaper before
// spawning, but a restart path can have one running, and killing it would turn
// a routine restart into a tunnel outage.
func TestReapSparesOwnChild(t *testing.T) {
	exe := stagePowershellCopy(t, "cloudflared.exe")
	child := spawnSleeper(t, exe) // parented to this test process

	reapStrayCloudflared(exe, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, ok := processImagePath(uint32(child.Process.Pid)); !ok {
		t.Error("reaper killed its own child; a restart would drop the live tunnel")
	}
	if parentPID(uint32(child.Process.Pid)) != uint32(os.Getpid()) {
		t.Skip("child was reparented; the spare-own-child assertion is not meaningful here")
	}
}

// Nothing to reap must be silent and harmless — this runs on every tunnel
// start, including the overwhelming majority where the previous shutdown was
// clean.
func TestReapNoStraysIsHarmless(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "cloudflared.exe")
	reapStrayCloudflared(exe, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
