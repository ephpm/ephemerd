//go:build windows

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// startContainerRuntime starts containerd in-process for Windows jobs.
// If Linux VM is enabled in config, boots a WSL2 Linux VM in the background
// running containerd-only + dispatch worker for Linux jobs.
//
// Returns the native containerd client for Windows jobs and a function that
// blocks until the Linux dispatch client is ready (nil if Linux VM is disabled
// or failed to start).
func startContainerRuntime(dataDir string, log *slog.Logger, linuxVMEnabled bool, _ uint32) (*client.Client, func() *scheduler.DispatchClient, func(), error) {
	// Start native containerd for Windows container jobs
	ctrd, err := containerd.New(containerd.Config{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	cleanup := func() { ctrd.Stop() }

	if !linuxVMEnabled {
		return ctrd.Client(), func() *scheduler.DispatchClient { return nil }, cleanup, nil
	}

	// Boot the Linux VM in the background so we don't block Windows jobs.
	// The WSL import + binary copy can take a while.
	var linuxVM vm.LinuxVM
	var dispatchClient *scheduler.DispatchClient
	linuxVMDone := make(chan struct{})

	go func() {
		defer close(linuxVMDone)
		log.Info("starting Linux VM in background (WSL)")

		lvm, err := vm.StartLinuxVM(vm.LinuxVMConfig{
			DataDir: dataDir,
			Log:     log,
		})
		if err != nil {
			log.Warn("Linux VM not started — Linux jobs will not be available on this host", "error", err)
			return
		}

		linuxVM = lvm

		if addr := lvm.DispatchAddr(); addr != "" {
			dc, err := scheduler.NewDispatchClient(addr)
			if err != nil {
				log.Warn("failed to connect dispatch client", "address", addr, "error", err)
			} else {
				dispatchClient = dc
				log.Info("Linux dispatch client ready", "address", addr)
			}
		}

		log.Info("Linux VM ready — Linux jobs dispatched via gRPC")
	}()

	// waitDispatch blocks until the VM boot completes and returns the dispatch client.
	waitDispatch := func() *scheduler.DispatchClient {
		<-linuxVMDone
		return dispatchClient
	}

	cleanup = func() {
		<-linuxVMDone
		if dispatchClient != nil {
			_ = dispatchClient.Close()
		}
		if linuxVM != nil {
			linuxVM.Stop()
		}
		ctrd.Stop()
	}

	return ctrd.Client(), waitDispatch, cleanup, nil
}
