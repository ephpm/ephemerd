//go:build !windows

package forgerunner

import "os/exec"

// defaultShell returns the default shell for Unix platforms.
// Prefers bash, falls back to sh.
func defaultShell() (bin string, args []string, ext string) {
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash", []string{"--noprofile", "--norc", "-eo", "pipefail"}, ".sh"
	}
	return "sh", []string{"-e"}, ".sh"
}
