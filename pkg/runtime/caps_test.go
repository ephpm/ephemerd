package runtime

import (
	"strings"
	"testing"
)

func TestContainerCapabilities_NotEmpty(t *testing.T) {
	if len(containerCapabilities) == 0 {
		t.Fatal("containerCapabilities should not be empty")
	}
}

func TestContainerCapabilities_AllStartWithCAP(t *testing.T) {
	for _, cap := range containerCapabilities {
		if !strings.HasPrefix(cap, "CAP_") {
			t.Errorf("capability %q does not start with CAP_", cap)
		}
	}
}

func TestContainerCapabilities_NoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(containerCapabilities))
	for _, cap := range containerCapabilities {
		if seen[cap] {
			t.Errorf("duplicate capability: %q", cap)
		}
		seen[cap] = true
	}
}

// The required-capability assertion lives in hardening_test.go
// (TestContainerCapabilities_KeepsRequired), next to the dangerous-capability
// list it is the mirror image of. Keeping a second copy here let the two drift.

func TestContainerCapabilities_NoEmptyStrings(t *testing.T) {
	for i, cap := range containerCapabilities {
		if cap == "" {
			t.Errorf("containerCapabilities[%d] is empty", i)
		}
		if strings.TrimSpace(cap) != cap {
			t.Errorf("containerCapabilities[%d] = %q has leading/trailing whitespace", i, cap)
		}
	}
}
