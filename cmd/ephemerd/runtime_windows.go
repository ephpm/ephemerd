//go:build windows

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/ephpm/ephemerd/pkg/containerd"
	"github.com/ephpm/ephemerd/pkg/scheduler"
	"github.com/ephpm/ephemerd/pkg/vm"
)

// Linux-sidecar start retry ladder.
//
// WHY IT EXISTS. vm.StartLinuxVM used to be a single shot: one error and the
// goroutine logged "Linux jobs will not be available on this host" and gave
// up for the daemon's entire uptime. The failures that actually happen on
// this fleet are transient cold-boot ones — vmcompute/HCS and the Hyper-V
// services are still coming up when ephemerd's service start races them
// after a host reboot — so a node that would have been fine 20 seconds later
// silently lost all of its Linux capacity, kept reporting healthy, and kept
// accepting Windows jobs. Nobody noticed until Linux jobs queued.
//
// PARAMS. 10 attempts × 6s ≈ 1 minute of tolerance, matching the shape of
// the networking.New ladder (30 × 2s) that #189 added for the same class of
// post-boot race but with a longer per-attempt delay: each StartLinuxVM
// attempt is itself expensive (WSL import + binary copy) and does its own
// blocking, so hammering it every 2s buys nothing. A minute comfortably
// covers vmcompute settling; anything longer is a real misconfiguration that
// retrying will not fix, and the give-up log is the signal for it.
const (
	linuxVMStartAttempts = 10
	linuxVMStartDelay    = 6 * time.Second
)

// startContainerRuntime starts containerd in-process for Windows jobs.
// If Linux VM is enabled in config, boots a WSL2 Linux VM in the background
// running containerd-only + dispatch worker for Linux jobs.
//
// Returns the native containerd client for Windows jobs and a function that
// blocks until the Linux dispatch client is ready (nil if Linux VM is disabled
// or failed to start).
func startContainerRuntime(dataDir string, log *slog.Logger, linuxVMEnabled bool, _ uint32, _ string, dindEnabled, dindAllowPrivileged bool, linuxVMCPUs uint, linuxVMMemoryMB uint64, linuxVMDiskSizeGB uint64, dispatchToken string) (*client.Client, func() (*scheduler.DispatchClient, *client.Client), func(), error) {
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
		return ctrd.Client(), func() (*scheduler.DispatchClient, *client.Client) { return nil, nil }, cleanup, nil
	}

	// dispatchToken was minted + persisted into config.toml by the caller
	// (EnsureDispatchToken) before we get here, so it also rides into the VM's
	// config and the in-VM dispatch server requires the identical value.

	// Boot the Linux VM in the background so we don't block Windows jobs.
	// The WSL import + binary copy can take a while.
	var linuxVM vm.LinuxVM
	var dispatchClient *scheduler.DispatchClient
	linuxVMDone := make(chan struct{})

	// The retry ladder needs a cancellable ctx and startContainerRuntime
	// takes none (the signature is shared with the Linux/macOS builds), so
	// own one here. cleanup cancels it before waiting on linuxVMDone —
	// otherwise a shutdown that lands mid-ladder would block the whole
	// daemon stop for the ladder's remaining budget.
	vmCtx, cancelVMStart := context.WithCancel(context.Background())

	go func() {
		defer close(linuxVMDone)
		log.Info("starting Linux VM in background (Hyper-V)")

		// Retrying StartLinuxVM is safe: it opens with cleanupStaleVMs (which
		// removes a previous attempt's distro) and every step after the boot
		// calls l.Stop() on its own way out, so a failed attempt does not
		// leave a half-built VM for the next one to trip over.
		lvm, err := retryInit(vmCtx, linuxVMStartAttempts, linuxVMStartDelay, log, "Linux VM", func() (vm.LinuxVM, error) {
			return vm.StartLinuxVM(vm.LinuxVMConfig{
				DataDir:             dataDir,
				CPUs:                linuxVMCPUs,
				MemoryMB:            linuxVMMemoryMB,
				DiskSizeGB:          linuxVMDiskSizeGB,
				DindEnabled:         dindEnabled,
				DindAllowPrivileged: dindAllowPrivileged,
				// Share the host's data dir read-only so the in-VM ephemerd
				// reads the same config.toml. See docs/arch/plan9-config-share.md.
				HostDataDir: dataDir,
				Log:         log,
			})
		})
		if err != nil {
			// Still fail-soft, deliberately: the Linux sidecar is extra
			// capacity, not a dependency of the Windows runtime that is
			// already up and serving. Exiting non-zero here would turn a
			// missing sidecar into a Windows-CI outage, which is strictly
			// worse than the queueing it replaces.
			log.Error("Linux VM not started after retries — Linux jobs will not be available on this host; Windows jobs are unaffected",
				"attempts", linuxVMStartAttempts, "error", err)
			return
		}

		linuxVM = lvm

		if addr := lvm.DispatchAddr(); addr != "" {
			dc, err := scheduler.NewDispatchClient(addr, dispatchToken)
			if err != nil {
				log.Warn("failed to connect dispatch client", "address", addr, "error", err)
			} else {
				dispatchClient = dc
				log.Info("Linux dispatch client ready", "address", addr)
			}
		}

		log.Info("Linux VM ready — Linux jobs dispatched via gRPC")
	}()

	// waitDispatch blocks until the VM boot completes and returns the dispatch
	// client and the VM's containerd client (for importing deferred images).
	waitDispatch := func() (*scheduler.DispatchClient, *client.Client) {
		<-linuxVMDone
		var vmClient *client.Client
		if linuxVM != nil {
			vmClient = linuxVM.Client()
		}
		return dispatchClient, vmClient
	}

	cleanup = func() {
		// Abort an in-progress start ladder first; see vmCtx above.
		cancelVMStart()
		<-linuxVMDone
		if dispatchClient != nil {
			if err := dispatchClient.Close(); err != nil {
				log.Warn("closing dispatch client", "error", err)
			}
		}
		if linuxVM != nil {
			linuxVM.Stop()
		}
		ctrd.Stop()
	}

	return ctrd.Client(), waitDispatch, cleanup, nil
}
