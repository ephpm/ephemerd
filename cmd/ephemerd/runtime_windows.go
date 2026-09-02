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
// WHERE IT RUNS. Entirely off the daemon's startup path. Nothing here blocks
// the scheduler, the webhook tunnel, or Windows jobs — see waitDispatch
// below and the bounded wait in serve(). It used to: waitDispatch was a bare
// `<-linuxVMDone`, called before scheduler.New, so the whole ladder ran with
// the daemon serving nothing at all.
//
// PARAMS, AND THE COST THEY ACTUALLY BUY. The obvious reading of "10 × 6s" is
// "about a minute", which is what the previous comment claimed. It is wrong
// by an order of magnitude, because it counts only the SLEEPS. One failing
// vm.StartLinuxVM attempt does its own long blocking waits before it returns
// an error (pkg/vm/linuxvm_windows.go: discoverIP 60s, waitForContainerd
// ~360s, the dispatch wait ~270s, Stop ~11s) — call it ten to eleven minutes
// worst case. Ten attempts is therefore closer to 1.5 HOURS than to a minute.
//
// So the ladder is bounded by WALL CLOCK, not by attempt count.
// linuxVMStartBudget is the real limit; the attempt/delay pair only shapes
// the retries inside it. The budget is sized to cover a couple of genuine
// post-reboot attempts (the transient vmcompute/Hyper-V race this exists for
// settles well inside one), after which a give-up log is the correct answer:
// a host that has failed to boot the sidecar for 25 minutes is misconfigured,
// and retrying a misconfiguration for another hour only hides it.
const (
	linuxVMStartAttempts = 10
	linuxVMStartDelay    = 6 * time.Second
	linuxVMStartBudget   = 25 * time.Minute
)

// linuxVMShutdownGrace bounds how long daemon shutdown will wait for the
// start ladder to notice it has been cancelled.
//
// vm.StartLinuxVM takes no context, so cancelling vmCtx cannot interrupt an
// attempt that is already inside it — retryInit only gets to look at the ctx
// when the attempt returns, which can be minutes away. Blocking shutdown on
// that is not an option: the Windows SCM hard-kills a service that has not
// stopped within 30 seconds, so "wait for the ladder" means "get killed
// mid-teardown, every time". We wait briefly for a clean exit and otherwise
// abandon the goroutine; it owns no state the rest of shutdown touches, and
// the process is going away regardless.
const linuxVMShutdownGrace = 3 * time.Second

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
	// own one here. The deadline is what actually bounds the ladder (see
	// linuxVMStartBudget); cleanup cancels it early on shutdown.
	//
	// cancelVMStart is now genuinely reachable while the ladder is running.
	// It was not before: the only caller is cleanup(), which main invokes on
	// its way out — and main was parked inside waitDispatch for the entire
	// ladder, so the cancel could not be reached until the thing it was
	// meant to cancel had already finished.
	vmCtx, cancelVMStart := context.WithTimeout(context.Background(), linuxVMStartBudget)

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
			log.Error("Linux VM not started within the start budget — Linux jobs will not be available on this host; Windows jobs are unaffected",
				"max_attempts", linuxVMStartAttempts, "budget", linuxVMStartBudget, "error", err)
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
	//
	// CALLERS MUST NOT PUT THIS ON A CRITICAL PATH. It can block for the
	// whole linuxVMStartBudget. serve() calls it from a goroutine with a
	// short bounded wait for the scheduler's initial dispatcher, and attaches
	// the dispatcher later (scheduler.SetLinuxDispatcher) if it arrives after
	// that. Reading linuxVM/dispatchClient only after <-linuxVMDone is what
	// makes those reads race-free.
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
		// Bounded: an attempt already inside vm.StartLinuxVM cannot be
		// interrupted, and the SCM will not wait. See linuxVMShutdownGrace.
		select {
		case <-linuxVMDone:
			if dispatchClient != nil {
				if err := dispatchClient.Close(); err != nil {
					log.Warn("closing dispatch client", "error", err)
				}
			}
			if linuxVM != nil {
				linuxVM.Stop()
			}
		case <-time.After(linuxVMShutdownGrace):
			// Deliberately do NOT touch dispatchClient/linuxVM here: the
			// start goroutine still owns them and reading them without the
			// linuxVMDone barrier would be a data race. A half-started VM
			// is cleaned up by the next daemon start (StartLinuxVM opens
			// with cleanupStaleVMs).
			log.Warn("Linux VM start ladder did not stop in time; abandoning it so shutdown can finish",
				"grace", linuxVMShutdownGrace)
		}
		ctrd.Stop()
	}

	return ctrd.Client(), waitDispatch, cleanup, nil
}
