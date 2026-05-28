//go:build darwin

package vm

import (
	"testing"
)

func TestDarwinMacOSVM_SetArtifactsDir(t *testing.T) {
	// Use a zero-value struct — no disk image needed for this test.
	vm := &darwinMacOSVM{
		done: make(chan struct{}),
	}

	// Initially empty
	if vm.artifactsDir != "" {
		t.Errorf("initial artifactsDir = %q, want empty", vm.artifactsDir)
	}

	// Set a directory
	vm.SetArtifactsDir("/tmp/test-artifacts")
	if vm.artifactsDir != "/tmp/test-artifacts" {
		t.Errorf("artifactsDir = %q, want /tmp/test-artifacts", vm.artifactsDir)
	}

	// Set to empty (clear)
	vm.SetArtifactsDir("")
	if vm.artifactsDir != "" {
		t.Errorf("after clear, artifactsDir = %q, want empty", vm.artifactsDir)
	}
}

func TestDarwinMacOSVM_SetArtifactsDir_SatisfiesInterface(t *testing.T) {
	// Compile-time check that darwinMacOSVM implements MacOSVM.
	var _ MacOSVM = (*darwinMacOSVM)(nil)
}
