//go:build windows

package tunnel

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// alive reports whether pid still exists. OpenProcess-based checks are the
// reliable way on Windows; os.FindProcess always succeeds there.
func alive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"if (Get-Process -Id "+itoa(pid)+" -ErrorAction SilentlyContinue) { 'yes' } else { 'no' }").Output()
	if err != nil {
		t.Fatalf("probing pid %d: %v", pid, err)
	}
	return string(out[:3]) == "yes"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The whole point of the Job Object: releasing it kills the child, with no
// cooperation from the child and no signal — because Windows has no SIGTERM
// for os.Process.Signal, which is exactly why cloudflared leaked one process
// per ephemerd restart until 2026-09-22 (25 orphans, oldest 13 days).
func TestBindChildLifetimeKillsChildOnRelease(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	closer, err := bindChildLifetime(cmd)
	if err != nil {
		t.Fatalf("bindChildLifetime: %v", err)
	}
	if closer == nil {
		t.Fatal("bindChildLifetime returned a nil closer on windows; the child is unbound")
	}
	if !alive(t, pid) {
		t.Fatal("child died before the job was released; test proves nothing")
	}

	if err := closer.Close(); err != nil {
		t.Fatalf("closing job: %v", err)
	}

	// KILL_ON_JOB_CLOSE is synchronous-ish but not instantaneous.
	deadline := time.Now().Add(10 * time.Second)
	for alive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatal("child survived the job handle closing: the lifetime binding does not bind")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Double close must not blow up — close() releases it and the deferred
	// path can run again on a reused Cloudflared.
	if err := closer.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

// A process that has not started cannot be assigned to a job. Returning an
// error (rather than panicking on a nil cmd.Process) keeps Listen's failure
// path a warning instead of a crash.
func TestBindChildLifetimeRejectsUnstartedProcess(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "exit")
	closer, err := bindChildLifetime(cmd)
	if err == nil {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatal("bindChildLifetime(unstarted) = nil error, want an error")
	}
	if closer != nil {
		t.Error("bindChildLifetime returned both an error and a closer")
	}
	if errors.Is(err, os.ErrClosed) {
		t.Errorf("unexpected error kind: %v", err)
	}
}
