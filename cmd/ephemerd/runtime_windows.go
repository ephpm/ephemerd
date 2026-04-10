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
// running the full ephemerd stack (scheduler + runner + CNI) so it can
// independently handle Linux-labeled GitHub jobs.
//
// Returns the native containerd client for Windows jobs.
func startContainerRuntime(dataDir string, log *slog.Logger, linuxVMEnabled bool, _ uint32, configFile string, privateKeyPath string) (*client.Client, func(), error) {
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
			DataDir:        dataDir,
			ConfigFile:     configFile,
			PrivateKeyPath: privateKeyPath,
			Log:            log,
		})
		if err != nil {
			log.Warn("Linux VM not started — Linux jobs will not be available on this host", "error", err)
			return
		}

		linuxVM = lvm
		log.Info("Linux VM ready — WSL ephemerd handles Linux jobs independently")
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
