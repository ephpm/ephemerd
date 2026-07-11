//go:build windows

package containerd

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// pipeSecurityDescriptor restricts who may open the containerd control pipe
// (\\.\pipe\ephemerd-containerd). This pipe exposes the full, unauthenticated
// containerd control API (create/run/exec any container, mount any host path)
// backed by a SYSTEM-privileged process, so it must not be openable by ordinary
// local users. The SDDL below is a protected DACL granting GENERIC_ALL to only
// NT AUTHORITY\SYSTEM (SY) and BUILTIN\Administrators (BA):
//
//	D:P               DACL, Protected (no inherited ACEs)
//	(A;;GA;;;SY)      Allow GENERIC_ALL to SYSTEM
//	(A;;GA;;;BA)      Allow GENERIC_ALL to Administrators
//
// This mirrors the HvSocket default bind SD already used for the Linux VM in
// pkg/vm/linuxvm_windows.go, and replaces the previous nil PipeConfig which let
// go-winio apply its (historically permissive, version-dependent) default SD.
const pipeSecurityDescriptor = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

func listen(address string) (net.Listener, error) {
	return winio.ListenPipe(address, &winio.PipeConfig{
		SecurityDescriptor: pipeSecurityDescriptor,
	})
}
