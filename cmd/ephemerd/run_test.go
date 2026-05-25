package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ephpm/ephemerd/pkg/workflow"
)

// configDirGuard saves and restores the package-level configDir global so
// each test case can point at its own tempdir without leaking state.
func configDirGuard(t *testing.T) func() {
	t.Helper()
	prev := configDir
	return func() { configDir = prev }
}

// writeConfig drops a minimal valid config.toml into dir and returns the
// path. Tests pass it via the configDir global, the same way the live
// CLI command does.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing config.toml: %v", err)
	}
}

func TestResolveRunImage_FlagWins(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	if got := resolveRunImage("ghcr.io/explicit:v1", workflow.PlatformLinux); got != "ghcr.io/explicit:v1" {
		t.Errorf("flag-wins: got %q, want explicit override", got)
	}
}

func TestResolveRunImage_ConfigWins_Linux(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	got := resolveRunImage("", workflow.PlatformLinux)
	if got != "ghcr.io/from-config:linux" {
		t.Errorf("config-wins linux: got %q, want %q", got, "ghcr.io/from-config:linux")
	}
}

func TestResolveRunImage_ConfigWins_Windows(t *testing.T) {
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
default_image_linux = "ghcr.io/from-config:linux"
default_image_windows = "ghcr.io/from-config:windows"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	got := resolveRunImage("", workflow.PlatformWindows)
	if got != "ghcr.io/from-config:windows" {
		t.Errorf("config-wins windows: got %q, want %q", got, "ghcr.io/from-config:windows")
	}
}

func TestResolveRunImage_NoConfigFile(t *testing.T) {
	// Empty data dir → config.Load returns ENOENT → resolver returns "" so
	// the downstream RunJob can apply the built-in default.
	defer configDirGuard(t)()
	configDir = t.TempDir()

	if got := resolveRunImage("", workflow.PlatformLinux); got != "" {
		t.Errorf("no-config: got %q, want empty (caller defaults)", got)
	}
}

func TestResolveRunImage_ConfigParseError(t *testing.T) {
	// Malformed TOML should not panic and must fall through to "" so the
	// caller can default. Today this happens via the swallowed error in
	// the config.Load call site — guard against a refactor that surfaces
	// the error and crashes the resolver.
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, "this is not valid TOML [\n")
	configDir = dir

	if got := resolveRunImage("", workflow.PlatformLinux); got != "" {
		t.Errorf("config-parse-error: got %q, want empty fallback", got)
	}
}

func TestResolveRunImage_ConfigWithoutImageOverride(t *testing.T) {
	// A config.toml that exists but doesn't set a default image for this
	// platform must fall through to "" so the built-in default applies.
	defer configDirGuard(t)()
	dir := t.TempDir()
	writeConfig(t, dir, `
[github]
owner = "testorg"
`)
	configDir = dir
	t.Setenv("GITHUB_TOKEN", "ghp_test")

	// Windows has no built-in default image (the runtime picks one from
	// the host build number), so DefaultImageFor("windows") returns "" —
	// resolver must propagate the empty string.
	if got := resolveRunImage("", workflow.PlatformWindows); got != "" {
		t.Errorf("no-windows-override: got %q, want empty (caller defaults)", got)
	}
}
