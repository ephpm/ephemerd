//go:build windows

package runtime

import "golang.org/x/sys/windows"

// defaultImage picks a Windows Server Core image that matches the host OS.
// Windows containers require the container OS version to match the host kernel.
func defaultImage() string {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	tag := buildToTag(major, minor, build)
	return "mcr.microsoft.com/windows/servercore:" + tag
}

// buildToTag maps Windows version numbers to the corresponding Server Core LTSC tag.
func buildToTag(major, minor, build uint32) string {
	switch {
	case build >= 26100:
		return "ltsc2025"
	case build >= 20348:
		return "ltsc2022"
	case build >= 17763:
		return "ltsc2019"
	default:
		return "ltsc2025" // best guess for unknown future builds
	}
}
