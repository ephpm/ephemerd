//go:build windows

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// startContainerRuntime starts containerd in-process for Windows jobs.
// If Linux VM is enabled in config, also boots a Hyper-V Linux VM for Linux jobs.
//
// Returns the native containerd client. The Linux VM client (if any) is
// stored for the scheduler to use when routing Linux-labeled jobs.
func startContainerRuntime(dataDir string, log *slog.Logger, linuxVMEnabled bool) (*client.Client, func(), error) {
	// Start native containerd for Windows container jobs
	ctrd, err := containerd.New(containerd.Config{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() { ctrd.Stop() }

	if !linuxVMEnabled {
		return ctrd.Client(), cleanup, nil
	}

	// Also try to start a Linux VM for cross-OS Linux jobs.
	// This is optional — if it fails, Windows jobs still work.
	linuxVM, err := vm.StartLinuxVM(vm.LinuxVMConfig{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		log.Warn("Linux VM not started — Linux jobs will not be available on this host", "error", err)
		return ctrd.Client(), cleanup, nil
	}

	log.Info("Linux VM ready — this host can run both Windows and Linux jobs")

	// Store the Linux VM client globally so the scheduler can route to it.
	// TODO: properly wire this through the runtime instead of a global.
	linuxVMClient = linuxVM.Client()

	cleanup = func() {
		linuxVM.Stop()
		ctrd.Stop()
	}

	return ctrd.Client(), cleanup, nil
}

// linuxVMClient holds the containerd client for the Hyper-V Linux VM.
// Used by the scheduler to route Linux-labeled jobs to the VM's containerd.
// This is a temporary solution — should be properly wired through the runtime.
var linuxVMClient *client.Client
