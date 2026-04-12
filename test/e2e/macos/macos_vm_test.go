//go:build e2e && darwin

package macos

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ephpm/ephemerd/pkg/vm"
)

// testLog returns a shared logger for macOS e2e tests.
func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// baseImagePath returns the macOS base image path from EPHEMERD_MACOS_BASE_IMAGE
// or skips the test if unset.
func baseImagePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("EPHEMERD_MACOS_BASE_IMAGE")
	if path == "" {
		t.Skip("EPHEMERD_MACOS_BASE_IMAGE not set — skipping macOS VM test")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("base image not found at %s", path)
	}
	return path
}

// TestMacOSVM_Lifecycle boots a macOS VM, waits for SSH, and tears it down.
// Requires a provisioned base image with SSH enabled.
//
// Run with:
//
//	EPHEMERD_MACOS_BASE_IMAGE=/path/to/macos.img go test -tags e2e -run TestMacOSVM_Lifecycle -v ./test/e2e/macos/
func TestMacOSVM_Lifecycle(t *testing.T) {
	baseImage := baseImagePath(t)
	log := testLog()
	dataDir := t.TempDir()

	cfg := vm.MacOSVMConfig{
		DataDir:   dataDir,
		BaseImage: baseImage,
		CPUs:      2,
		MemoryMB:  4096,
		Log:       log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	jobID := fmt.Sprintf("e2e-lifecycle-%d", time.Now().UnixNano())

	// Create VM
	macVM, err := vm.NewMacOSVM(cfg, jobID)
	if err != nil {
		t.Fatalf("NewMacOSVM: %v", err)
	}

	// Boot
	t.Log("starting macOS VM")
	if err := macVM.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the VM to become reachable
	t.Log("waiting for VM runner (SSH on port 22)")
	ip, err := macVM.WaitForRunner(ctx)
	if err != nil {
		macVM.Stop()
		t.Fatalf("WaitForRunner: %v", err)
	}
	t.Logf("macOS VM reachable at %s", ip)

	if ip == "" {
		macVM.Stop()
		t.Fatal("WaitForRunner returned empty IP")
	}

	// Verify RunnerAddress returns the same IP
	addr := macVM.RunnerAddress()
	if addr != ip {
		t.Errorf("RunnerAddress() = %q, want %q", addr, ip)
	}

	// Stop and verify cleanup
	t.Log("stopping macOS VM")
	macVM.Stop()

	// Verify the APFS clone was removed
	clonePath := filepath.Join(dataDir, "vm", "macos", "jobs", jobID+".img")
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Errorf("clone image not cleaned up: %s", clonePath)
	}

	// Verify the job directory was removed
	jobDir := filepath.Join(dataDir, "vm", "macos", "jobs", jobID)
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job directory not cleaned up: %s", jobDir)
	}

	t.Log("macOS VM lifecycle test passed")
}

// TestMacOSVM_JITConfig verifies that the JIT config file is written to the
// shared directory and the VM can boot with it present.
//
// Run with:
//
//	EPHEMERD_MACOS_BASE_IMAGE=/path/to/macos.img go test -tags e2e -run TestMacOSVM_JITConfig -v ./test/e2e/macos/
func TestMacOSVM_JITConfig(t *testing.T) {
	baseImage := baseImagePath(t)
	log := testLog()
	dataDir := t.TempDir()

	cfg := vm.MacOSVMConfig{
		DataDir:   dataDir,
		BaseImage: baseImage,
		CPUs:      2,
		MemoryMB:  4096,
		Log:       log,
	}

	jobID := fmt.Sprintf("e2e-jit-%d", time.Now().UnixNano())

	macVM, err := vm.NewMacOSVM(cfg, jobID)
	if err != nil {
		t.Fatalf("NewMacOSVM: %v", err)
	}

	// Write JIT config before boot
	fakeJIT := "eyJzY2hlbWUiOiAidGVzdCJ9" // base64 of {"scheme": "test"}
	if err := macVM.WriteJITConfig(fakeJIT); err != nil {
		t.Fatalf("WriteJITConfig: %v", err)
	}

	// Verify the file was written to the expected path
	jitPath := filepath.Join(dataDir, "vm", "macos", "jobs", jobID, ".jit_config")
	data, err := os.ReadFile(jitPath)
	if err != nil {
		t.Fatalf("reading JIT config: %v", err)
	}
	if string(data) != fakeJIT {
		t.Errorf("JIT config = %q, want %q", string(data), fakeJIT)
	}

	// Verify file permissions are restrictive
	info, err := os.Stat(jitPath)
	if err != nil {
		t.Fatalf("stat JIT config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("JIT config permissions = %o, want 0600", perm)
	}

	// Boot the VM with the JIT config present in the share
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Log("starting macOS VM with JIT config in shared directory")
	if err := macVM.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for SSH — the runner in the base image should read .jit_config
	// from the virtio-fs mount and start the GitHub Actions runner.
	ip, err := macVM.WaitForRunner(ctx)
	if err != nil {
		macVM.Stop()
		t.Fatalf("WaitForRunner: %v", err)
	}
	t.Logf("macOS VM with JIT config reachable at %s", ip)

	macVM.Stop()
	t.Log("macOS VM JIT config test passed")
}

// TestMacOSVM_ConcurrentVMs boots multiple macOS VMs in parallel to verify
// isolation — each gets its own APFS clone, job directory, and IP.
//
// Run with:
//
//	EPHEMERD_MACOS_BASE_IMAGE=/path/to/macos.img go test -tags e2e -run TestMacOSVM_ConcurrentVMs -v ./test/e2e/macos/
func TestMacOSVM_ConcurrentVMs(t *testing.T) {
	baseImage := baseImagePath(t)
	log := testLog()
	dataDir := t.TempDir()

	cfg := vm.MacOSVMConfig{
		DataDir:   dataDir,
		BaseImage: baseImage,
		CPUs:      2,
		MemoryMB:  4096,
		Log:       log,
	}

	const numVMs = 2
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	type vmResult struct {
		id  string
		ip  string
		vm  vm.MacOSVM
		err error
	}

	results := make(chan vmResult, numVMs)

	for i := range numVMs {
		jobID := fmt.Sprintf("e2e-concurrent-%d-%d", i, time.Now().UnixNano())
		go func(id string) {
			macVM, err := vm.NewMacOSVM(cfg, id)
			if err != nil {
				results <- vmResult{id: id, err: fmt.Errorf("NewMacOSVM: %w", err)}
				return
			}
			if err := macVM.Start(ctx); err != nil {
				results <- vmResult{id: id, err: fmt.Errorf("Start: %w", err)}
				return
			}
			ip, err := macVM.WaitForRunner(ctx)
			if err != nil {
				macVM.Stop()
				results <- vmResult{id: id, err: fmt.Errorf("WaitForRunner: %w", err)}
				return
			}
			results <- vmResult{id: id, ip: ip, vm: macVM}
		}(jobID)
	}

	// Collect results
	var vms []vmResult
	for range numVMs {
		r := <-results
		if r.err != nil {
			// Stop any VMs that did start
			for _, v := range vms {
				if v.vm != nil {
					v.vm.Stop()
				}
			}
			t.Fatalf("VM %s failed: %v", r.id, r.err)
		}
		vms = append(vms, r)
		t.Logf("VM %s ready at %s", r.id, r.ip)
	}

	// Verify all IPs are unique
	seen := make(map[string]string)
	for _, v := range vms {
		if prev, exists := seen[v.ip]; exists {
			t.Errorf("VMs %s and %s have the same IP %s", prev, v.id, v.ip)
		}
		seen[v.ip] = v.id
	}

	// Clean up all VMs
	for _, v := range vms {
		v.vm.Stop()
	}

	// Verify all clones cleaned up
	for _, v := range vms {
		clonePath := filepath.Join(dataDir, "vm", "macos", "jobs", v.id+".img")
		if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
			t.Errorf("VM %s clone not cleaned up: %s", v.id, clonePath)
		}
	}

	t.Logf("concurrent VM test passed with %d VMs", numVMs)
}

// TestMacOSVM_StopBeforeReady verifies that stopping a VM before it becomes
// reachable does not leak resources.
func TestMacOSVM_StopBeforeReady(t *testing.T) {
	baseImage := baseImagePath(t)
	log := testLog()
	dataDir := t.TempDir()

	cfg := vm.MacOSVMConfig{
		DataDir:   dataDir,
		BaseImage: baseImage,
		CPUs:      2,
		MemoryMB:  4096,
		Log:       log,
	}

	jobID := fmt.Sprintf("e2e-early-stop-%d", time.Now().UnixNano())

	macVM, err := vm.NewMacOSVM(cfg, jobID)
	if err != nil {
		t.Fatalf("NewMacOSVM: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := macVM.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Immediately stop — don't wait for runner
	t.Log("stopping VM immediately after boot")
	macVM.Stop()

	// Verify cleanup
	clonePath := filepath.Join(dataDir, "vm", "macos", "jobs", jobID+".img")
	if _, err := os.Stat(clonePath); !os.IsNotExist(err) {
		t.Errorf("clone not cleaned up: %s", clonePath)
	}
	jobDir := filepath.Join(dataDir, "vm", "macos", "jobs", jobID)
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job directory not cleaned up: %s", jobDir)
	}

	t.Log("early stop cleanup test passed")
}
