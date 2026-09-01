//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func postInstallBinary(_ string) error { return nil }

// errorServiceExists is ERROR_SERVICE_EXISTS (Win32 error 1073), which sc.exe
// both returns as its exit code and prints in its output when `sc create` is
// asked to create a service that is already installed.
const errorServiceExists = 1073

// serviceDependencies is the set of Windows services that must be RUNNING
// before the SCM starts ephemerd, in sc.exe `depend=` syntax (forward-slash
// separated). serve() needs all of them in its first seconds:
//
//   - hns:       Host Network Service — pkg/networking creates/adopts HCN
//     networks and per-endpoint ACLs via hcsshim/hcn before dispatching
//     anything.
//   - vmcompute: Hyper-V Host Compute Service — Windows job containers and
//     the Linux VM sidecar (pkg/vm) are HCS compute systems.
//   - Tcpip:     the TCP/IP stack under both of the above and the daemon's
//     own listeners.
//
// ephemerd only runs on Hyper-V-enabled CI nodes, so hns and vmcompute are
// assumed to exist; on a machine without them the SCM fails the start with a
// missing-dependency error, which is louder and more accurate than the daemon
// crash-looping against an absent HNS.
const serviceDependencies = "hns/vmcompute/Tcpip"

// serviceBinPath composes the service ImagePath. This exact composition is
// load-bearing: in-place upgrades (pkg/upgrade/swap_windows.go) rename the new
// binary into this canonical path and deliberately never touch the service
// definition, so whatever string is registered here is what the SCM launches
// forever after.
func serviceBinPath(binPath, dataDir string) string {
	return fmt.Sprintf(`"%s" serve --data-dir "%s"`, binPath, dataDir)
}

// createServiceArgs is the sc.exe argument list that registers the service
// fresh.
func createServiceArgs(binPath, dataDir string) []string {
	return []string{
		"create", "ephemerd",
		"binPath=", serviceBinPath(binPath, dataDir),
		"start=", "delayed-auto",
		"depend=", serviceDependencies,
		"DisplayName=", "ephemerd - Ephemeral GitHub Actions Runner",
	}
}

// configServiceArgs is the sc.exe argument list that converges an EXISTING
// service onto the same definition createServiceArgs would have written:
// same binPath, start type, and dependencies.
func configServiceArgs(binPath, dataDir string) []string {
	return []string{
		"config", "ephemerd",
		"binPath=", serviceBinPath(binPath, dataDir),
		"start=", "delayed-auto",
		"depend=", serviceDependencies,
	}
}

// isServiceExists reports whether an sc.exe create failure means the service
// is already installed (ERROR_SERVICE_EXISTS). Checked two ways because
// either signal alone is fragile: the exit code survives a localized Windows
// that prints the message in another language, and the output text covers any
// wrapper that lost the *exec.ExitError.
func isServiceExists(err error, out string) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == errorServiceExists {
		return true
	}
	return strings.Contains(out, fmt.Sprintf("FAILED %d", errorServiceExists)) ||
		strings.Contains(strings.ToLower(out), "already exists")
}

func installService(binPath, dataDir string) error {
	// Create the Windows service using sc.exe.
	out, err := exec.Command("sc.exe", createServiceArgs(binPath, dataDir)...).CombinedOutput()
	switch {
	case err == nil:
		fmt.Println("  service: ephemerd (Windows service)")
	case isServiceExists(err, string(out)):
		// The service already exists — a re-run of `ephemerd install`, or a
		// node originally set up by an older ephemerd. `sc.exe create`
		// refuses to touch an existing service, and returning here was
		// exactly the bug that left long-lived nodes without the recovery
		// ladder below: in-place upgrades never rewrite the service
		// definition (pkg/upgrade/swap_windows.go), so a node kept whatever
		// its ORIGINAL installer wrote — SCM defaults, no restart-on-failure,
		// silently dead after the first bad boot. Converge the existing
		// definition with `sc.exe config` and fall through so the recovery
		// settings are ALWAYS (re)applied.
		out, err = exec.Command("sc.exe", configServiceArgs(binPath, dataDir)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sc.exe config (service exists, updating definition): %s", string(out))
		}
		fmt.Println("  service: ephemerd (already installed, definition updated)")
	default:
		return fmt.Errorf("sc.exe create: %s", string(out))
	}

	// Set description. Cosmetic — a failure here is worth a note, not a
	// failed install.
	out, err = exec.Command("sc.exe", "description", "ephemerd",
		"Ephemeral GitHub Actions runner daemon. Provisions isolated containers for each CI job.").CombinedOutput()
	if err != nil {
		fmt.Printf("  warning: could not set service description: %s\n", string(out))
	}

	// The recovery settings below are the self-healing the fleet depends on;
	// failing to apply them is a real error, not a cosmetic warning. Collect
	// them so the caller reports exactly what did not stick.
	var errs []error

	// Set recovery: restart on failure, backing off 5s → 30s → 60s. The SCM
	// repeats the LAST action for every subsequent failure until the reset
	// window elapses, so this retries forever at one minute rather than
	// giving up after three tries.
	out, err = exec.Command("sc.exe", "failure", "ephemerd",
		"reset=", "86400", "actions=", "restart/5000/restart/30000/restart/60000").CombinedOutput()
	if err != nil {
		errs = append(errs, fmt.Errorf("setting recovery actions (sc.exe failure): %s", string(out)))
	}

	// Apply those recovery actions to CLEAN stops that report a non-zero exit
	// code too, not only to a process that dies without telling the SCM.
	//
	// This is why the node came back from a hard reset with the service
	// Stopped despite start=delayed-auto: when serve() fails at boot (the
	// Hyper-V/containerd stack is commonly not ready yet on a cold start) the
	// service handler reports SERVICE_STOPPED with exit code 1, which the SCM
	// treats by default as a deliberate stop and does not recover from. With
	// the flag set, that same exit re-triggers the restart ladder above and
	// the node heals itself instead of sitting idle until someone notices.
	out, err = exec.Command("sc.exe", "failureflag", "ephemerd", "1").CombinedOutput()
	if err != nil {
		errs = append(errs, fmt.Errorf("setting recovery failure flag (sc.exe failureflag): %s", string(out)))
	}

	// Create env file equivalent — Windows uses the system environment
	// or a wrapper script. Print instructions instead.
	envFile := fmt.Sprintf(`%s\env.cmd`, dataDir)
	if _, statErr := os.Stat(envFile); statErr != nil {
		envContent := "@echo off\r\nrem Set your GitHub token here\r\nrem set GITHUB_TOKEN=ghp_your_token_here\r\n"
		if writeErr := os.WriteFile(envFile, []byte(envContent), 0o644); writeErr != nil {
			fmt.Printf("  warning: could not create %s: %v\n", envFile, writeErr)
		} else {
			fmt.Printf("  env:     %s\n", envFile)
		}
	}

	return errors.Join(errs...)
}

func printNextSteps(dataDir string) {
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Edit %s\\config.toml (set github.owner)\n", dataDir)
	fmt.Println("    2. Set GITHUB_TOKEN as a system environment variable")
	fmt.Println("       or edit the service to include it")
	fmt.Println("    3. sc.exe start ephemerd")
}
