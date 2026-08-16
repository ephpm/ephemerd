package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadRunnerLinuxTOML writes a minimal valid config carrying the given
// [runner.linux] block (and optional extra sections) and runs it through
// Load, returning the validation error (if any).
func loadRunnerLinuxTOML(t *testing.T, linuxBlock, extra string) (*Config, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[github]\nowner = \"testorg\"\n\n" + extra + "\n[runner.linux]\n" + linuxBlock
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// The default must stay runc: Kata is opt-in for this release, and a
// config that never mentions the key must behave exactly as before.
func TestLinuxRunner_DefaultsToRunc(t *testing.T) {
	var l LinuxRunnerToml
	if got := l.ResolvedRuntime(); got != LinuxRuntimeRunc {
		t.Errorf("ResolvedRuntime() = %q, want %q", got, LinuxRuntimeRunc)
	}
	if got := l.ContainerdRuntime(); got != "io.containerd.runc.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.runc.v2", got)
	}
}

// An absent [runner.linux] table must resolve to runc through a real Load,
// not just on a zero struct.
func TestLoad_RunnerLinuxAbsent_DefaultsToRunc(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[github]\nowner = \"testorg\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Runner.Linux.ContainerdRuntime(); got != "io.containerd.runc.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.runc.v2", got)
	}
}

// A whitespace-only value must mean "unset", matching how the rest of the
// config treats blank strings.
func TestLinuxRunner_BlankIsUnset(t *testing.T) {
	l := LinuxRunnerToml{Runtime: "   "}
	if got := l.ResolvedRuntime(); got != LinuxRuntimeRunc {
		t.Errorf("ResolvedRuntime() = %q, want %q", got, LinuxRuntimeRunc)
	}
}

func TestLoad_RunnerLinuxKata_BindsAndMapsToShim(t *testing.T) {
	cfg, err := loadRunnerLinuxTOML(t, "runtime = \"kata\"\n", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.Linux.Runtime != "kata" {
		t.Errorf("Runner.Linux.Runtime = %q, want kata", cfg.Runner.Linux.Runtime)
	}
	if got := cfg.Runner.Linux.ContainerdRuntime(); got != "io.containerd.kata.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.kata.v2", got)
	}
}

// An unknown runtime must be rejected at load with an error naming the key
// and listing the supported values — a typo must not silently fall back to
// runc, because that would quietly drop the isolation the operator asked
// for.
func TestLoad_RunnerLinuxUnknownRuntime_Rejected(t *testing.T) {
	_, err := loadRunnerLinuxTOML(t, "runtime = \"gvisor\"\n", "")
	if err == nil {
		t.Fatal("expected error: an unrecognised runner.linux.runtime must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{"runner.linux.runtime", "gvisor", "runc", "kata"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// Kata + dind must load. It was rejected while dind could only hand over a
// bind-mounted unix socket, which a guest with its own kernel cannot connect
// to; VM-isolated jobs now get the DOCKER_HOST=tcp:// transport instead, so
// refusing the combination would block a configuration that works.
func TestLoad_RunnerLinuxKataWithDind_OK(t *testing.T) {
	cfg, err := loadRunnerLinuxTOML(t, "runtime = \"kata\"\n", "[dind]\nenabled = true\n")
	if err != nil {
		t.Fatalf("Load with kata + dind: %v", err)
	}
	if !cfg.Dind.Enabled {
		t.Error("Dind.Enabled = false, want true")
	}
	if got := cfg.Runner.Linux.ContainerdRuntime(); got != "io.containerd.kata.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.kata.v2", got)
	}
}

// Kata with dind explicitly disabled is the supported opt-in path.
func TestLoad_RunnerLinuxKataWithoutDind_OK(t *testing.T) {
	cfg, err := loadRunnerLinuxTOML(t, "runtime = \"kata\"\n", "[dind]\nenabled = false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Runner.Linux.ContainerdRuntime(); got != "io.containerd.kata.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.kata.v2", got)
	}
}

// dind with the default (runc) runtime must keep working untouched — the
// new validation must not regress the common configuration.
func TestLoad_DindWithDefaultRuntime_OK(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_test123")
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[github]\nowner = \"testorg\"\n\n[dind]\nenabled = true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with dind and default runtime: %v", err)
	}
	if !cfg.Dind.Enabled {
		t.Error("Dind.Enabled = false, want true")
	}
	if got := cfg.Runner.Linux.ContainerdRuntime(); got != "io.containerd.runc.v2" {
		t.Errorf("ContainerdRuntime() = %q, want io.containerd.runc.v2", got)
	}
}

// Explicit runtime = "runc" alongside dind must also be accepted.
func TestLoad_ExplicitRuncWithDind_OK(t *testing.T) {
	if _, err := loadRunnerLinuxTOML(t, "runtime = \"runc\"\n", "[dind]\nenabled = true\n"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
