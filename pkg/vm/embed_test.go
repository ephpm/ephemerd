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

func TestFindEmbedded_SkipsPlaceholder(t *testing.T) {
	// The embed dir contains ephemerd-rootfs-placeholder.tar.gz (0 bytes).
	// findEmbedded should skip it and either find the real rootfs or return
	// an error — never return the placeholder file as the matched name.
	name, err := findEmbedded("ephemerd-rootfs-")
	if err != nil {
		// No real rootfs embedded — that's fine. The error message itself
		// may legitimately mention "placeholder" as informational text
		// (e.g. "only the placeholder rootfs is embedded").
		return
	}
	if strings.Contains(name, "placeholder") {
		t.Errorf("findEmbedded returned placeholder file: %s", name)
	}
}

func TestValidateEmbeddedAsset_Empty(t *testing.T) {
	err := validateEmbeddedAsset("rootfs", nil, true)
	if err == nil {
		t.Fatal("expected error for empty data")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty, got: %s", err)
	}
	if !strings.Contains(err.Error(), "mage windows") {
		t.Errorf("error should suggest 'mage windows', got: %s", err)
	}
}

func TestValidateEmbeddedAsset_BadMagic(t *testing.T) {
	err := validateEmbeddedAsset("rootfs", []byte("not a gzip file"), true)
	if err == nil {
		t.Fatal("expected error for non-gzip data")
	}
	if !strings.Contains(err.Error(), "not a valid gzip") {
		t.Errorf("error should mention gzip, got: %s", err)
	}
}

func TestValidateEmbeddedAsset_ValidGzip(t *testing.T) {
	// 0x1f 0x8b is the gzip magic number
	data := []byte{0x1f, 0x8b, 0x08, 0x00}
	if err := validateEmbeddedAsset("rootfs", data, true); err != nil {
		t.Errorf("valid gzip header should pass: %v", err)
	}
}

func TestValidateEmbeddedAsset_NoGzipCheck(t *testing.T) {
	// When expectGzip is false, any non-empty data should pass
	data := []byte("ELF binary data here")
	if err := validateEmbeddedAsset("ephemerd-linux", data, false); err != nil {
		t.Errorf("non-gzip asset should pass without gzip check: %v", err)
	}
}
