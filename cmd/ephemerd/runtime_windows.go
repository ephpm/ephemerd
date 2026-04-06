//go:build windows

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// startContainerRuntime starts containerd in-process for Windows jobs.
// If Linux VM is enabled in config, boots a WSL2 Linux VM in the background
// so Windows jobs can start immediately while the Linux VM is provisioning.
//
// Returns the native containerd client. The Linux VM client becomes available
// asynchronously once the WSL distro is ready.
func startContainerRuntime(dataDir string, log *slog.Logger, linuxVMEnabled bool, _ uint32, configFile string) (*client.Client, func(), error) {
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

	// Boot the Linux VM in the background so we don't block Windows jobs.
	// The WSL import + binary copy can take a while.
	var linuxVM vm.LinuxVM
	linuxVMDone := make(chan struct{})

	go func() {
		defer close(linuxVMDone)
		log.Info("starting Linux VM in background (WSL)")

		lvm, err := vm.StartLinuxVM(vm.LinuxVMConfig{
			DataDir:    dataDir,
			ConfigFile: configFile,
			Log:        log,
		})
		if err != nil {
			log.Warn("Linux VM not started — Linux jobs will not be available on this host", "error", err)
			return
		}

		linuxVM = lvm
		linuxVMClient = lvm.Client()
		log.Info("Linux VM ready — this host can run both Windows and Linux jobs")
	}()

	cleanup = func() {
		// Wait for the background boot to finish before stopping
		<-linuxVMDone
		if linuxVM != nil {
			linuxVM.Stop()
		}
		ctrd.Stop()
	}

	return ctrd.Client(), cleanup, nil
}

// linuxVMClient holds the containerd client for the WSL2 Linux VM.
// Used by the scheduler to route Linux-labeled jobs to the VM's containerd.
// This is a temporary solution — should be properly wired through the runtime.
var linuxVMClient *client.Client
