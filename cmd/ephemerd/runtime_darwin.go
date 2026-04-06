//go:build darwin

package main

import (
	"log/slog"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// startContainerRuntime boots a Linux VM via Virtualization.framework on macOS
// and returns a containerd client connected to containerd inside the VM.
// Linux jobs run as containers inside this VM.
func startContainerRuntime(dataDir string, log *slog.Logger, _ bool) (*client.Client, func(), error) {
	log.Info("macOS detected — booting Linux VM for container runtime")

	linuxVM, err := vm.StartLinuxVM(vm.LinuxVMConfig{
		DataDir: dataDir,
		Log:     log,
	})
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() { linuxVM.Stop() }
	return linuxVM.Client(), cleanup, nil
}
