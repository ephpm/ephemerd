//go:build windows

package forgerunner

import "os/exec"

// defaultShell returns the default shell for Windows.
// Prefers pwsh (PowerShell 7+), falls back to powershell, then cmd.
func defaultShell() (bin string, args []string, ext string) {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, ".ps1"
	}
	if _, err := exec.LookPath("powershell"); err == nil {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, ".ps1"
	}
	return "cmd", []string{"/D", "/E:ON", "/V:OFF", "/S", "/C", "call"}, ".cmd"
}
