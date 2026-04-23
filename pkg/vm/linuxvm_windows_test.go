//go:build windows

package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSchemaVersion_Missing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.schema")
	if got := readSchemaVersion(missing); got != 0 {
		t.Errorf("missing file: got %d, want 0", got)
	}
}

func TestReadSchemaVersion_Parses(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"plain integer", "2", 2},
		{"trailing newline", "2\n", 2},
		{"leading whitespace", "  3\n", 3},
		{"zero", "0", 0},
		{"large", "99", 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readSchemaVersion(path); got != tc.want {
				t.Errorf("content=%q: got %d, want %d", tc.content, got, tc.want)
			}
		})
	}
}

func TestReadSchemaVersion_Unparseable(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"v2",
		"two",
	}
	for _, content := range cases {
		path := filepath.Join(t.TempDir(), "schema")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readSchemaVersion(path); got != 0 {
			t.Errorf("content=%q: got %d, want 0 (unparseable)", content, got)
		}
	}
}

func TestRootVHDXSchemaVersion_IsCurrent(t *testing.T) {
	// Bump this sentinel when the on-disk layout changes and the init script
	// in mage/download/download.go is updated to match. The goal: a bump
	// forces the next daemon start to wipe old VHDXes.
	const expected = 2
	if rootVHDXSchemaVersion != expected {
		t.Errorf("rootVHDXSchemaVersion = %d, want %d — bump the sentinel here and in the init script if the disk layout changed", rootVHDXSchemaVersion, expected)
	}
}
