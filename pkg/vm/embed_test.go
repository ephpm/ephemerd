//go:build windows

package vm

import (
	"strings"
	"testing"
)

func TestFindEmbedded_RootfsPrefix(t *testing.T) {
	name, err := findEmbedded("ephemerd-rootfs-")
	if err != nil {
		t.Skipf("skipping: %v (rootfs not embedded)", err)
	}
	if !strings.HasPrefix(name, "embed/ephemerd-rootfs-") {
		t.Errorf("findEmbedded(rootfs) = %q, expected embed/ephemerd-rootfs- prefix", name)
	}
}

func TestFindEmbedded_NoMatch(t *testing.T) {
	_, err := findEmbedded("nonexistent-prefix-")
	if err == nil {
		t.Error("expected error for nonexistent prefix")
	}
}
