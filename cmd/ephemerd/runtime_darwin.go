//go:build darwin

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// startContainerRuntime boots a Linux VM via Virtualization.framework on macOS
// and returns a containerd client connected to containerd inside the VM, plus
// a dispatch client to ephemerd-linux running inside the VM. Linux jobs run as
// containers inside the VM, dispatched through the gRPC dispatch server so
// they get full CNI networking (the raw containerd API skips CRI/CNI).
func startContainerRuntime(dataDir string, log *slog.Logger, _ bool, _ uint32, _ string, _ bool) (*client.Client, func() (*scheduler.DispatchClient, *client.Client), func(), error) {
	log.Info("macOS detected — booting Linux VM for container runtime")

	linuxVM, err := vm.StartLinuxVM(vm.LinuxVMConfig{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	var dispatchClient *scheduler.DispatchClient
	if addr := linuxVM.DispatchAddr(); addr != "" {
		dc, derr := scheduler.NewDispatchClient(addr)
		if derr != nil {
			log.Warn("dispatch client failed — Linux jobs will not have CNI networking",
				"address", addr, "error", derr)
		} else {
			dispatchClient = dc
			log.Info("Linux dispatch client ready", "address", addr)
		}
	}

	cleanup := func() {
		if dispatchClient != nil {
			_ = dispatchClient.Close()
		}
		linuxVM.Stop()
	}
	return linuxVM.Client(), func() (*scheduler.DispatchClient, *client.Client) { return dispatchClient, linuxVM.Client() }, cleanup, nil
}
