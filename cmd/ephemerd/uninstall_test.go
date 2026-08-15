package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUninstallPlan_WarnsAboutDataDeletion pins the confirmation text for the
// destructive default: `ephemerd uninstall` (no --keep-data) removes the
// whole data directory, and the operator must be shown that path before
// answering.
func TestUninstallPlan_WarnsAboutDataDeletion(t *testing.T) {
	dataDir := filepath.Join("var", "lib", "ephemerd")
	plan := uninstallPlan("linux", dataDir, false)

	if !strings.Contains(plan, dataDir) {
		t.Errorf("plan must name the data dir being deleted:\n%s", plan)
	}
	if !strings.Contains(plan, "DELETE") {
		t.Errorf("plan must say the data dir is deleted:\n%s", plan)
	}
	if !strings.Contains(plan, "not recoverable") {
		t.Errorf("plan should say the deletion is irreversible:\n%s", plan)
	}
}

// TestUninstallPlan_KeepDataDoesNotThreatenTheDataDir verifies --keep-data
// semantics are reflected in the prompt: nothing may claim the data dir is
// going away when it is not.
func TestUninstallPlan_KeepDataDoesNotThreatenTheDataDir(t *testing.T) {
	plan := uninstallPlan("linux", "/var/lib/ephemerd", true)
	if strings.Contains(plan, "DELETE the entire data directory") {
		t.Errorf("--keep-data must not threaten the data dir:\n%s", plan)
	}
	if !strings.Contains(plan, "KEEP the data directory") {
		t.Errorf("--keep-data should say the data dir is kept:\n%s", plan)
	}
}

// TestUninstallPlan_WindowsNamesVMs verifies Windows operators are warned
// about the VM/WSL destruction, which happens even with --keep-data.
func TestUninstallPlan_WindowsNamesVMs(t *testing.T) {
	plan := uninstallPlan("windows", `C:\ProgramData\ephemerd`, true)
	if !strings.Contains(plan, "Hyper-V") || !strings.Contains(plan, "WSL") {
		t.Errorf("windows plan must mention the VMs and WSL distros it deletes:\n%s", plan)
	}
}

// withStdin swaps os.Stdin for a file containing input for the duration of
// the test, so the confirm() gate can be driven from a unit test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatalf("writing fake stdin: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fake stdin: %v", err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		if err := f.Close(); err != nil {
			t.Errorf("closing fake stdin: %v", err)
		}
	})
}

// TestConfirm_Answers covers the shared confirmation gate used by uninstall,
// doctor cleanup and cache clear. Only an explicit yes proceeds.
func TestConfirm_Answers(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"maybe\n", false},
		// No trailing newline and no input at all: a non-interactive shell
		// (script, cron, CI) must abort rather than proceed unattended.
		{"y", true},
		{"", false},
	}
	for _, tc := range cases {
		withStdin(t, tc.input)
		if got := confirm("Proceed? [y/N] "); got != tc.want {
			t.Errorf("confirm(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
