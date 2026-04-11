//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ephpm/ephemerd/pkg/vm"
	"github.com/ephpm/ephemerd/pkg/workflow"
)

// TestE2E_RunRouting_LinuxJob verifies that a workflow with runs-on: ubuntu-latest
// is detected as a Linux job and, on Windows, would be delegated to WSL.
func TestE2E_RunRouting_LinuxJob(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "linux.yml")
	if err := os.WriteFile(wfPath, []byte(`
name: Linux CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo "hello from linux"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := workflow.Parse(wfPath)
	if err != nil {
		t.Fatalf("parsing workflow: %v", err)
	}

	job := wf.Jobs["build"]
	platform := workflow.DetectPlatform(job.RunsOn)
	if platform != workflow.PlatformLinux {
		t.Fatalf("expected PlatformLinux, got %v", platform)
	}

	if runtime.GOOS == "windows" {
		// Verify path translation works for delegation
		wslPath, err := vm.WindowsPathToWSL(wfPath)
		if err != nil {
			t.Fatalf("WindowsPathToWSL(%q): %v", wfPath, err)
		}
		t.Logf("WSL path: %s", wslPath)
	}
}

// TestE2E_RunRouting_WindowsJob verifies that a workflow with runs-on: windows-latest
// is detected as a Windows job and would NOT be delegated to WSL.
func TestE2E_RunRouting_WindowsJob(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "windows.yml")
	if err := os.WriteFile(wfPath, []byte(`
name: Windows CI
on: push
jobs:
  build:
    runs-on: windows-latest
    steps:
      - run: echo "hello from windows"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := workflow.Parse(wfPath)
	if err != nil {
		t.Fatalf("parsing workflow: %v", err)
	}

	job := wf.Jobs["build"]
	platform := workflow.DetectPlatform(job.RunsOn)
	if platform != workflow.PlatformWindows {
		t.Fatalf("expected PlatformWindows, got %v", platform)
	}
}

// TestE2E_RunRouting_BothOSes verifies that a multi-job workflow with both
// Linux and Windows jobs is correctly routed per-job.
func TestE2E_RunRouting_BothOSes(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "multi-os.yml")
	if err := os.WriteFile(wfPath, []byte(`
name: Multi-OS CI
on: push
jobs:
  linux-build:
    runs-on: ubuntu-22.04
    steps:
      - run: uname -a
  windows-build:
    runs-on: windows-2022
    steps:
      - run: ver
  macos-build:
    runs-on: macos-14
    steps:
      - run: sw_vers
  self-hosted-linux:
    runs-on: [self-hosted, linux, x64]
    steps:
      - run: echo "self-hosted linux"
  self-hosted-bare:
    runs-on: [self-hosted]
    steps:
      - run: echo "self-hosted default"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, err := workflow.Parse(wfPath)
	if err != nil {
		t.Fatalf("parsing workflow: %v", err)
	}

	expected := map[string]workflow.TargetPlatform{
		"linux-build":       workflow.PlatformLinux,
		"windows-build":     workflow.PlatformWindows,
		"macos-build":       workflow.PlatformMacOS,
		"self-hosted-linux": workflow.PlatformLinux,
		"self-hosted-bare":  workflow.PlatformLinux, // default
	}

	for jobName, wantPlatform := range expected {
		job, ok := wf.Jobs[jobName]
		if !ok {
			t.Errorf("job %q not found in workflow", jobName)
			continue
		}
		got := workflow.DetectPlatform(job.RunsOn)
		if got != wantPlatform {
			t.Errorf("job %q: DetectPlatform = %v, want %v", jobName, got, wantPlatform)
		}
	}

	// On Windows, verify that the linux jobs would go through WSL path translation
	if runtime.GOOS == "windows" {
		for jobName, plat := range expected {
			if plat == workflow.PlatformLinux {
				t.Logf("job %q (linux) would be delegated to WSL on this Windows host", jobName)
			} else {
				t.Logf("job %q (%s) would NOT be delegated to WSL", jobName, plat)
			}
		}
	}
}
