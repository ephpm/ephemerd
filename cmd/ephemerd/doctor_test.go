package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDecideDoctorCleanup_RefusesWhileDaemonRuns pins the guard that keeps
// `ephemerd doctor` from deleting the control socket, the PID file and the
// running Hyper-V VMs out from under a live daemon. This is the whole point
// of the guard: a bare `ephemerd doctor` on a busy node must clean nothing.
func TestDecideDoctorCleanup_RefusesWhileDaemonRuns(t *testing.T) {
	got := decideDoctorCleanup(true, false)
	if got.Run {
		t.Fatal("cleanup must not run while the daemon is running")
	}
	if got.Reason == "" {
		t.Error("a refusal must explain itself")
	}
	// The operator needs to be told what to do next, not just told no.
	if !strings.Contains(got.Reason, "ephemerd stop") {
		t.Errorf("reason should tell the operator to stop the daemon, got %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "--force") {
		t.Errorf("reason should mention the --force override, got %q", got.Reason)
	}
}

// TestDecideDoctorCleanup_RunsWhenDaemonStopped verifies the normal path is
// untouched: with no daemon, cleanup is exactly what doctor is for.
func TestDecideDoctorCleanup_RunsWhenDaemonStopped(t *testing.T) {
	got := decideDoctorCleanup(false, false)
	if !got.Run {
		t.Fatalf("cleanup must run when the daemon is stopped (reason: %q)", got.Reason)
	}
}

// TestDecideDoctorCleanup_ForceOverrides verifies --force is a real escape
// hatch — an operator who knows the daemon is wedged can still clean.
func TestDecideDoctorCleanup_ForceOverrides(t *testing.T) {
	if got := decideDoctorCleanup(true, true); !got.Run {
		t.Fatal("--force must allow cleanup even with the daemon running")
	}
	if got := decideDoctorCleanup(false, true); !got.Run {
		t.Fatal("--force must not block cleanup when the daemon is stopped")
	}
}

// TestPrivilegeHint_NoSudoAdviceOnWindows covers the bug where os.Geteuid()
// returns -1 on Windows, so the != 0 test always fired and every Windows run
// printed "run with sudo".
func TestPrivilegeHint_NoSudoAdviceOnWindows(t *testing.T) {
	hint := privilegeHint("windows", -1)
	if strings.Contains(hint, "sudo") {
		t.Errorf("Windows hint must not mention sudo, got %q", hint)
	}
	if hint == "" {
		t.Error("Windows should still get an elevation hint")
	}
}

func TestPrivilegeHint_Posix(t *testing.T) {
	if hint := privilegeHint("linux", 0); hint != "" {
		t.Errorf("root needs no hint, got %q", hint)
	}
	if hint := privilegeHint("linux", 1000); !strings.Contains(hint, "sudo") {
		t.Errorf("non-root should be told to use sudo, got %q", hint)
	}
	if hint := privilegeHint("darwin", 501); !strings.Contains(hint, "sudo") {
		t.Errorf("non-root darwin should be told to use sudo, got %q", hint)
	}
}

// TestPlatformCleanupPrompt_NamesTheDamage checks the confirmation actually
// says what will be destroyed. A prompt that just asks "are you sure?" is a
// prompt operators learn to answer y to.
func TestPlatformCleanupPrompt_NamesTheDamage(t *testing.T) {
	dataDir := filepath.Join("C:", "ProgramData", "ephemerd")
	prompt := platformCleanupPrompt("windows", dataDir)
	for _, want := range []string{"Hyper-V", "WSL", filepath.Join(dataDir, "vm"), "[y/N]"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("windows prompt missing %q:\n%s", want, prompt)
		}
	}

	for _, goos := range []string{"linux", "darwin"} {
		p := platformCleanupPrompt(goos, "/var/lib/ephemerd")
		if !strings.Contains(p, "[y/N]") {
			t.Errorf("%s prompt is not a y/N question:\n%s", goos, p)
		}
		if strings.Contains(p, "Hyper-V") {
			t.Errorf("%s prompt should not mention Hyper-V:\n%s", goos, p)
		}
	}
}
