//go:build windows

package containerd

import (
	"strings"
	"testing"

	"github.com/Microsoft/go-winio"
)

// TestPipeSecurityDescriptorSDDL locks in the exact SDDL for the containerd
// control pipe and proves go-winio can parse it into a real security
// descriptor. A regression here (e.g. a widened ACE or a typo) would silently
// re-expose the SYSTEM-backed containerd control API to non-privileged local
// callers.
func TestPipeSecurityDescriptorSDDL(t *testing.T) {
	const want = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
	if pipeSecurityDescriptor != want {
		t.Fatalf("pipeSecurityDescriptor = %q, want %q", pipeSecurityDescriptor, want)
	}

	// Protected DACL: no inherited ACEs may weaken it.
	if !strings.HasPrefix(pipeSecurityDescriptor, "D:P") {
		t.Fatalf("DACL is not protected (missing D:P): %q", pipeSecurityDescriptor)
	}
	// Only SYSTEM (SY) and Administrators (BA) may be granted.
	for _, sid := range []string{";SY)", ";BA)"} {
		if !strings.Contains(pipeSecurityDescriptor, sid) {
			t.Fatalf("SDDL %q missing expected grant to %q", pipeSecurityDescriptor, sid)
		}
	}
	// No World/Everyone (WD) or Authenticated Users (AU) grant.
	for _, bad := range []string{";WD)", ";AU)"} {
		if strings.Contains(pipeSecurityDescriptor, bad) {
			t.Fatalf("SDDL %q must not grant %q", pipeSecurityDescriptor, bad)
		}
	}

	// The SDDL must be convertible to a Windows security descriptor; an invalid
	// string would fail here (and would fail winio.ListenPipe at runtime).
	if _, err := winio.SddlToSecurityDescriptor(pipeSecurityDescriptor); err != nil {
		t.Fatalf("SddlToSecurityDescriptor(%q): %v", pipeSecurityDescriptor, err)
	}
}
