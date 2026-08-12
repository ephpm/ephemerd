//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func postInstallBinary(_ string) error { return nil }

func installService(binPath, dataDir string) error {
	// Create the Windows service using sc.exe
	args := []string{
		"create", "ephemerd",
		"binPath=", fmt.Sprintf(`"%s" serve --data-dir "%s"`, binPath, dataDir),
		"start=", "delayed-auto",
		"DisplayName=", "ephemerd - Ephemeral GitHub Actions Runner",
	}

	out, err := exec.Command("sc.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc.exe create: %s", string(out))
	}
	fmt.Println("  service: ephemerd (Windows service)")

	// Set description
	out, err = exec.Command("sc.exe", "description", "ephemerd",
		"Ephemeral GitHub Actions runner daemon. Provisions isolated containers for each CI job.").CombinedOutput()
	if err != nil {
		fmt.Printf("  warning: could not set service description: %s\n", string(out))
	}

	// Set recovery: restart on failure, backing off 5s → 30s → 60s. The SCM
	// repeats the LAST action for every subsequent failure until the reset
	// window elapses, so this retries forever at one minute rather than
	// giving up after three tries.
	out, err = exec.Command("sc.exe", "failure", "ephemerd",
		"reset=", "86400", "actions=", "restart/5000/restart/30000/restart/60000").CombinedOutput()
	if err != nil {
		fmt.Printf("  warning: could not set recovery options: %s\n", string(out))
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
		fmt.Printf("  warning: could not set recovery failure flag: %s\n", string(out))
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

	return nil
}

func printNextSteps(dataDir string) {
	fmt.Println("  Next steps:")
	fmt.Printf("    1. Edit %s\\config.toml (set github.owner)\n", dataDir)
	fmt.Println("    2. Set GITHUB_TOKEN as a system environment variable")
	fmt.Println("       or edit the service to include it")
	fmt.Println("    3. sc.exe start ephemerd")
}
